"""files — a generic artifact store (the first write path).

Path-addressed files, downloadable with plain wget/curl and uploaded via PUT (by
CI with the per-project write token, or by the console which proxies the upload).
Reuses the shared storage atomics + ledger + progress; adds the write side.

Semantics:
  * GET/HEAD  — anonymous; Range/resume free (FileResponse); a directory renders an
    HTML autoindex so browsers and `wget -r` work.
  * PUT       — token-gated, ONLINE-ONLY, write-once (?overwrite=1 to replace),
    optional X-Checksum-Sha256 verification; sha256 returned.
  * DELETE    — token-gated, online-only; removes a file (+ its ledger row).

Writes are refused when OFFLINE=1: the air-gapped side is serve-only (an upload
there would be wiped by the next import's `dvc checkout` or block its fast-forward).
"""
from __future__ import annotations

import hashlib
import hmac
import html
import mimetypes
import time
from collections.abc import Iterable
from pathlib import Path

from starlette.requests import ClientDisconnect, Request
from starlette.responses import HTMLResponse, JSONResponse, PlainTextResponse, Response
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.config import files_token
from ..core.ledger import ArtifactRecord
from ..core.storage import UnsafePath
from .common import external_base

_CHUNK = 1 << 16
# Names the role owns under its cache root — never writable/deletable via the API.
_RESERVED = ("ledger.db", "ledger.db-wal", "ledger.db-shm")


