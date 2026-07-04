"""git pull-through — mirror-and-serve smart HTTP.

Unlike the byte-cached ecosystems, a git fetch is a negotiation (the client posts
its have/want set; the server computes a bespoke packfile), so we keep a real bare
mirror on disk (see core/gitmirror.py) and run `git upload-pack` against it.

Client URL: https://<cache>:3143/<upstream-host>/<owner>/<repo>.git — the first
path segment is the real upstream host (any public https host). Transparent
adoption:
    git config --global url."https://<cache>:3143/github.com/".insteadOf "https://github.com/"

Read-only: push (git-receive-pack) is always refused. Anonymous/public repos only.
"""
from __future__ import annotations

import gzip
import re
import subprocess
from collections.abc import Iterable
from pathlib import Path

from starlette.requests import Request
from starlette.responses import JSONResponse, PlainTextResponse, Response, StreamingResponse
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.gitmirror import MirrorError, MirrorManager, NotCached, _dir_size
from ..core.ledger import ArtifactRecord
from ..core.storage import UnsafePath
from .common import external_base

_LFS_CT = "application/vnd.git-lfs+json"
_OID_RE = re.compile(r"^[0-9a-f]{64}$")   # LFS oids are sha256 hex

# Prepended to the info/refs advertisement (even for protocol v2, which skips it).
# 0x1e = len("# service=git-upload-pack\n") + 4.
_PKT_ADVERT = b"001e# service=git-upload-pack\n0000"
_NOCACHE = {
    "Cache-Control": "no-cache, max-age=0, must-revalidate",
    "Pragma": "no-cache",
    "Expires": "Fri, 01 Jan 1980 00:00:00 GMT",
}
_MAX_BODY = 64 << 20  # buffer cap on the upload-pack POST negotiation body
_HOST_RE = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$", re.I)


