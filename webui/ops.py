"""Air-gap cache operations — the backend's own copy of the logic, so the control
UI is self-contained and never reaches into scripts/.

Every operation is a *generator that yields log lines* as it runs. Two callers
share this one implementation:

  * the control UI (webui/jobs.py) imports `build` and streams each yielded line
    into its job console, and
  * the operator CLI (scripts/pkgops.py) is a thin wrapper over this module, so
    `python3 scripts/pkgops.py <action> …` runs the exact same code by hand on
    the air-gapped host.

Python does all the orchestration, validation and control flow; git/dvc/docker
are still the underlying CLIs (there is no Python-native equivalent for
`git bundle` or `docker compose --profile`), invoked via subprocess with NO
shell — so untrusted values (drive paths, commit hashes) are always separate
argv elements, never interpolated into a command line.
"""
import os
import pathlib
import re
import shutil
import subprocess

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The cache state is versioned in its OWN git + DVC repo rooted at caches/, kept
# separate from the code repo (ROOT): checkpoints, rollback and the shuttle only
# ever touch cache state, never the application code. All git/dvc calls below run
# with cwd=CACHE_REPO; only the manifest regen and `docker compose` run from ROOT.
CACHE_REPO = ROOT / "caches"

_HASH = re.compile(r"[0-9a-f]{7,40}")


class OpError(RuntimeError):
    """A bad request (failed validation) or a failed step. Subclasses
    RuntimeError so the webui's POST handler turns it into a 400."""


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
}


def _commit_identity_env():
    """A default 'pkgcache' committer identity, used ONLY when the environment
    has none configured. A fresh container (root, no ~/.gitconfig) would
    otherwise abort the checkpoint commit with 'Author identity unknown'; an
    operator's own git config or GIT_* env still takes precedence."""
    probe_env = dict(os.environ, **_GIT_TRUST)

    def have(cfg_key, *env_keys):
        if any(os.environ.get(k) for k in env_keys):
            return True
        res = subprocess.run(
            ["git", "config", cfg_key], cwd=str(CACHE_REPO),
            text=True, capture_output=True, env=probe_env,
        )
        return res.returncode == 0 and bool(res.stdout.strip())

    env = {}
    if not have("user.name", "GIT_COMMITTER_NAME", "GIT_AUTHOR_NAME"):
        env["GIT_AUTHOR_NAME"] = env["GIT_COMMITTER_NAME"] = "pkgcache"
    if not have("user.email", "GIT_COMMITTER_EMAIL", "GIT_AUTHOR_EMAIL", "EMAIL"):
        env["GIT_AUTHOR_EMAIL"] = env["GIT_COMMITTER_EMAIL"] = "pkgcache@localhost"
    return env


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


# ---- operations (each is a generator of log lines) ----------------------