class FilesRepo:
    role = "files"
    progress_path = "/+progress"

    def client_endpoint(self, host: str) -> str:
        return (f"https://{host}:3144/<path>   "
                f"(wget --ca-certificate=ca.crt; PUT/DELETE need the write token)")

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        self._max_bytes = int(core.config.max_upload_mb) * 1024 * 1024
        return [Route("/{path:path}", self.dispatch, methods=["GET", "HEAD", "PUT", "DELETE"])]

    async def dispatch(self, request: Request) -> Response:
        method = request.method
        if method in ("GET", "HEAD"):
            return await self.get(request)
        if method == "PUT":
            return await self.put(request)
        if method == "DELETE":
            return await self.delete(request)
        return PlainTextResponse("method not allowed", status_code=405)

    # ---- resolve + guards ----------------------------------------------------
    def _resolve(self, raw: str):
        """(parts, target_path) under the role's cache root, or None if unsafe."""
        parts = [p for p in raw.strip("/").split("/") if p and p not in (".", "..")]
        try:
            target = self._core.storage.safe_path(*parts) if parts else self._core.storage.root
        except UnsafePath:
            return None
        return parts, target

    @staticmethod
    def _reserved(parts: list[str]) -> bool:
        """Reject writes that would clobber the role's own files or the endpoint
        namespace: the ledger DB, any .part temp, or a leading '+' segment."""
        if not parts:
            return True
        if parts[0].startswith("+"):
            return True
        return any(p in _RESERVED or p.endswith(".part") for p in parts)

    def _authorize(self, request: Request) -> Response | None:
        """None if the write may proceed, else the rejection response."""
        expected = files_token(self._core.config.project)
        if not expected:
            return PlainTextResponse(
                "no write token set for this project — generate one in the console",
                status_code=403)
        auth = request.headers.get("authorization", "")
        token = auth[7:].strip() if auth.lower().startswith("bearer ") else \
            request.headers.get("x-auth-token", "")
        if not (token and hmac.compare_digest(token, expected)):
            return PlainTextResponse("invalid or missing write token", status_code=401)
        if self._core.config.offline:
            return PlainTextResponse(
                "read-only: writes are disabled on the air-gapped (OFFLINE) side",
                status_code=403)
        return None

    # ---- GET / HEAD ----------------------------------------------------------
    async def get(self, request: Request) -> Response:
        resolved = self._resolve(request.path_params["path"])
        if resolved is None:
            return PlainTextResponse("not found", status_code=404)
        parts, target = resolved
        if target.is_dir():
            return self._autoindex(parts, target)
        if target.is_file():
            rel = "/".join(parts)
            if request.method == "GET":
                size = target.stat().st_size
                self._core.stats.access("files", rel)
                self._core.stats.traffic("files", hit=True, nbytes=size)
                self._core.progress.record_recent(f"files/{rel}", rel, size, hit=True)
            media = mimetypes.guess_type(target.name)[0] or "application/octet-stream"
            return self._core.storage.file_response(
                target, media_type=media, method=request.method)
        return PlainTextResponse("not found", status_code=404)

    def _autoindex(self, parts: list[str], target: Path) -> HTMLResponse:
        base = "/" + "/".join(parts)
        if not base.endswith("/"):
            base += "/"
        rows = []
        if parts:  # parent link
            rows.append('<a href="../">../</a>')
        for entry in sorted(target.iterdir(), key=lambda p: (not p.is_dir(), p.name.lower())):
            if entry.name in _RESERVED or entry.name.endswith(".part"):
                continue
            try:
                st = entry.stat()
            except OSError:
                continue
            is_dir = entry.is_dir()
            name = entry.name + ("/" if is_dir else "")
            size = "-" if is_dir else _fmt_size(st.st_size)
            mt = time.strftime("%Y-%m-%d %H:%M", time.gmtime(st.st_mtime))
            rows.append(
                f'<a href="{html.escape(name)}">{html.escape(name)}</a>'
                f'\t{mt}\t{size}')
        body = (
            f"<!DOCTYPE html><html><head><meta charset=utf-8>"
            f"<title>Index of {html.escape(base)}</title></head><body>"
            f"<h1>Index of {html.escape(base)}</h1><pre>\n" + "\n".join(rows) +
            "\n</pre></body></html>\n")
        return HTMLResponse(body)

    # ---- PUT (upload) --------------------------------------------------------
    async def put(self, request: Request) -> Response:
        denied = self._authorize(request)
        if denied is not None:
            return denied
        raw = request.path_params["path"]
        if not raw.strip("/") or raw.endswith("/"):
            return PlainTextResponse("PUT requires a file path", status_code=400)
        resolved = self._resolve(raw)
        if resolved is None:
            return PlainTextResponse("invalid path", status_code=404)
        parts, final = resolved
        if self._reserved(parts):
            return PlainTextResponse("reserved path", status_code=403)
        rel = "/".join(parts)
        overwrite = request.query_params.get("overwrite") in ("1", "true", "yes")
        if final.exists():
            if final.is_dir():
                return PlainTextResponse("a directory exists at that path", status_code=409)
            if not overwrite:
                return PlainTextResponse(
                    "already exists (write-once) — retry with ?overwrite=1 to replace",
                    status_code=409)

        clen = request.headers.get("content-length")
        if self._max_bytes and clen and clen.isdigit() and int(clen) > self._max_bytes:
            return PlainTextResponse(f"too large (max {self._core.config.max_upload_mb} MB)",
                                     status_code=413)

        dl_id = f"files/{rel}"
        total = int(clen) if clen and clen.isdigit() else None
        self._core.progress.start(dl_id, rel, total)
        tmp, fh = self._core.storage.open_part(final)
        h = hashlib.sha256()
        n = 0
        try:
            async for chunk in request.stream():
                n += len(chunk)
                if self._max_bytes and n > self._max_bytes:
                    raise _TooLarge()
                fh.write(chunk)
                h.update(chunk)
                self._core.progress.update(dl_id, n)
            fh.flush()
            fh.close()
        except _TooLarge:
            _cleanup(fh, tmp)
            self._core.progress.error(dl_id)
            return PlainTextResponse(f"too large (max {self._core.config.max_upload_mb} MB)",
                                     status_code=413)
        except ClientDisconnect:
            _cleanup(fh, tmp)
            self._core.progress.error(dl_id)
            return PlainTextResponse("client disconnected", status_code=400)

        hexd = h.hexdigest()
        want = request.headers.get("x-checksum-sha256")
        if want and want.lower() != hexd:
            _cleanup(fh, tmp)
            self._core.progress.error(dl_id)
            return PlainTextResponse("checksum mismatch", status_code=400)

        self._core.storage.commit_part(tmp, final)
        self._core.progress.complete(dl_id)
        await self._core.ledger.adelete_artifact("files", rel)
        await self._core.ledger.arecord(ArtifactRecord(
            ecosystem="files", name=rel, version="", digest=f"sha256:{hexd}",
            size=n, origin=f"upload:{request.client.host if request.client else '?'}",
            path=rel))
        self._core.stats.access("files", rel)
        ext = external_base(request)
        return JSONResponse(
            {"path": rel, "size": n, "sha256": hexd, "url": f"{ext}/{rel}"},
            status_code=200 if overwrite else 201)

    # ---- DELETE --------------------------------------------------------------
    async def delete(self, request: Request) -> Response:
        denied = self._authorize(request)
        if denied is not None:
            return denied
        resolved = self._resolve(request.path_params["path"])
        if resolved is None:
            return PlainTextResponse("invalid path", status_code=404)
        parts, target = resolved
        if self._reserved(parts):
            return PlainTextResponse("reserved path", status_code=403)
        if target.is_dir():
            return PlainTextResponse("refusing to delete a directory", status_code=400)
        if not target.is_file():
            return PlainTextResponse("not found", status_code=404)
        rel = "/".join(parts)
        target.unlink()
        # prune now-empty parent dirs, but never the role root
        parent = target.parent
        root = self._core.storage.root
        while parent != root and parent.is_dir() and not any(parent.iterdir()):
            parent.rmdir()
            parent = parent.parent
        await self._core.ledger.adelete_artifact("files", rel)
        return Response(status_code=204)

    # ---- rebuild (repair path) ----------------------------------------------
    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        for f in sorted(cache_dir.rglob("*")):
            if not f.is_file():
                continue
            rel = str(f.relative_to(cache_dir))
            if f.name in _RESERVED or f.name.endswith(".part"):
                continue
            h = hashlib.sha256()
            with f.open("rb") as fh:
                for chunk in iter(lambda: fh.read(1 << 20), b""):
                    h.update(chunk)
            yield ArtifactRecord(
                ecosystem="files", name=rel, version="", digest=f"sha256:{h.hexdigest()}",
                size=f.stat().st_size, path=rel)


class _TooLarge(Exception):
    pass


def _cleanup(fh, tmp: Path) -> None:
    try:
        fh.close()
    except Exception:  # noqa: BLE001
        pass
    try:
        Path(tmp).unlink()
    except OSError:
        pass


def _fmt_size(n: int) -> str:
    u = ["B", "KB", "MB", "GB", "TB"]
    i = 0
    v = float(n)
    while v >= 1024 and i < len(u) - 1:
        v /= 1024
        i += 1
    return f"{v:.0f}{u[i]}" if i == 0 else f"{v:.1f}{u[i]}"