class GitRepo:
    role = "git"
    progress_path = "/+progress"

    def client_endpoint(self, host: str) -> str:
        return (f"https://{host}:3143/<upstream-host>/<owner>/<repo>.git   "
                f'(git config --global url."https://{host}:3143/github.com/".insteadOf '
                f'"https://github.com/")')

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        cfg = core.config
        self._mirror = MirrorManager(
            storage=core.storage, ledger=core.ledger, progress=core.progress,
            stats=core.stats, offline=cfg.offline,
            refs_ttl=cfg.refs_ttl, max_upload_packs=cfg.max_upload_packs,
        )
        self._lfs: dict[str, dict] = {}  # oid -> {href, header} from the last batch (P2)
        return [
            Route("/+maintain", self.maintain, methods=["POST"]),
            Route("/+lfs/{oid}", self.lfs_get, methods=["GET"]),
            Route("/{path:path}/info/lfs/objects/batch", self.lfs_batch, methods=["POST"]),
            Route("/{path:path}/info/refs", self.info_refs, methods=["GET"]),
            Route("/{path:path}/git-upload-pack", self.upload_pack, methods=["POST"]),
            Route("/{path:path}/git-receive-pack", self.receive_pack, methods=["GET", "POST"]),
            Route("/{path:path}", self.dumb, methods=["GET"]),  # dumb-protocol probes → clean 404
        ]

    # ---- helpers -------------------------------------------------------------
    def _resolve(self, path: str):
        """(repo, mirror_dir, upstream_url) from a request path, or None if invalid.

        repo = "host/owner/name" (canonical, no .git); mirror = caches/git/<repo>.git."""
        path = path.strip("/")
        if path.endswith(".git"):
            path = path[:-4]
        segs = [s for s in path.split("/") if s]
        if len(segs) < 2 or not _HOST_RE.match(segs[0]):
            return None
        try:
            mirror = self._core.storage.safe_path(*segs[:-1], segs[-1] + ".git")
        except UnsafePath:
            return None
        return "/".join(segs), mirror, f"https://{'/'.join(segs)}.git"

    # ---- routes --------------------------------------------------------------
    async def info_refs(self, request: Request) -> Response:
        service = request.query_params.get("service")
        if service == "git-receive-pack":
            return self._refuse_push()
        if service != "git-upload-pack":
            return PlainTextResponse("dumb HTTP protocol is not supported", status_code=404)
        resolved = self._resolve(request.path_params["path"])
        if resolved is None:
            return PlainTextResponse("not a valid repository path", status_code=404)
        repo, mirror, upstream = resolved
        self._core.stats.access("git", repo)
        try:
            await self._mirror.ensure(repo, mirror, upstream)
        except NotCached:
            return PlainTextResponse("repository not cached (offline)", status_code=404)
        except MirrorError as exc:
            return PlainTextResponse(f"upstream clone/fetch failed: {exc}", status_code=502)

        git_protocol = request.headers.get("git-protocol")

        async def gen():
            yield _PKT_ADVERT
            async for chunk in self._mirror.upload_pack(mirror, None, git_protocol, advertise=True):
                yield chunk

        return StreamingResponse(gen(), media_type="application/x-git-upload-pack-advertisement",
                                 headers=dict(_NOCACHE))

    async def upload_pack(self, request: Request) -> Response:
        resolved = self._resolve(request.path_params["path"])
        if resolved is None:
            return PlainTextResponse("not a valid repository path", status_code=404)
        repo, mirror, upstream = resolved

        clen = request.headers.get("content-length")
        if clen and clen.isdigit() and int(clen) > _MAX_BODY:
            return PlainTextResponse("negotiation body too large", status_code=413)
        body = await request.body()
        if len(body) > _MAX_BODY:
            return PlainTextResponse("negotiation body too large", status_code=413)
        if request.headers.get("content-encoding", "").lower() == "gzip":
            body = gzip.decompress(body)

        # A POST usually follows a fresh info/refs; if the mirror somehow isn't here
        # (direct POST), materialize it now.
        if not (mirror / "HEAD").exists():
            try:
                await self._mirror.ensure(repo, mirror, upstream)
            except NotCached:
                return PlainTextResponse("repository not cached (offline)", status_code=404)
            except MirrorError as exc:
                return PlainTextResponse(f"upstream clone/fetch failed: {exc}", status_code=502)

        git_protocol = request.headers.get("git-protocol")
        hit = self._mirror.is_fresh(repo)
        served = {"n": 0}

        async def gen():
            async for chunk in self._mirror.upload_pack(mirror, body, git_protocol, advertise=False):
                served["n"] += len(chunk)
                yield chunk
            # "bytes served from cache" on a hit → feeds bytes-saved / time-saved.
            self._core.stats.traffic("git", hit=hit, nbytes=served["n"])
            self._core.progress.record_recent(f"git/{repo}", repo, served["n"], hit=hit)

        return StreamingResponse(gen(), media_type="application/x-git-upload-pack-result",
                                 headers=dict(_NOCACHE))

    async def receive_pack(self, request: Request) -> Response:
        return self._refuse_push()

    async def dumb(self, request: Request) -> Response:
        return PlainTextResponse(
            "dumb HTTP git protocol is not supported; this is a smart-HTTP mirror",
            status_code=404)

    async def maintain(self, request: Request) -> Response:
        """Internal: geometric-repack every mirror (called at checkpoint, before the
        DVC snapshot, so the one deliberate file rewrite lands in that commit)."""
        root = self._core.storage.root
        maintained = 0
        for head in sorted(root.glob("**/HEAD")):
            mirror = head.parent
            if mirror.suffix != ".git":
                continue
            repo = str(mirror.relative_to(root))
            repo = repo[:-4] if repo.endswith(".git") else repo
            try:
                await self._mirror.maintain(repo, mirror)
                maintained += 1
            except Exception:  # noqa: BLE001 — one bad mirror shouldn't abort the rest
                pass
        return JSONResponse({"maintained": maintained})

    # ---- Git LFS (phase 2) --------------------------------------------------
    # LFS objects are sha256-addressed blobs, so they reuse the shared CAS +
    # single-flight + Range machinery. git-lfs derives this endpoint from the clone
    # URL, so it lands here automatically when cloning through the cache.
    async def lfs_batch(self, request: Request) -> Response:
        resolved = self._resolve(request.path_params["path"])
        if resolved is None:
            return JSONResponse({"message": "not a valid repository path"},
                                status_code=404, media_type=_LFS_CT)
        _repo, _mirror, upstream_url = resolved
        try:
            payload = await request.json()
        except (ValueError, UnicodeDecodeError):
            return JSONResponse({"message": "invalid JSON"}, status_code=400, media_type=_LFS_CT)
        if payload.get("operation") != "download":
            return JSONResponse({"message": "read-only mirror: LFS upload is not supported"},
                                status_code=403, media_type=_LFS_CT)

        ext = external_base(request)
        result, to_forward = [], []
        for o in payload.get("objects", []) or []:
            oid, size = o.get("oid"), o.get("size")
            if not (isinstance(oid, str) and _OID_RE.match(oid)):
                result.append({"oid": oid, "size": size,
                               "error": {"code": 422, "message": "invalid oid"}})
            elif self._core.storage.blob_path(oid).exists():
                result.append(_lfs_action(oid, size, ext))  # already cached → our href
            elif self._core.config.offline:
                result.append({"oid": oid, "size": size,
                               "error": {"code": 404, "message": "not cached (offline)"}})
            else:
                to_forward.append((oid, size))

        if to_forward:
            hrefs = await self._forward_lfs_batch(upstream_url, [o for o, _ in to_forward])
            for oid, size in to_forward:
                if oid in hrefs:
                    self._lfs[oid] = hrefs[oid]           # remember upstream signed URL for the GET
                    result.append(_lfs_action(oid, size, ext))
                else:
                    result.append({"oid": oid, "size": size,
                                   "error": {"code": 404, "message": "no such object upstream"}})
        return JSONResponse({"transfer": "basic", "objects": result}, media_type=_LFS_CT)

    async def _forward_lfs_batch(self, upstream_url: str, oids: list[str]) -> dict[str, dict]:
        """Ask the upstream LFS server for signed download URLs for the given oids."""
        body = {"operation": "download", "transfers": ["basic"],
                "objects": [{"oid": o} for o in oids]}
        try:
            r = await self._core.upstream.client.post(
                f"{upstream_url}/info/lfs/objects/batch", json=body,
                headers={"Accept": _LFS_CT, "Content-Type": _LFS_CT})
            if r.status_code != 200:
                return {}
            data = r.json()
        except Exception:  # noqa: BLE001 — upstream LFS unreachable → no hrefs
            return {}
        out = {}
        for o in data.get("objects", []) or []:
            dl = (o.get("actions") or {}).get("download") or {}
            if dl.get("href"):
                out[o.get("oid")] = {"href": dl["href"], "header": dl.get("header") or {}}
        return out

    async def lfs_get(self, request: Request) -> Response:
        oid = request.path_params["oid"]
        if not _OID_RE.match(oid):
            return PlainTextResponse("invalid oid", status_code=404)
        final = self._core.storage.blob_path(oid)
        if self._core.config.offline and not final.exists():
            return PlainTextResponse("not cached (offline)", status_code=404)
        info = self._lfs.get(oid)
        if not final.exists() and info is None:
            return PlainTextResponse("unknown LFS object (request a batch first)", status_code=404)

        client = self._core.upstream.client
        href = info["href"] if info else ""
        headers = info["header"] if info else {}

        def opener():
            return client.stream("GET", href, headers=headers)

        def on_commit(size: int, hexd: str):
            return ArtifactRecord(ecosystem="git", name="(lfs)", version=oid[:12],
                                  digest=f"sha256:{hexd}", size=size, extra={"lfs": True})

        self._core.stats.access("git", "(lfs)")
        return await self._core.cache.fetch(
            key=f"lfs/{oid}", final_path=final, stream_opener=opener,
            name=f"lfs:{oid[:12]}", method=request.method, request=request,
            expected_sha256=oid, eco="git", on_commit=on_commit,
        )

    def _refuse_push(self) -> Response:
        body = _pkt(b"ERR read-only mirror: push (git-receive-pack) is not supported\n")
        return Response(body, status_code=403,
                        media_type="application/x-git-receive-pack-result", headers=dict(_NOCACHE))

    # ---- rebuild (repair path for gen_manifest.py --rebuild) ----------------
    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        for head in sorted(cache_dir.glob("**/HEAD")):
            mirror = head.parent
            if mirror.suffix != ".git":
                continue
            repo = str(mirror.relative_to(cache_dir))
            repo = repo[:-4] if repo.endswith(".git") else repo
            try:
                out = subprocess.run(
                    ["git", "--git-dir", str(mirror), "for-each-ref",
                     "--format=%(refname:short) %(objectname)", "refs/heads", "refs/tags"],
                    capture_output=True, text=True, timeout=30).stdout
                head_ref = subprocess.run(
                    ["git", "--git-dir", str(mirror), "symbolic-ref", "--short", "-q", "HEAD"],
                    capture_output=True, text=True, timeout=10).stdout.strip()
            except (OSError, subprocess.SubprocessError):
                continue
            size = _dir_size(mirror)
            for line in out.splitlines():
                parts = line.split()
                if len(parts) == 2:
                    ref, sha = parts
                    yield ArtifactRecord(ecosystem="git", name=repo, version=ref, digest=sha,
                                         size=(size if ref == head_ref else None))


def _pkt(data: bytes) -> bytes:
    return f"{len(data) + 4:04x}".encode() + data


def _lfs_action(oid: str, size, ext: str) -> dict:
    """An LFS batch 'download' action pointing the client back at this cache."""
    return {"oid": oid, "size": size,
            "actions": {"download": {"href": f"{ext}/+lfs/{oid}"}}}
