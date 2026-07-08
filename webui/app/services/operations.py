"""Air-gap cache operations — the backend's own copy of the logic, so the control
UI is self-contained and never reaches into scripts/.

The workflows live on the `Operations` service; each is a *generator that yields
log lines* as it runs. Two callers share this one implementation:

  * the control UI (webui/jobs.py) holds an Operations instance, calls `build` and
    streams each yielded line into its job console, and
  * the operator CLI (scripts/pkgops.py) is a thin wrapper over the same service, so
    `python3 scripts/pkgops.py <action> …` runs the exact same code by hand on
    the air-gapped host.

Python does all the orchestration, validation and control flow; git/dvc/docker
are still the underlying CLIs (there is no Python-native equivalent for
`git bundle` or `docker compose --profile`), invoked via subprocess with NO
shell — so untrusted values (drive paths, commit hashes) are always separate
argv elements, never interpolated into a command line.
"""
import json
import os
import pathlib
import re
import shutil
import ssl
import subprocess
import urllib.error
import urllib.request

from app import settings
from app.errors import OpError
from app.gateways import proc
from app.gateways.pkgcache import git_maintain_url as _git_maintain_url
from app.gateways.proc import run
from app.services import projects
from app.services.lockwarm import IndexMap, LockParser, LockRewriter, Proxy, Warmer
from app.services.projects import GLOBAL
from app.urls import pypi_internal

ROOT = settings.ROOT

# The cache state is versioned in its OWN git + DVC repo. The GLOBAL project's repo
# is caches/ (unchanged); each named project gets caches/projects/<name>/, its own
# git+DVC repo, so a per-project checkpoint / rollback / shuttle only ever touches
# that one project's state. All git/dvc calls below run with cwd=_repo(project);
# only the manifest regen and `docker compose` run from ROOT.
CACHE_REPO = settings.CACHE_REPO   # the global project's repo (default)

# Fixed shuttle staging dirs — the tool never takes a drive path. Export writes to
# out/, import reads from in/; the OPERATOR copies out/ onto removable media,
# carries it across the gap, and drops it into in/ on the other machine. A named
# project nests under projects/<name>/ so its shuttle stays self-contained.
SHUTTLE_DIR = pathlib.Path(os.environ.get("PKGCACHE_SHUTTLE") or (ROOT / "shuttle"))
EXPORT_DIR = SHUTTLE_DIR / "out"
IMPORT_DIR = SHUTTLE_DIR / "in"

# Rewritten uv.locks land here for the UI to download; per-project like the shuttle.
LOCKWARM_DIR = pathlib.Path(os.environ.get("PKGCACHE_LOCKWARM") or (ROOT / "lockwarm"))

# On the OFFLINE host every project's cache repo shares ONE DVC object store, so an
# artifact already imported for one project is not copied again for the next — one
# physical copy per unique file no matter how many projects reference it. It MUST
# live on the same filesystem as every repo (link-based checkout), so it sits under
# caches/ (the single mounted volume). Only the import path (apply) wires repos to
# it, via .dvc/config.local (git-ignored); the ONLINE side keeps its default
# per-repo cache, so checkpoints/exports stay byte-for-byte unchanged. Opt out with
# PKGCACHE_SHARED_DVC_CACHE=0 to fall back to today's per-repo import behaviour.
_SHARED_DVC_STORE = CACHE_REPO / ".dvc-shared"
_SHARED_DVC_CACHE = os.environ.get(
    "PKGCACHE_SHARED_DVC_CACHE", "1").strip().lower() not in {"0", "false", "no", "off"}

# The pkgcache proxy's cross-project sha256 content store (see pkgcache config
# _CAS_SUBDIR). It sits under caches/ — i.e. inside the GLOBAL repo — so the
# checkpoint must git-ignore it, exactly like the shared DVC store, or `git add -A`
# would commit the raw object bytes. Keep the name in sync with pkgcache.
_CAS_DIR = CACHE_REPO / ".cas"

_HASH = re.compile(r"[0-9a-f]{7,40}")
# A bare hostname/IP for the rewritten lock's URLs — no scheme, port or path.
_HOST = re.compile(r"[a-zA-Z0-9.\-]+")


# ---- path / project helpers ---------------------------------------------

def _repo(project):
    """The git+DVC cache repo dir for a project (global → caches/)."""
    return projects.repo_dir(project)


def _export_dir(project):
    return EXPORT_DIR if project == GLOBAL else EXPORT_DIR / "projects" / project


