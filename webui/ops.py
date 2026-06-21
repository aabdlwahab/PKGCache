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
import subprocess

import projects
from projects import GLOBAL

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The cache state is versioned in its OWN git + DVC repo. The GLOBAL project's repo
# is caches/ (unchanged); each named project gets caches/projects/<name>/, its own
# git+DVC repo, so a per-project checkpoint / rollback / shuttle only ever touches
# that one project's state. All git/dvc calls below run with cwd=_repo(project);
# only the manifest regen and `docker compose` run from ROOT.
CACHE_REPO = ROOT / "caches"   # the global project's repo (default)

# Fixed shuttle staging dirs — the tool never takes a drive path. Export writes to
# out/, import reads from in/; the OPERATOR copies out/ onto removable media,
# carries it across the gap, and drops it into in/ on the other machine. A named
# project nests under projects/<name>/ so its shuttle stays self-contained.
SHUTTLE_DIR = pathlib.Path(os.environ.get("PKGCACHE_SHUTTLE") or (ROOT / "shuttle"))
EXPORT_DIR = SHUTTLE_DIR / "out"
IMPORT_DIR = SHUTTLE_DIR / "in"

_HASH = re.compile(r"[0-9a-f]{7,40}")


class OpError(RuntimeError):
    """A bad request (failed validation) or a failed step. Subclasses
    RuntimeError so the webui's POST handler turns it into a 400."""


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


# git refuses to touch a repo owned by a different uid ("dubious ownership"),
# which bites whenever this tool runs as a different user than the checkout's
# owner (e.g. root in a container, or root vs. the host user). We only ever
# operate on our own repos, so trust them for every git call — including the
# ones dvc spawns underneath — via git's env-based config. No global/sudo
# `git config safe.directory` needed.
_GIT_TRUST = {
    "GIT_CONFIG_COUNT": "1",
    "GIT_CONFIG_KEY_0": "safe.directory",
    "GIT_CONFIG_VALUE_0": "*",
    # Keep cache-repo git calls from climbing UP into the code repo at ROOT (the
    # `git init` bootstrap aside, nothing here should ever touch the code repo).
    "GIT_CEILING_DIRECTORIES": str(ROOT),
}


def _commit_identity_env(repo):
    """A default 'pkgcache' committer identity, used ONLY when the environment
    has none configured. A fresh container (root, no ~/.gitconfig) would
    otherwise abort the checkpoint commit with 'Author identity unknown'; an
    operator's own git config or GIT_* env still takes precedence."""
    probe_env = dict(os.environ, **_GIT_TRUST)

    def have(cfg_key, *env_keys):
        if any(os.environ.get(k) for k in env_keys):
            return True
        res = subprocess.run(
            ["git", "config", cfg_key], cwd=str(repo),
            text=True, capture_output=True, env=probe_env,
        )
        return res.returncode == 0 and bool(res.stdout.strip())

    env = {}
    if not have("user.name", "GIT_COMMITTER_NAME", "GIT_AUTHOR_NAME"):
        env["GIT_AUTHOR_NAME"] = env["GIT_COMMITTER_NAME"] = "pkgcache"
    if not have("user.email", "GIT_COMMITTER_EMAIL", "GIT_AUTHOR_EMAIL", "EMAIL"):
        env["GIT_AUTHOR_EMAIL"] = env["GIT_COMMITTER_EMAIL"] = "pkgcache@localhost"
    return env


def _has_staged_changes(repo):
    """True if the cache repo has anything staged to commit (so we can skip a
    no-op checkpoint instead of letting `git commit` fail with 'nothing to commit')."""
    res = subprocess.run(
        ["git", "diff", "--cached", "--quiet"],
        cwd=str(repo), env=dict(os.environ, **_GIT_TRUST),
    )
    return res.returncode == 1  # 0 = nothing staged, 1 = staged changes present


def run(cmd, env=None, cwd=None):
    """Run one command, yielding its banner then its combined output line by
    line, and raising OpError on a non-zero exit. The yielded text is exactly
    what the UI streams (and what stdout shows on the CLI)."""
    cwd = str(cwd or ROOT)
    yield "$ " + " ".join(cmd) + "\n"
    full_env = dict(os.environ, **_GIT_TRUST)
    if env:
        full_env.update(env)
    try:
        proc = subprocess.Popen(
            cmd, cwd=cwd, env=full_env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        )
    except OSError as exc:
        raise OpError(f"could not start {cmd[0]}: {exc}") from exc
    assert proc.stdout is not None
    for line in proc.stdout:
        yield line
    proc.wait()
    if proc.returncode != 0:
        raise OpError(f"{cmd[0]} exited {proc.returncode}")


