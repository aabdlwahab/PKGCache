"""Local bare-mirror lifecycle for the git role.

A git fetch is a negotiation (the client posts its have/want set and the server
computes a bespoke packfile), so responses can't be byte-cached by URL like the
other ecosystems. Instead we keep a real bare mirror on disk and run `git
upload-pack` against it — mirror-and-serve. This module owns the mirrors: create
(init --bare + fetch heads+tags), revalidate on a TTL, sync HEAD, sync the ledger,
and the checkpoint-time geometric repack.

Invariants that keep this DVC-safe and reader-safe:
  * gc.auto=0 + maintenance.auto=false + --no-auto-maintenance → git NEVER rewrites
    packfiles on its own; the only rewrite is the explicit repack at checkpoint.
  * one asyncio lock per repo serializes clone/fetch/HEAD-sync/repack (writers);
    upload-pack readers take no lock. Safe because fetch lands objects via
    tmp-pack+rename before moving refs, and prune removes refs, never objects.
    (Local filesystem only — unlink-while-open is unsafe on NFS.)
"""
from __future__ import annotations

import asyncio
import os
import re
import time
from pathlib import Path

# heads + tags only — skip refs/pull/* etc, which balloon busy GitHub mirrors and
# the ref advertisement. (Documented limitation: commits reachable only from those
# refs aren't mirrored.)
_REFSPECS = ("+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
_PROGRESS_RE = re.compile(r"([A-Za-z ]+):\s+(\d+)% \((\d+)/(\d+)\)")
_STREAM_CHUNK = 1 << 16


class MirrorError(Exception):
    """A git subprocess (clone/fetch/repack) failed — carries the stderr tail."""


class NotCached(Exception):
    """Offline and the mirror doesn't exist — the air-gap miss."""


class MirrorManager:
    def __init__(self, *, storage, ledger, progress, stats, is_offline,
                 refs_ttl: float, max_upload_packs: int) -> None:
        self._storage = storage
        self._ledger = ledger
        self._progress = progress
        self._stats = stats
        # A callable, not a snapshot: the supervisor live-swaps the owning core's
        # config when the per-project offline flag changes, and the mirror must
        # follow the mode actually being served.
        self._is_offline = is_offline
        self._refs_ttl = refs_ttl
        self._locks: dict[str, asyncio.Lock] = {}
        self._fresh: dict[str, float] = {}   # repo -> monotonic ts of last revalidate/clone
        self._sem = asyncio.Semaphore(max_upload_packs)
        self._env = _build_env()

    def _lock(self, repo: str) -> asyncio.Lock:
        lk = self._locks.get(repo)
        if lk is None:
            lk = self._locks[repo] = asyncio.Lock()
        return lk

    def is_fresh(self, repo: str) -> bool:
        ts = self._fresh.get(repo)
        return ts is not None and (time.monotonic() - ts) < self._refs_ttl

    # ---- ensure a mirror exists and is fresh --------------------------------
    async def ensure(self, repo: str, mirror_dir: Path, upstream_url: str) -> str:
        """Make `mirror_dir` present + fresh. Returns 'hit' (served from a fresh
        mirror, no upstream), 'miss' (fetched), 'clone' (first clone), or
        'offline'. Raises NotCached / MirrorError."""
        async with self._lock(repo):
            exists = (mirror_dir / "HEAD").exists()
            if self._is_offline():
                if not exists:
                    raise NotCached(repo)
                return "offline"
            if exists and self.is_fresh(repo):
                return "hit"
            if not exists:
                await self._clone(repo, mirror_dir, upstream_url)
                self._fresh[repo] = time.monotonic()
                return "clone"
            await self._fetch(repo, mirror_dir, upstream_url)
            self._fresh[repo] = time.monotonic()
            return "miss"

    async def _clone(self, repo: str, mirror_dir: Path, upstream_url: str) -> None:
        mirror_dir.parent.mkdir(parents=True, exist_ok=True)
        # init --bare + a heads+tags refspec (not clone --mirror, which grabs
        # refs/pull/*). gc/maintenance disabled up front so no auto-rewrite ever.
        await self._git(["init", "--bare", "-q", str(mirror_dir)])
        cfg = [
            ("remote.origin.url", upstream_url),
            ("gc.auto", "0"),
            ("maintenance.auto", "false"),
            ("uploadpack.allowFilter", "true"),
            ("uploadpack.allowAnySHA1InWant", "true"),
        ]
        for k, v in cfg:
            await self._git(["--git-dir", str(mirror_dir), "config", k, v])
        await self._git(["--git-dir", str(mirror_dir), "config", "remote.origin.fetch", _REFSPECS[0]])
        await self._git(["--git-dir", str(mirror_dir), "config", "--add",
                         "remote.origin.fetch", _REFSPECS[1]])
        await self._fetch(repo, mirror_dir, upstream_url, first=True)

    async def _fetch(self, repo: str, mirror_dir: Path, upstream_url: str, first: bool = False) -> None:
        size_before = 0 if first else _dir_size(mirror_dir)
        dl_id = f"git/{repo}"
        name = f"{repo} ({'clone' if first else 'fetch'})"
        self._progress.start(dl_id, name, None)
        t0 = time.monotonic()
        try:
            await self._git(
                ["-c", "credential.helper=", "--git-dir", str(mirror_dir), "fetch",
                 "--progress", "--prune", "--no-write-fetch-head",
                 "--no-auto-maintenance", "--atomic", "origin"],
                progress_id=dl_id, progress_name=name,
            )
            await self._sync_head(mirror_dir, upstream_url)
            await self._sync_ledger(repo, mirror_dir)
            self._progress.complete(dl_id)
        except Exception:
            self._progress.error(dl_id)
            raise
        # Passive bandwidth sample: bytes added to the mirror ≈ bytes fetched.
        elapsed = time.monotonic() - t0
        delta = _dir_size(mirror_dir) - size_before
        if self._stats is not None and delta >= (2 << 20) and elapsed > 0:
            self._stats.bandwidth(delta / elapsed, source="passive")
            self._stats.traffic("git", hit=False, nbytes=delta)

    async def _sync_head(self, mirror_dir: Path, upstream_url: str) -> None:
        """Point the mirror's HEAD at upstream's default branch (fetch never does).
        Best-effort: a mirror with a stale HEAD still serves, just picks a wrong
        default branch on fresh clones."""
        out = await self._git(
            ["--git-dir", str(mirror_dir), "ls-remote", "--symref", upstream_url, "HEAD"],
            capture=True, check=False,
        )
        m = re.search(r"^ref:\s+(\S+)\s+HEAD", out or "", re.M)
        if m:
            await self._git(["--git-dir", str(mirror_dir), "symbolic-ref", "HEAD", m.group(1)],
                            check=False)

    async def _sync_ledger(self, repo: str, mirror_dir: Path) -> None:
        out = await self._git(
            ["--git-dir", str(mirror_dir), "for-each-ref",
             "--format=%(refname:short) %(objectname)", "refs/heads", "refs/tags"],
            capture=True, check=False,
        )
        entries: list[tuple[str, str]] = []
        for line in (out or "").splitlines():
            parts = line.split()
            if len(parts) == 2:
                entries.append((parts[0], parts[1]))
        head = await self._git(
            ["--git-dir", str(mirror_dir), "symbolic-ref", "--short", "-q", "HEAD"],
            capture=True, check=False,
        )
        head_ref = (head or "").strip() or None
        await self._ledger.async_sync_git_refs(repo, entries, head_ref, _dir_size(mirror_dir))

    # ---- serve upload-pack --------------------------------------------------
    async def upload_pack(self, mirror_dir: Path, body: bytes | None, git_protocol: str | None,
                          advertise: bool):
        """Async generator streaming `git upload-pack` output. advertise=True is the
        info/refs GET (--advertise-refs, cheap); otherwise it's the fetch POST (pack
        computation — bounded by the semaphore). Kills the subprocess if the client
        disconnects so an aborted clone doesn't burn CPU building a huge pack."""
        # NB: no uploadpack.allowRefInWant — its `wanted-refs` section, combined with
        # `shallow-info`, breaks protocol-v2 shallow clients ("expected 'packfile',
        # received 'shallow-info'"). ref-in-want is optional; clients fall back to
        # `want <sha>` after ls-refs, which works for filters and SHA pins alike.
        args = [
            "-c", "uploadpack.allowFilter=true",
            "-c", "uploadpack.allowAnySHA1InWant=true",
            "upload-pack", "--stateless-rpc",
        ]
        if advertise:
            args.append("--advertise-refs")
        args.append(str(mirror_dir))
        env = dict(self._env)
        if git_protocol:
            env["GIT_PROTOCOL"] = git_protocol

        if advertise:
            async for chunk in self._pump(args, env, body):
                yield chunk
        else:
            async with self._sem:
                async for chunk in self._pump(args, env, body):
                    yield chunk

    async def _pump(self, args, env, body):
        proc = await asyncio.create_subprocess_exec(
            "git", *args, stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE, env=env,
        )
        stderr_task = asyncio.create_task(_drain(proc.stderr))
        try:
            if body is not None:
                proc.stdin.write(body)
                await proc.stdin.drain()
            proc.stdin.close()
            while True:
                chunk = await proc.stdout.read(_STREAM_CHUNK)
                if not chunk:
                    break
                yield chunk
            await proc.wait()
        finally:
            if proc.returncode is None:  # client disconnect / cancellation
                with _suppress():
                    proc.kill()
                await proc.wait()
            stderr_task.cancel()
            with _suppress():
                await stderr_task

    # ---- checkpoint maintenance ---------------------------------------------
    async def maintain(self, repo: str, mirror_dir: Path) -> None:
        """Geometric repack + pack-refs under the repo lock — the ONE deliberate
        file rewrite per checkpoint. Geometric mode rolls only small/recent packs
        together (churn ∝ recent activity), so the DVC delta stays small."""
        async with self._lock(repo):
            await self._git(["--git-dir", str(mirror_dir), "repack", "-d",
                             "--geometric=2", "--write-midx"], check=False)
            await self._git(["--git-dir", str(mirror_dir), "pack-refs", "--all"], check=False)

    # ---- subprocess helper --------------------------------------------------
    async def _git(self, args, *, capture: bool = False, check: bool = True,
                   progress_id: str | None = None, progress_name: str | None = None) -> str | None:
        proc = await asyncio.create_subprocess_exec(
            "git", *args, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
            env=self._env,
        )
        # Drain both pipes concurrently (never let one fill and deadlock the child).
        out_task = asyncio.create_task(proc.stdout.read())
        err_task = asyncio.create_task(self._read_err(proc.stderr, progress_id, progress_name))
        await proc.wait()
        stdout = await out_task
        err = await err_task
        if check and proc.returncode != 0:
            raise MirrorError(f"git {' '.join(args[:3])}… exited {proc.returncode}: {err[-400:]}")
        return stdout.decode("utf-8", "replace") if capture else None

    async def _read_err(self, stderr, progress_id, progress_name) -> str:
        buf = b""
        tail = b""
        while True:
            chunk = await stderr.read(4096)
            if not chunk:
                break
            tail = (tail + chunk)[-1024:]
            if progress_id:
                buf += chunk
                parts = re.split(rb"[\r\n]", buf)
                buf = parts.pop()
                for line in parts:
                    self._parse_progress(line.decode("utf-8", "replace"), progress_id, progress_name)
        return tail.decode("utf-8", "replace")

    def _parse_progress(self, line: str, dl_id: str, name: str | None) -> None:
        m = _PROGRESS_RE.search(line)
        if not m:
            return
        phase, _pct, cur, total = m.group(1).strip(), m.group(2), int(m.group(3)), int(m.group(4))
        # "Receiving objects" is the dominant, client-visible phase.
        if phase in ("Receiving objects", "Resolving deltas", "Counting objects"):
            self._progress.start(dl_id, name or dl_id, total)
            self._progress.update(dl_id, cur)


def _build_env() -> dict:
    """Frozen non-interactive env for every git subprocess. Never prompts (which
    would hang a coroutine holding the repo lock); trusts public CAs via the system
    store (ca-certificates in the image). Never disables TLS verification."""
    env = {
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_ASKPASS": "/bin/true",
        "GIT_CONFIG_GLOBAL": "/dev/null",   # isolate from a service-account ~/.gitconfig
        "HOME": os.environ.get("HOME", "/tmp"),
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
        "LANG": "C.UTF-8",
    }
    for k in ("https_proxy", "http_proxy", "no_proxy", "HTTPS_PROXY", "HTTP_PROXY",
              "NO_PROXY", "GIT_SSL_CAINFO", "SSL_CERT_FILE", "SSL_CERT_DIR"):
        if k in os.environ:
            env[k] = os.environ[k]
    return env


async def _drain(stream) -> None:
    while True:
        if not await stream.read(4096):
            return


def _dir_size(path: Path) -> int:
    total = 0
    for root, _dirs, files in os.walk(path, onerror=lambda _e: None):
        for f in files:
            try:
                total += os.stat(os.path.join(root, f)).st_size
            except OSError:
                pass
    return total


class _suppress:
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return True