def _import_dir(project):
    return IMPORT_DIR if project == GLOBAL else IMPORT_DIR / "projects" / project


def _is_hash(value):
    return bool(_HASH.fullmatch(value or ""))


def _echo(msg):
    """A progress line, mirroring the old scripts' `echo "==> ..."`."""
    return f"==> {msg}\n"


# The git trust env and the subprocess/git-log helpers now live in the proc gateway
# (app.gateways.proc); `run` is imported bare above so it stays monkeypatchable in
# tests, and _GIT_TRUST is kept as an alias for the direct subprocess.run calls below
# (git-mirror repack, apply's branch probe) that don't go through run().
_GIT_TRUST = proc.GIT_ENV


def lockwarm_path(project=GLOBAL):
    """Path of the rewritten uv.lock for a project — written by the lockwarm op,
    streamed by the UI's download route."""
    base = LOCKWARM_DIR if project == GLOBAL else LOCKWARM_DIR / "projects" / project
    return base / "uv.lock"


def shuttle_info(project=GLOBAL):
    """For /api/shuttle: the fixed staging paths + the checkpoints currently
    staged in the import dir (read from the sidecar an export wrote), so the UI
    can list what an import would bring in and show the copy instructions. Scoped
    to a project — each project has its own out/ and in/ subtree."""
    export_dir, import_dir = _export_dir(project), _import_dir(project)
    checkpoints, sidecar = [], import_dir / "checkpoints.json"
    if sidecar.is_file():
        try:
            checkpoints = json.loads(sidecar.read_text())
        except (OSError, ValueError):
            checkpoints = []
    return {
        "project": project,
        "export_dir": str(export_dir),
        "import_dir": str(import_dir),
        "import_ready": (import_dir / "repo.bundle").is_file(),
        "import_checkpoints": checkpoints,
    }


def _has_objects(tree):
    """True if a directory holds at least one real file (not just the empty 2-char
    fan-out dirs an aborted/partial copy can leave behind). Missing dir → False."""
    for _root, _dirs, files in os.walk(tree):
        if files:
            return True
    return False


def _find_md5_tree(import_dir):
    """Locate the populated DVC `md5/` object tree inside an import dir, tolerating
    the ways an operator's copy mangles the canonical out/projects/<name>/dvcstore/
    layout. DVC 3.x stores objects at <remote-root>/files/md5/…; the cases seen in
    the field are the dvcstore/ wrapper stripped, the files/ level stripped, or both
    (someone copies .dvc/cache/files/md5/ straight in). Return the Path to the md5/
    dir, or None if no populated object tree exists anywhere we look."""
    for cand in (
        import_dir / "dvcstore" / "files" / "md5",   # canonical (DVC 3.x)
        import_dir / "files" / "md5",                 # dvcstore/ wrapper stripped
        import_dir / "dvcstore" / "md5",              # files/ level stripped
        import_dir / "md5",                           # both stripped (raw md5/ tree)
    ):
        if _has_objects(cand):
            return cand
    return None


def _normalize_dvcstore(import_dir, md5_tree):
    """Put a located md5/ object tree into the canonical, DVC-readable position
    (<import_dir>/dvcstore/files/md5) and return the remote root (…/dvcstore). If
    the tree is already canonical this is a no-op move. DVC 3.x only reads a remote
    whose root contains files/md5/, so a stripped files/ level must be restored —
    we do it by relocating within the import dir (the operator's staging area)."""
    canonical = import_dir / "dvcstore" / "files" / "md5"
    if md5_tree.resolve() == canonical.resolve():
        return import_dir / "dvcstore"
    canonical.parent.mkdir(parents=True, exist_ok=True)
    os.replace(md5_tree, canonical)
    return import_dir / "dvcstore"