def _changed_dvc(repo, base, target):
    """The DVC-tracked cache dirs whose pointer changed between two checkpoints
    (pointers live at the cache repo root, e.g. docker.dvc)."""
    res = subprocess.run(
        ["git", "diff", "--name-only", base, target, "--", "*.dvc"],
        cwd=str(repo), text=True, capture_output=True,
        env=dict(os.environ, **_GIT_TRUST),
    )
    if res.returncode != 0:
        raise OpError(res.stderr.strip() or f"git diff {base}..{target} failed")
    return [line for line in res.stdout.splitlines() if line.strip()]


def _cache_checkpoints(cwd):
    """Cache-repo checkpoints [{hash,short,date,subject}] from cwd's git log, or
    [] if cwd isn't a git repo (no checkpoints yet)."""
    res = subprocess.run(
        ["git", "log", "--pretty=format:%H%x1f%h%x1f%ad%x1f%s", "--date=short"],
        cwd=str(cwd), text=True, capture_output=True, env=dict(os.environ, **_GIT_TRUST),
    )
    if res.returncode != 0:
        return []
    out = []
    for line in res.stdout.splitlines():
        parts = line.split("\x1f")
        if len(parts) == 4:
            out.append({"hash": parts[0], "short": parts[1], "date": parts[2], "subject": parts[3]})
    return out


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
        if "*.part" not in (dvcignore.read_text() if dvcignore.exists() else ""):
            with open(dvcignore, "a") as fh:
                fh.write("# in-flight downloads (atomic temp→rename); skip the transient .part\n*.part\n")

        yield _echo("regenerating the cross-ecosystem manifest")
        # Point gen_manifest at THIS project's ledgers (global → caches/ by default).
        yield from run(["python3", "scripts/gen_manifest.py"], cwd=ROOT,
                       env={"PKGCACHE_MANIFEST_ROOT": str(repo)})

        yield _echo("hashing caches into DVC (per-file dedup; only new files become objects)")
        yield from run(["dvc", "add", "docker", "npm", "pip", "apt"], cwd=repo)

        yield _echo("committing pointers + manifest to the cache repo (the audit ledger)")
        yield from run(["git", "add", "-A"], cwd=repo)
        # A checkpoint with no new artifacts since the last one is a clean no-op, not a
        # failure — `git commit` would otherwise exit 1 ("nothing to commit").
        if not _has_staged_changes(repo):
            yield _echo("cache unchanged since the last checkpoint — nothing to commit")
            return
        yield from run(
            ["git", "commit", "-m", f"checkpoint: {msg}"],
            cwd=repo, env=_commit_identity_env(repo),
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
            changed = _changed_dvc(repo, base, target)
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
            json.dumps(_cache_checkpoints(repo), indent=2) + "\n"
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
            # Carry the project's identity so import can register it (and the central
            # process can bind its ports) on the air-gapped side.
            (export_dir / "project.json").write_text(
                json.dumps({"name": project, "ports": projects.ports(project)}, indent=2) + "\n"
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
            for line in (
                "    pip   ->  https://<host>:3141/root/pypi/+simple/",
                "    npm   ->  https://<host>:4873/",
                "    docker->  <host>:5000   (zot: pull <host>:5000/dockerhub/library/<img>, /ghcr/<org>/<img>, /quay/<org>/<img>)",
                "    apt   ->  http://<host>:3142/   (plain HTTP proxy; apk too)",
            ):
                yield line + "\n"
            return

        # Named project: register it (with the ports the online side assigned) so the
        # always-on central process binds this project's URLs on its next poll. No
        # container recreate — the pool ports are already published.
        meta_path = import_dir / "project.json"
        ports = None
        if meta_path.is_file():
            try:
                ports = (json.loads(meta_path.read_text()) or {}).get("ports")
            except (OSError, ValueError):
                ports = None
        if ports:
            registry = projects.load_registry()
            registry["projects"][project] = {r: int(ports[r]) for r in projects.ROLES if r in ports}
            projects.save_registry(registry)
            yield _echo(f"registered project '{project}' on ports {registry['projects'][project]}")
        else:
            yield (f"WARNING: no project.json in the shuttle — '{project}' imported but not "
                   f"registered; create it in the UI (or add it to the registry) to serve it.\n")
        yield _echo(f"done. '{project}' is materialized; the cache process will bind its ports shortly.")

    def rollback(self, project, commit):
        """Restore the cache pointers at COMMIT, then materialize the matching blobs.
        Operates only in the project's cache repo — the application code is never touched."""
        repo = _repo(project)
        yield from run(["git", "checkout", commit], cwd=repo)
        yield from run(["dvc", "checkout"], cwd=repo)

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