def _checkpoint(msg):
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

    Everything here happens in the cache repo (caches/), separate from the code
    repo — so this commit, and any rollback of it, only ever touches cache state."""
    # First checkpoint on a fresh checkout: bootstrap the cache repo's own git +
    # DVC store so there's somewhere to commit/hash. Idempotent — each only runs
    # when its marker dir is absent.
    if not (CACHE_REPO / ".git").is_dir():
        yield _echo("initializing the cache-state git repo (caches/, separate from the code)")
        yield from run(["git", "init", "-q"], cwd=CACHE_REPO)
    if not (CACHE_REPO / ".dvc").is_dir():
        yield _echo("initializing DVC store in the cache repo (first checkpoint)")
        yield from run(["dvc", "init", "-q"], cwd=CACHE_REPO)

    yield _echo("regenerating the cross-ecosystem manifest")
    yield from run(["python3", "scripts/gen_manifest.py"], cwd=ROOT)

    yield _echo("hashing caches into DVC (per-file dedup; only new files become objects)")
    yield from run(["dvc", "add", "docker", "npm", "pip", "apt"], cwd=CACHE_REPO)

    yield _echo("committing pointers + manifest to the cache repo (the audit ledger)")
    yield from run(["git", "add", "-A"], cwd=CACHE_REPO)
    yield from run(
        ["git", "commit", "-m", f"checkpoint: {msg}"],
        cwd=CACHE_REPO, env=_commit_identity_env(),
    )

    yield _echo("done. Run `pkgops export <drive>` to stage the delta for transfer.")


def _changed_dvc(base, target):
    """The DVC-tracked cache dirs whose pointer changed between two checkpoints
    (pointers live at the cache repo root, e.g. docker.dvc)."""
    res = subprocess.run(
        ["git", "diff", "--name-only", base, target, "--", "*.dvc"],
        cwd=str(CACHE_REPO), text=True, capture_output=True,
        env=dict(os.environ, **_GIT_TRUST),
    )
    if res.returncode != 0:
        raise OpError(res.stderr.strip() or f"git diff {base}..{target} failed")
    return [line for line in res.stdout.splitlines() if line.strip()]


def _export(drive, base, target):
    """Stage the shuttle: the cache repo's DVC objects (full delta, or just
    BASE..TARGET) + a full, self-contained git bundle of the cache repo + the TLS
    certs. Only cache state crosses the gap — the application code does not."""
    store = os.path.join(drive, "dvcstore")
    bundle = os.path.join(drive, "repo.bundle")

    yield _echo(f"staging shuttle at {drive}")
    os.makedirs(store, exist_ok=True)
    # Register the drive as a DVC remote named 'shuttle' (idempotent).
    yield from run(["dvc", "remote", "add", "-f", "shuttle", store], cwd=CACHE_REPO)

    if base and target:
        yield _echo(f"exporting DVC delta for checkpoint range {base}..{target}")
        changed = _changed_dvc(base, target)
        if not changed:
            yield _echo(f"no DVC-tracked changes between {base} and {target} — nothing to push")
        else:
            for path in changed:
                yield f"    changed: {path}\n"
            # Push those outputs as they stand at TARGET; remote dedup skips
            # anything already on the drive (i.e. everything present at BASE).
            yield from run(["dvc", "push", "-r", "shuttle", "--rev", target, *changed], cwd=CACHE_REPO)
    else:
        yield _echo("pushing DVC objects to shuttle (delta only)")
        yield from run(["dvc", "push", "-r", "shuttle"], cwd=CACHE_REPO)

    # Always a FULL bundle of the cache repo: a range bundle carries prerequisites,
    # not history, so a brand-new air-gapped host couldn't clone it. Pointers +
    # manifests are KBs — cheap. Write to a temp name and atomically swap so an
    # interrupted export never leaves a truncated bundle on the drive.
    yield _echo("bundling the cache repo's full history to shuttle (self-contained/cloneable)")
    yield from run(["git", "bundle", "create", bundle + ".new", "--all"], cwd=CACHE_REPO)
    os.replace(bundle + ".new", bundle)

    # The TLS key/cert are git-ignored, so carry them on the trusted shuttle —
    # the air-gapped side then serves HTTPS with the SAME CA.
    certs = ROOT / "certs"
    if certs.is_dir():
        yield _echo("copying TLS certs to shuttle (CA + server cert)")
        shutil.copytree(certs, os.path.join(drive, "certs"), dirs_exist_ok=True)

    yield _echo(f"shuttle ready at {drive} — carry it to the air-gapped network.")


def _import(drive, repo_dir):
    """Apply a shuttle on the air-gapped host. The code (compose, images) is
    already present here; this only brings in cache STATE, by cloning/ff-merging
    the cache-repo bundle into this checkout's caches/ dir, pulling the DVC
    objects, materializing them, installing the certs, and starting the proxies."""
    store = os.path.join(drive, "dvcstore")
    bundle = os.path.join(drive, "repo.bundle")
    # The cache repo is this checkout's caches/ (override with CACHE_DIR for tests).
    cache_dir = repo_dir or os.environ.get("CACHE_DIR") or str(CACHE_REPO)

    if not os.path.isdir(os.path.join(cache_dir, ".git")):
        yield _echo(f"first import: cloning the cache repo into {cache_dir}")
        # caches/ isn't tracked by the code repo, so on a fresh checkout it's
        # absent and clone creates it cleanly.
        yield from run(["git", "clone", bundle, cache_dir], cwd=ROOT)
    else:
        yield _echo("incremental import: fast-forwarding the cache repo from bundle")
        # The air-gapped side is a pure mirror (never commits), so a fast-forward
        # is always possible. Fetch the bundle's branches into remote-tracking
        # refs, then ff-merge the current branch.
        branch = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            cwd=cache_dir, text=True, capture_output=True, env=dict(os.environ, **_GIT_TRUST),
        ).stdout.strip()
        yield from run(["git", "fetch", bundle, "+refs/heads/*:refs/remotes/shuttle/*"], cwd=cache_dir)
        yield from run(["git", "merge", "--ff-only", f"refs/remotes/shuttle/{branch}"], cwd=cache_dir)

    yield from run(["dvc", "remote", "add", "-f", "shuttle", store], cwd=cache_dir)
    yield _echo("fetching new DVC objects from shuttle (delta only)")
    yield from run(["dvc", "pull", "-r", "shuttle"], cwd=cache_dir)
    yield _echo("materializing cache dirs (dvc checkout)")
    yield from run(["dvc", "checkout"], cwd=cache_dir)

    drive_certs = os.path.join(drive, "certs")
    if os.path.isdir(drive_certs):
        yield _echo("installing TLS certs from shuttle (same CA as the online side)")
        shutil.copytree(drive_certs, str(ROOT / "certs"), dirs_exist_ok=True)
    else:
        yield "WARNING: no certs/ on the shuttle — run scripts/gen-certs.sh on the online\n"
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


def _rollback(commit):
    """Restore the cache pointers at COMMIT, then materialize the matching blobs.
    Operates only in the cache repo — the application code is never touched."""
    yield from run(["git", "checkout", commit], cwd=CACHE_REPO)
    yield from run(["dvc", "checkout"], cwd=CACHE_REPO)


def _mode(target):
    """Recreate just the pkgcache container under the online/offline profile."""
    yield _echo(f"switching cache to {target}")
    yield from run(
        ["docker", "compose", "--profile", target, "up", "-d", "--no-deps", "pkgcache"],
        env={"OFFLINE": "1" if target == "offline" else "0"},
    )


# ---- dispatch (shared by the webui and the CLI) -------------------------

def build(action, params):
    """Validate params eagerly (raising OpError on bad input — so a UI POST gets
    an immediate error) and return the log-line generator for the action."""
    params = params or {}
    if action == "checkpoint":
        msg = (params.get("message") or "").strip()
        if not msg:
            raise OpError("a checkpoint message is required")
        return _checkpoint(msg)
    if action == "export":
        drive = (params.get("drive") or "").strip()
        if not drive:
            raise OpError("a shuttle drive path is required")
        base = (params.get("base") or "").strip()
        target = (params.get("target") or "").strip()
        if base or target:
            if not (_is_hash(base) and _is_hash(target)):
                raise OpError("export range needs a valid base and target checkpoint")
        else:
            base = target = None
        return _export(drive, base, target)
    if action == "import":
        drive = (params.get("drive") or "").strip()
        if not drive:
            raise OpError("a shuttle drive path is required")
        return _import(drive, (params.get("repo_dir") or "").strip() or None)
    if action == "rollback":
        commit = (params.get("commit") or "").strip()
        if not _is_hash(commit):
            raise OpError("invalid commit hash")
        return _rollback(commit)
    if action == "mode":
        target = (params.get("target") or "").strip()
        if target not in ("online", "offline"):
            raise OpError("mode target must be 'online' or 'offline'")
        return _mode(target)
    raise OpError(f"unknown action: {action}")