def _use_shared_dvc_cache(repo, cache_type="reflink,hardlink,copy"):
    """Point one cache repo at the shared DVC object store, so an artifact is stored
    ONCE across every project's repo instead of once per project.

    Used on BOTH sides: the offline import (apply) and the online checkpoint. Written
    to .dvc/config.local (which DVC git-ignores) rather than the tracked .dvc/config,
    so it never collides with the config that travels in the shuttle bundle and the
    offline repo stays a pure fast-forward mirror.

    cache_type picks how a materialized file relates to its store object:
      * offline import → "reflink,hardlink,copy": CoW clone where the fs supports it,
        else a hardlink, else a plain copy (import stays correct on any fs, and
        _unshare_ledgers repairs the one file the hardlink case would endanger).
      * online checkpoint → "reflink,copy" (NO hardlink): the live proxy rewrites
        ledger.db in place, and a hardlink into the store would corrupt the shared
        object — reflink is copy-on-write (safe) and copy is trivially safe, so
        neither needs an unshare pass here."""
    _SHARED_DVC_STORE.mkdir(parents=True, exist_ok=True)
    yield _echo(f"using the shared DVC object store {_SHARED_DVC_STORE} (cross-project dedup)")
    yield from run(["dvc", "cache", "dir", "--local", str(_SHARED_DVC_STORE)], cwd=repo)
    yield from run(["dvc", "config", "--local", "cache.type", cache_type], cwd=repo)
    # A large shared store makes DVC's "you may want to enable links" hint pure noise.
    yield from run(["dvc", "config", "--local", "cache.slow_link_warning", "false"], cwd=repo)
    _ignore_shared_store(pathlib.Path(repo))


def _ignore_shared_store(repo):
    """Git-ignore the shared DVC store when it sits inside this repo (the global-repo
    case). Thin wrapper kept for its focused call sites/tests."""
    _ignore_path_in_repo(repo, _SHARED_DVC_STORE)


def _ignore_path_in_repo(repo, target):
    """Ensure `target` is git-ignored when it sits INSIDE this repo's tree.

    Both the shared DVC store and the proxy's CAS live under caches/, which IS the
    global project's repo, so the global checkpoint's `git add -A` would otherwise
    stage their raw object bytes into git. Named-project repos live under
    caches/projects/<name>/, so these stores are outside them and this is a no-op.
    Idempotent."""
    try:
        rel = target.resolve().relative_to(repo.resolve())
    except ValueError:
        return  # target is outside this repo → git never sees it
    entry = f"/{rel}/"
    gitignore = repo / ".gitignore"
    existing = gitignore.read_text() if gitignore.exists() else ""
    if entry not in existing.splitlines():
        with open(gitignore, "a") as fh:
            fh.write(f"# cross-project object store (dedup; not git-tracked)\n{entry}\n")


def _unshare_ledgers(repo):
    """Give each role's ledger.db a PRIVATE inode after a link-based checkout.

    ledger.db lives inside a DVC-tracked role dir, so under hardlink checkout it is a
    link into the shared store; the always-on offline proxy is its single writer and
    folds the WAL into the main db file in place, which would corrupt the shared
    object every other project's checkout points at. Copying to a fresh inode and
    renaming over it breaks the link while preserving the (authoritative, just-
    imported) content. Harmless under copy/reflink checkout — a no-op re-copy / CoW
    break the proxy's writes wouldn't have needed."""
    detached = []
    for sub in projects.ROLE_SUBDIR.values():
        db = repo / sub / "ledger.db"
        if not db.is_file():
            continue
        tmp = db.with_name("ledger.db.unshare")
        shutil.copy2(db, tmp)
        os.replace(tmp, db)
        detached.append(sub)
    if detached:
        yield _echo(f"detached ledgers from the shared store ({', '.join(detached)})")


def _project_param(params, *, require_exists=True):
    """The validated project for an op, defaulting to the global project. For most
    ops the project must already exist (it's created via the projects API, which
    allocates ports); import is the exception — it may register a brand-new project
    from the shuttle, so the caller passes require_exists=False."""
    project = (params.get("project") or GLOBAL).strip() or GLOBAL
    if project == GLOBAL:
        return project
    try:
        projects.validate_name(project)
    except projects.ProjectError as exc:
        raise OpError(str(exc)) from exc
    if require_exists and not projects.exists(project):
        raise OpError(f"no such project: {project}")
    return project


# ---- the service (workflows + dispatch, shared by the webui and the CLI) ---

