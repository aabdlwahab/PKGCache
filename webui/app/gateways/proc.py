"""Subprocess gateway — the one boundary where the backend shells out to git, dvc
and docker. Every command runs with NO shell, so untrusted values (drive paths,
commit hashes) are always separate argv elements, never interpolated into a line.

`run` streams a command's combined output line by line (what the UI job console
shows and the CLI prints) and raises OpError on a non-zero exit. The rest are small
git helpers shared by the operations and reads services, so the git trust env and
the checkpoint-log parsing live in exactly one place."""
import os
import subprocess

from app import settings
from app.errors import OpError

# git refuses a repo owned by another uid ("dubious ownership"); trust our own repos
# for every git call (including the ones dvc spawns) via env-based config. Single
# definition, re-exported from settings so call sites that build their own env agree.
GIT_ENV = settings.GIT_ENV


def _git_env(extra=None):
    env = dict(os.environ, **GIT_ENV)
    if extra:
        env.update(extra)
    return env


def run(cmd, env=None, cwd=None):
    """Run one command, yielding its banner then its combined output line by line,
    and raising OpError on a non-zero exit. The yielded text is exactly what the UI
    streams (and what stdout shows on the CLI)."""
    cwd = str(cwd or settings.ROOT)
    yield "$ " + " ".join(cmd) + "\n"
    full_env = _git_env(env)
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


def commit_identity_env(repo):
    """A default 'pkgcache' committer identity, used ONLY when the environment has
    none configured. A fresh container (root, no ~/.gitconfig) would otherwise abort
    the checkpoint commit with 'Author identity unknown'; an operator's own git
    config or GIT_* env still takes precedence."""
    probe_env = _git_env()

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


def has_staged_changes(repo):
    """True if the cache repo has anything staged to commit (so a no-op checkpoint is
    skipped instead of letting `git commit` fail with 'nothing to commit')."""
    res = subprocess.run(
        ["git", "diff", "--cached", "--quiet"],
        cwd=str(repo), env=_git_env(),
    )
    return res.returncode == 1  # 0 = nothing staged, 1 = staged changes present


def changed_dvc(repo, base, target):
    """The DVC-tracked cache dirs whose pointer changed between two checkpoints
    (pointers live at the cache repo root, e.g. docker.dvc)."""
    res = subprocess.run(
        ["git", "diff", "--name-only", base, target, "--", "*.dvc"],
        cwd=str(repo), text=True, capture_output=True, env=_git_env(),
    )
    if res.returncode != 0:
        raise OpError(res.stderr.strip() or f"git diff {base}..{target} failed")
    return [line for line in res.stdout.splitlines() if line.strip()]


def git_head(repo):
    """Full HEAD sha, or '' if not resolvable (no commits / not a repo)."""
    try:
        res = subprocess.run(
            ["git", "rev-parse", "--verify", "HEAD"], cwd=str(repo),
            text=True, capture_output=True, timeout=10, env=_git_env(),
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return res.stdout.strip()


def git_log(repo, limit=None):
    """[(hash, short, date, subject)] newest-first from the repo's log, or [] on any
    failure (not a repo, git missing, timeout). One parse for both the checkpoint
    sidecar and the History panel."""
    cmd = ["git", "log", "--pretty=format:%H%x1f%h%x1f%ad%x1f%s", "--date=short"]
    if limit:
        cmd.insert(2, f"-{limit}")
    try:
        raw = subprocess.run(
            cmd, cwd=str(repo), text=True, capture_output=True, timeout=10, env=_git_env(),
        ).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    rows = []
    for line in raw.splitlines():
        parts = line.split("\x1f")
        if len(parts) == 4:
            rows.append(tuple(parts))
    return rows


def cache_checkpoints(cwd):
    """Cache-repo checkpoints [{hash,short,date,subject}] from cwd's git log, or []
    if cwd isn't a git repo yet."""
    return [{"hash": h, "short": s, "date": d, "subject": subj}
            for (h, s, d, subj) in git_log(cwd)]