class Operations:
    """The air-gap cache workflows. Each public method is a generator of log lines;
    `build` validates a request and returns the right one. Stateless — every method
    derives the project's repo/shuttle paths from the helpers above on each call."""

    def _maintain_git(self, project, repo):
        """Compact this project's git mirrors before the DVC snapshot. Prefer the
        live git role's POST /+maintain (which repacks under each mirror's lock,
        correctly serialized against in-flight fetches); fall back to repacking
        directly if the role is unreachable (cache down → no fetches to race)."""
        git_dir = repo / "git"
        if not git_dir.is_dir() or not any(git_dir.glob("**/HEAD")):
            return  # no git mirrors → nothing to compact
        yield _echo("compacting git mirrors (geometric repack) before hashing")
        try:
            url = _git_maintain_url(project)
        except projects.ProjectError:
            url = None
        if url:
            try:
                req = urllib.request.Request(url, method="POST")
                with urllib.request.urlopen(
                    req, timeout=1800, context=ssl._create_unverified_context()
                ) as resp:
                    data = json.loads(resp.read().decode("utf-8") or "{}")
                yield _echo(f"  repacked {data.get('maintained', 0)} mirror(s) via the live git role")
                return
            except (urllib.error.URLError, OSError, ValueError) as exc:
                yield _echo(f"  git role unreachable ({exc}); repacking directly")
        env = dict(os.environ, **_GIT_TRUST)
        for head in sorted(git_dir.glob("**/HEAD")):
            mirror = head.parent
            if mirror.suffix != ".git":
                continue
            try:
                subprocess.run(["git", "--git-dir", str(mirror), "repack", "-d",
                                "--geometric=2", "--write-midx"],
                               cwd=str(repo), env=env, check=False, capture_output=True)
                subprocess.run(["git", "--git-dir", str(mirror), "pack-refs", "--all"],
                               cwd=str(repo), env=env, check=False, capture_output=True)
                yield _echo(f"  repacked {mirror.relative_to(git_dir)}")
            except OSError as exc:
                yield _echo(f"  skip {mirror.name}: {exc}")

    def checkpoint(self, project, msg):
        """Hash the cache into DVC and commit the pointers + manifest to git — WITHOUT
        taking the proxies down.

        The cache stays live the whole time. The storage layer writes every artifact
        via an atomic temp→fsync→rename at 0644 (see pkgcache/.../storage.py), so DVC
        never observes a partial file and a checkpoint can't corrupt a concurrent
        download; the per-ecosystem SQLite ledgers are crash-consistent and recover on
        open. In-flight `.part` files are skipped via .dvcignore, so an in-progress
        download is captured whole by the *next* checkpoint, never half by this one.

        (The old flow stopped the proxies to quiesce verdaccio/devpi's mutable indexes
        and chmod'd zot's 0700 OCI tree — both obsolete now that one atomic-write proxy
        replaced them. Dropping the stop/start is what keeps live downloads alive.)

        Everything here happens in this PROJECT's cache repo (global → caches/, else
        caches/projects/<name>/), separate from the code repo — so this commit, and any
        rollback of it, only ever touches that project's cache state."""
        repo = _repo(project)
        # First checkpoint on a fresh checkout: bootstrap the cache repo's own git +
        # DVC store so there's somewhere to commit/hash. Idempotent — each only runs
        # when its marker dir is absent.
        repo.mkdir(parents=True, exist_ok=True)
        if not (repo / ".git").is_dir():
            yield _echo(f"initializing the {project} cache-state git repo ({repo}, separate from the code)")
            yield from run(["git", "init", "-q"], cwd=repo)
        if not (repo / ".dvc").is_dir():
            yield _echo("initializing DVC store in the cache repo (first checkpoint)")
            yield from run(["dvc", "init", "-q"], cwd=repo)

        # Skip in-flight downloads (atomic temp→rename .part files) so a live
        # checkpoint never half-captures one. Idempotent.
        dvcignore = repo / ".dvcignore"
        existing_ignore = dvcignore.read_text() if dvcignore.exists() else ""
        if "*.part" not in existing_ignore:
            with open(dvcignore, "a") as fh:
                fh.write("# in-flight downloads (atomic temp→rename); skip the transient .part\n*.part\n")
        if "tmp_pack_" not in existing_ignore:
            with open(dvcignore, "a") as fh:
                fh.write("# git mid-fetch transients (incoming packs + lock files)\ntmp_pack_*\n*.lock\n")

        yield _echo("regenerating the cross-ecosystem manifest")
        # Point gen_manifest at THIS project's ledgers (global → caches/ by default).
        yield from run(["python3", "scripts/gen_manifest.py"], cwd=ROOT,
                       env={"PKGCACHE_MANIFEST_ROOT": str(repo)})

        # The ONE deliberate git-mirror file rewrite per checkpoint: geometric repack
        # so the DVC delta stays proportional to recent churn (mirrors run gc.auto=0).
        yield from self._maintain_git(project, repo)

        # Store objects in the shared, cross-project DVC cache with reflink (CoW)
        # materialization, so an artifact several projects hold is one set of blocks on
        # disk, not one copy per project — and re-checkpointing an artifact another
        # project already hashed is near-free. reflink,copy only (never hardlink): the
        # live proxy rewrites ledger.db in place, which CoW/copy tolerate but a
        # hardlink into the shared store would not. Set before `dvc add` so this
        # checkpoint's objects land in the shared store.
        if _SHARED_DVC_CACHE:
            yield from _use_shared_dvc_cache(repo, cache_type="reflink,copy")
        # Keep the proxy's CAS out of git (it lives under the global repo). Harmless
        # if the CAS is disabled/absent — it's just an ignore line.
        _ignore_path_in_repo(repo, _CAS_DIR)

        yield _echo("hashing caches into DVC (per-file dedup; only new files become objects)")
        # Which cache subdirs exist to hash (git added dynamically; skip absent ones).
        subdirs = []
        for sd in projects.ROLE_SUBDIR.values():
            if sd not in subdirs and (repo / sd).is_dir():
                subdirs.append(sd)
        if subdirs:
            yield from run(["dvc", "add", *subdirs], cwd=repo)

        yield _echo("committing pointers + manifest to the cache repo (the audit ledger)")
        yield from run(["git", "add", "-A"], cwd=repo)
        # A checkpoint with no new artifacts since the last one is a clean no-op, not a
        # failure — `git commit` would otherwise exit 1 ("nothing to commit").
        if not proc.has_staged_changes(repo):
            yield _echo("cache unchanged since the last checkpoint — nothing to commit")
            return
        yield from run(
            ["git", "commit", "-m", f"checkpoint: {msg}"],
            cwd=repo, env=proc.commit_identity_env(repo),
        )

        yield _echo(f"done. Run an export for '{project}' to stage the delta for transfer.")

    def export(self, project, base, target):
        """Stage a PROJECT's cache repo into its export dir (out/, or out/projects/<name>/):
        DVC objects (ALL of them for a full/from-scratch export, or just BASE..TARGET for
        an incremental one) + a self-contained git bundle + a checkpoints.json listing
        what's inside. For the global project it also ships the TLS material; named
        projects additionally write project.json (name + ports) so the air-gap side can
        register the project and bind its URLs on import. The operator then copies the
        export dir onto their removable media."""
        repo = _repo(project)
        export_dir = _export_dir(project)
        store = str(export_dir / "dvcstore")
        bundle = str(export_dir / "repo.bundle")

        yield _echo(f"staging export for '{project}' in {export_dir}")
        try:
            os.makedirs(store, exist_ok=True)
        except OSError as exc:
            raise OpError(
                f"can't write the export dir {export_dir} ({exc.strerror}). The control UI "
                f"runs as uid {os.getuid()}; point PKGCACHE_SHUTTLE at a dir you own, or fix "
                f"its permissions."
            )
        yield from run(["dvc", "remote", "add", "-f", "shuttle", store], cwd=repo)

        if base and target:
            yield _echo(f"incremental export — DVC delta for checkpoint range {base}..{target}")
            changed = proc.changed_dvc(repo, base, target)
            if not changed:
                yield _echo(f"no DVC-tracked changes between {base} and {target} — nothing to push")
            else:
                for path in changed:
                    yield f"    changed: {path}\n"
                # Push those outputs as they stand at TARGET; remote dedup skips
                # whatever the target machine already has (everything present at BASE).
                yield from run(["dvc", "push", "-r", "shuttle", "--rev", target, *changed], cwd=repo)
        else:
            # Full export (no base): push the complete object set for the current cache
            # (HEAD), so a machine that has NOTHING materializes the whole cache on
            # import. The remote starts empty on a fresh shuttle, so this sends it all.
            yield _echo("full export — pushing the complete cache object set (for a fresh/empty machine)")
            yield from run(["dvc", "push", "-r", "shuttle"], cwd=repo)

        # Always a FULL git bundle (a range bundle carries prerequisites, not history,
        # so a fresh host couldn't clone it). Atomic temp→rename so an interrupted
        # export never leaves a truncated bundle.
        yield _echo("bundling the cache repo's full history (self-contained/cloneable)")
        yield from run(["git", "bundle", "create", bundle + ".new", "--all"], cwd=repo)
        os.replace(bundle + ".new", bundle)

        # Sidecar list so the import side can show what's inside without unpacking.
        (export_dir / "checkpoints.json").write_text(
            json.dumps(proc.cache_checkpoints(repo), indent=2) + "\n"
        )

        if project == GLOBAL:
            # Ship ONLY what the air-gap side needs to serve HTTPS under the same CA:
            # ca.crt (clients trust it) + server.crt/server.key (the proxy serves with
            # it). The CA signing key (ca.key) NEVER leaves the online host — it could
            # mint certs trusted by every build host, so it must not ride a drive. The
            # certs are instance-wide, so only the global export carries them.
            certs = ROOT / "certs"
            shipped = [f for f in ("ca.crt", "server.crt", "server.key") if (certs / f).is_file()]
            if shipped:
                yield _echo("copying TLS material (ca.crt + server cert/key — NOT the CA private key)")
                dest = export_dir / "certs"
                os.makedirs(dest, exist_ok=True)
                for name in shipped:
                    shutil.copy2(certs / name, dest / name)
        else:
            # Carry the project's identity so import can register it (by name — the
            # central process routes it by URL prefix) on the air-gapped side.
            (export_dir / "project.json").write_text(
                json.dumps({"name": project}, indent=2) + "\n"
            )

        yield _echo(f"export ready — copy EVERYTHING in {export_dir} onto your shuttle media.")

    def apply(self, project):
        """Apply a PROJECT's shuttle from its import dir (in/, or in/projects/<name>/).
        The operator has already copied the exported files there off their media. Brings
        in cache STATE only: clone/ff the bundle into the project's repo, pull + checkout
        the DVC objects, and (for global) install certs + start the offline proxies. For
        a named project it registers the project (from project.json) so the always-on
        central process binds its URLs; the container is left as-is. A first import
        initialises everything."""
        import_dir = _import_dir(project)
        bundle = str(import_dir / "repo.bundle")
        # For named projects the repo lives under caches/projects/<name>; CACHE_DIR only
        # overrides the GLOBAL target (kept for the existing tests).
        if project == GLOBAL:
            cache_dir = os.environ.get("CACHE_DIR") or str(_repo(project))
        else:
            cache_dir = str(_repo(project))

        if not (import_dir.is_dir() and os.path.isfile(bundle)):
            raise OpError(
                f"no shuttle for '{project}' found in {import_dir} — copy the exported files "
                f"(repo.bundle, dvcstore/, …) off your media into {import_dir}, then import."
            )

        if not os.path.isdir(os.path.join(cache_dir, ".git")):
            yield _echo(f"first import: initialising the cache repo at {cache_dir} from the bundle")
            # Ensure the parent exists (named projects live under caches/projects/).
            os.makedirs(os.path.dirname(cache_dir), exist_ok=True)
            if os.path.isdir(cache_dir) and os.listdir(cache_dir):
                # The project was already registered on this side, so projects.create()
                # pre-created the per-role cache subdirs — the dir exists and is non-empty,
                # which `git clone` refuses. Clone into a temp sibling, graft its .git in,
                # then reset --hard to lay HEAD's tree over the (empty) pre-created subdirs.
                tmp = cache_dir + ".import-tmp"
                shutil.rmtree(tmp, ignore_errors=True)
                yield from run(["git", "clone", "--no-checkout", bundle, tmp], cwd=ROOT)
                os.replace(os.path.join(tmp, ".git"), os.path.join(cache_dir, ".git"))
                shutil.rmtree(tmp, ignore_errors=True)
                yield from run(["git", "reset", "--hard", "HEAD"], cwd=cache_dir)
            else:
                # Fresh checkout: the dir is absent (or empty) and clone creates it cleanly.
                yield from run(["git", "clone", bundle, cache_dir], cwd=ROOT)
        else:
            yield _echo("incremental import: fast-forwarding the cache repo from the bundle")
            # The air-gapped side is a pure mirror (never commits), so a fast-forward
            # is always possible. Fetch the bundle's branches into remote-tracking
            # refs, then ff-merge the current branch.
            branch = subprocess.run(
                ["git", "rev-parse", "--abbrev-ref", "HEAD"],
                cwd=cache_dir, text=True, capture_output=True, env=dict(os.environ, **_GIT_TRUST),
            ).stdout.strip()
            yield from run(["git", "fetch", bundle, "+refs/heads/*:refs/remotes/shuttle/*"], cwd=cache_dir)
            yield from run(["git", "merge", "--ff-only", f"refs/remotes/shuttle/{branch}"], cwd=cache_dir)

        # Locate the object store, tolerating the ways a manual copy mangles the
        # dvcstore/files/md5 layout. Failing fast here with a precise message beats
        # DVC's downstream "Everything is up to date" + opaque checkout failure when the
        # cache bytes are absent or misplaced.
        md5_tree = _find_md5_tree(import_dir)
        if md5_tree is None:
            raise OpError(
                f"no DVC object store found under {import_dir} — the cache bytes didn't "
                f"travel with the bundle. Copy the export's dvcstore/ directory into "
                f"{import_dir / 'dvcstore'} (so objects land at "
                f"{import_dir / 'dvcstore' / 'files' / 'md5'}/…), then import again."
            )
        canonical = import_dir / "dvcstore" / "files" / "md5"
        if md5_tree.resolve() != canonical.resolve():
            yield _echo(
                f"note: object store found at {md5_tree} (its dvcstore/files/ wrapper was "
                f"stripped on copy) — normalizing to {canonical} so DVC can read it"
            )
        store_dir = _normalize_dvcstore(import_dir, md5_tree)
        if _SHARED_DVC_CACHE:
            yield from _use_shared_dvc_cache(cache_dir)
        yield from run(["dvc", "remote", "add", "-f", "shuttle", str(store_dir)], cwd=cache_dir)
        yield _echo("fetching DVC objects from the shuttle")
        # --force: the role dirs carry a live ledger.db (the always-on proxy is its
        # single writer and holds it open in WAL), so DVC sees it as an "unsaved" file
        # and refuses to overwrite without confirmation. The air-gapped side is a pure
        # mirror — the imported checkpoint's ledger is authoritative (on a first import
        # the local one is a freshly-created empty throwaway) — so overwriting is the
        # intended outcome. The proxy reopens the ledger on its next restart/poll.
        yield from run(["dvc", "pull", "-r", "shuttle", "--force"], cwd=cache_dir)
        yield _echo("materializing cache dirs (dvc checkout)")
        yield from run(["dvc", "checkout", "--force"], cwd=cache_dir)
        if _SHARED_DVC_CACHE:
            # Detach each ledger.db from the shared store before the proxy writes it.
            yield from _unshare_ledgers(pathlib.Path(cache_dir))

        if project == GLOBAL:
            in_certs = str(import_dir / "certs")
            if os.path.isdir(in_certs):
                yield _echo("installing TLS certs from the shuttle (same CA as the online side)")
                shutil.copytree(in_certs, str(ROOT / "certs"), dirs_exist_ok=True)
            else:
                yield "WARNING: no certs/ in the shuttle — run scripts/gen-certs.sh on the online\n"
                yield "         side and re-export, or the HTTPS proxy won't have a certificate.\n"

            yield _echo("bringing up air-gapped proxies (serve-only)")
            yield from run(
                ["docker", "compose", "--profile", "offline", "up", "-d"],
                cwd=ROOT, env={"COMPOSE_PROFILE": "offline"},
            )

            yield _echo("done. Point air-gapped clients at (install certs/ca.crt to trust these):")
            uni = projects.UNIFIED_PORT
            for line in (
                f"    pip   ->  https://<host>:{uni}/global/pypi/root/pypi/+simple/",
                f"    npm   ->  https://<host>:{uni}/global/npm/",
                f"    docker->  <host>:{uni}   (pull <host>:{uni}/dockerhub/library/<img>, /ghcr/<org>/<img>, /quay/<org>/<img>)",
                f"    git   ->  https://<host>:{uni}/global/git/<upstream-host>/<owner>/<repo>.git",
                f"    files ->  https://<host>:{uni}/global/files/<path>",
                f"    apt   ->  http://<host>:{projects.APT_PORT}/   (plain HTTP proxy; apk too)",
            ):
                yield line + "\n"
            return

        # Named project: register it (name only — projects share the default ports
        # and are routed by URL prefix) so the always-on central process starts
        # routing this project's URLs on its next registry poll. No recreate/rebind.
        registry = projects.load_registry()
        if project not in registry["projects"]:
            registry["projects"][project] = {}
            projects.save_registry(registry)
            yield _echo(f"registered project '{project}'")
        else:
            yield _echo(f"project '{project}' was already registered")
        yield _echo(f"done. '{project}' is materialized; the cache process will route it shortly.")

    def rollback(self, project, commit):
        """Restore the cache pointers at COMMIT, then materialize the matching blobs.
        Operates only in the project's cache repo — the application code is never touched."""
        repo = _repo(project)
        yield from run(["git", "checkout", commit], cwd=repo)
        yield from run(["dvc", "checkout"], cwd=repo)

    def lockwarm(self, project, lock_text, host):
        """Warm the cache from an uploaded uv.lock, then write a rewritten lock whose
        registry + file URLs pull through THIS cache (hashes preserved, so uv still
        verifies bytes). Online-only — warming fetches the pinned files from upstream.

        We pull each file the lock enumerates (every platform/python), not a re-resolve
        by name, so the air-gapped side has the exact closure the lock promises. Any
        file that fails to cache aborts the rewrite — a lock pointing at an uncached
        file would break an offline `uv sync`."""
        base, prefix = pypi_internal(project)
        public_base = f"https://{host}:{projects.ROLE_PORT['pypi']}{prefix}"
        proxy = Proxy(base)

        yield _echo("parsing the uploaded uv.lock")
        packages = LockParser().parse(lock_text)
        if not packages:
            yield _echo("no registry-sourced packages found — nothing to warm")
            return
        total = sum(len(p.files) for p in packages)

        yield _echo(f"checking the '{project}' pypi proxy is online")
        if proxy.offline():
            raise OpError("the cache is OFFLINE — switch to online mode before warming")

        index_map = IndexMap(proxy.indexes())
        unknown = sorted({p.registry for p in packages if index_map.index(p.registry) is None})
        if unknown:
            raise OpError(
                "no configured PKGCache index for: " + ", ".join(unknown)
                + " — add it under roles.pypi.indexes in pkgcache.yaml and retry"
            )

        yield _echo(f"warming {total} files from {len(packages)} packages via {base}")
        items = [(index_map.index(pkg.registry), pkg.project, f.filename)
                 for pkg in packages for f in pkg.files]
        failed = 0
        for result in Warmer(proxy).warm(items):
            if result.ok:
                yield f"    cached {result.filename}\n"
            else:
                failed += 1
                yield f"    FAIL {result.detail} {result.filename}\n"
        if failed:
            raise OpError(f"{failed} of {total} files failed to cache — lock not rewritten; fix and retry")

        yield _echo("rewriting the lock to pull through this cache (hashes preserved)")
        out = lockwarm_path(project)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(LockRewriter().rewrite(lock_text, packages, index_map, public_base))
        yield _echo(f"done — download the rewritten uv.lock from the UI ({out})")
        yield _echo(f"to re-resolve against this cache, point uv's index at {public_base}/<index>/+simple")

    def mode(self, target):
        """Recreate just the pkgcache container under the online/offline profile."""
        yield _echo(f"switching cache to {target}")
        yield from run(
            ["docker", "compose", "--profile", target, "up", "-d", "--no-deps", "pkgcache"],
            env={"OFFLINE": "1" if target == "offline" else "0"},
        )

    def build(self, action, params):
        """Validate params eagerly (raising OpError on bad input — so a UI POST gets
        an immediate error) and return the log-line generator for the action. All
        cache-state ops take an optional `project` (default: the global project)."""
        params = params or {}
        if action == "checkpoint":
            project = _project_param(params)
            msg = (params.get("message") or "").strip()
            if not msg:
                raise OpError("a checkpoint message is required")
            return self.checkpoint(project, msg)
        if action == "export":
            project = _project_param(params)
            # No drive — exports always go to the project's fixed export dir. With no
            # base it's a FULL export (for a fresh machine); base+target is a delta.
            base = (params.get("base") or "").strip()
            target = (params.get("target") or "").strip()
            if base or target:
                if not (_is_hash(base) and _is_hash(target)):
                    raise OpError("an incremental export needs a valid base and target checkpoint")
            else:
                base = target = None
            return self.export(project, base, target)
        if action == "import":
            # Imports always read the project's fixed import dir; a named project may be
            # registered fresh from the shuttle, so it need not exist yet.
            project = _project_param(params, require_exists=False)
            return self.apply(project)
        if action == "lockwarm":
            project = _project_param(params)
            lock = params.get("lock")
            if not isinstance(lock, str) or not lock.strip():
                raise OpError("a uv.lock file is required")
            host = (params.get("host") or "").strip()
            if not _HOST.fullmatch(host):
                raise OpError("a valid cache host (bare hostname or IP) is required")
            return self.lockwarm(project, lock, host)
        if action == "rollback":
            project = _project_param(params)
            commit = (params.get("commit") or "").strip()
            if not _is_hash(commit):
                raise OpError("invalid commit hash")
            return self.rollback(project, commit)
        if action == "mode":
            # Online/offline is instance-wide (one container, all projects) — no project.
            target = (params.get("target") or "").strip()
            if target not in ("online", "offline"):
                raise OpError("mode target must be 'online' or 'offline'")
            return self.mode(target)
        raise OpError(f"unknown action: {action}")
