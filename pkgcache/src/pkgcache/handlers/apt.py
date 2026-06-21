"""apt + apk pull-through — replaces apt-cacher-ng.

This is a FORWARD proxy: apt sets Acquire::http::Proxy and apk sets http_proxy, so
requests arrive in absolute form (GET http://archive.ubuntu.com/...). We reconstruct
the upstream URL from the Host header + path (parser-agnostic), cache by host/path,
and classify files:
  * volatile (InRelease/Release*/Packages*/Sources*/Contents*/APKINDEX) — revalidate
    online via ETag/Last-Modified; serve cache on 304 or when offline.
  * immutable (*.deb/*.udeb/*.apk, pool/*, by-hash/*) — cache forever.

Decision: proxies to ANY host (no allowlist) — acceptable on the trusted/isolated
networks this stack targets. Stays plain HTTP on :3142 (no TLS, no CONNECT).
"""
from __future__ import annotations

import json
import urllib.parse
from collections.abc import Iterable
from pathlib import Path

import httpx
from starlette.requests import Request
from starlette.responses import FileResponse, PlainTextResponse, Response
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.ledger import ArtifactRecord

_VOLATILE_EXACT = {"InRelease", "Release", "Release.gpg", "APKINDEX.tar.gz"}
_VOLATILE_PREFIX = ("Packages", "Sources", "Contents")


class AptRepo:
    role = "apt"
    progress_path = "/acng-progress"

    def client_endpoint(self, host: str) -> str:
        return f"http://{host}:3142   (apt: Acquire::http::Proxy; apk: http_proxy)"

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        return [Route("/{path:path}", self.proxy, methods=["GET", "HEAD"])]

    async def proxy(self, request: Request) -> Response:
        target = _reconstruct_target(request)
        if target is None:
            return PlainTextResponse("forward-proxy requires absolute target / Host", status_code=400)
        u = urllib.parse.urlparse(target)
        if not u.netloc:
            return PlainTextResponse("bad target", status_code=400)

        final = self._core.storage.safe_path(u.netloc, u.path)
        filename = u.path.rsplit("/", 1)[-1]
        head = request.method == "HEAD"

        if _is_volatile(filename):
            return await self._serve_volatile(target, final, head)
        return await self._serve_immutable(target, final, filename, request)

    # ---- volatile (always revalidate online) --------------------------------
    async def _serve_volatile(self, target: str, final: Path, head: bool) -> Response:
        meta_path = final.with_name(final.name + ".meta")
        if self._core.config.offline:
            if final.exists():
                self._core.progress.record_recent(target, final.name, final.stat().st_size, hit=True)
                return self._file(final, head)
            self._core.progress.record_recent(target, final.name, None, hit=False, failed=True)
            return PlainTextResponse("not cached (offline)", status_code=404)

        headers = {}
        if final.exists() and meta_path.exists():
            try:
                m = json.loads(meta_path.read_text())
                if m.get("etag"):
                    headers["If-None-Match"] = m["etag"]
                if m.get("last_modified"):
                    headers["If-Modified-Since"] = m["last_modified"]
            except (ValueError, OSError):
                pass  # unreadable/corrupt .meta — revalidate unconditionally below
        try:
            async with self._core.upstream.client.stream("GET", target, headers=headers) as r:
                if r.status_code == 304 and final.exists():
                    self._core.progress.record_recent(target, final.name, final.stat().st_size, hit=True)
                    return self._file(final, head)
                if r.status_code != 200:
                    if final.exists():
                        return self._file(final, head)
                    return Response(status_code=r.status_code)
                self._core.progress.start(target, final.name,
                                          int(r.headers["content-length"]) if "content-length" in r.headers else None)
                tmp, f = self._core.storage.open_part(final)
                written = 0
                with f:
                    async for chunk in r.aiter_bytes(1 << 16):
                        f.write(chunk)
                        written += len(chunk)
                        self._core.progress.update(target, written)
                self._core.storage.commit_part(tmp, final)
                meta_path.write_text(json.dumps({
                    "etag": r.headers.get("etag"),
                    "last_modified": r.headers.get("last-modified"),
                }))
                self._core.progress.complete(target)
                self._core.progress.record_recent(target, final.name, written, hit=False)
                return self._file(final, head)
        except httpx.HTTPError:
            if final.exists():
                return self._file(final, head)
            return PlainTextResponse("upstream unreachable", status_code=502)

    # ---- immutable (cache forever, single-flight) ---------------------------
    async def _serve_immutable(self, target, final, filename, request) -> Response:
        if self._core.config.offline and not final.exists():
            self._core.progress.record_recent(target, final.name, None, hit=False, failed=True)
            return PlainTextResponse("not cached (offline)", status_code=404)

        rel = str(final.relative_to(self._core.storage.root))

        def on_commit(size: int, hexd: str):
            rec = _artifact_for(filename)
            if rec is None:
                return None  # immutable but not a package (e.g. by-hash index blob)
            eco, name, version = rec
            return ArtifactRecord(
                ecosystem=eco, name=name, version=version,
                digest=f"sha256:{hexd}", size=size, origin=target, path=rel,
            )

        client = self._core.upstream.client

        def opener():
            return client.stream("GET", target)

        return await self._core.cache.fetch(
            key=f"apt/{final.relative_to(self._core.storage.root)}",
            final_path=final,
            stream_opener=opener,
            name=filename,
            method=request.method,
            request=request,
            on_commit=on_commit,
        )

    @staticmethod
    def _file(path: Path, head: bool) -> Response:
        if head:
            return Response(status_code=200, headers={
                "Content-Length": str(path.stat().st_size), "Accept-Ranges": "bytes",
            })
        return FileResponse(path)

    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        for pat in ("**/*.deb", "**/*.udeb", "**/*.apk"):
            for f in sorted(cache_dir.glob(pat)):
                rec = _artifact_for(f.name)
                if rec is None:
                    continue
                eco, name, version = rec
                yield ArtifactRecord(
                    ecosystem=eco, name=name, version=version, size=f.stat().st_size,
                    path=str(f.relative_to(cache_dir)),
                )


def _reconstruct_target(request: Request) -> str | None:
    raw = (request.scope.get("raw_path") or b"").decode("latin-1")
    if raw.startswith(("http://", "https://")):
        target = raw
    else:
        host = request.headers.get("host")
        if not host:
            return None
        target = f"http://{host}{request.url.path}"
    if request.url.query:
        target += "?" + request.url.query
    return target


def _is_volatile(filename: str) -> bool:
    return filename in _VOLATILE_EXACT or filename.startswith(_VOLATILE_PREFIX)


def _artifact_for(filename: str) -> tuple[str, str, str] | None:
    fn = urllib.parse.unquote(filename)
    if fn.endswith(".apk"):
        stem = fn[: -len(".apk")]
        parts = stem.rsplit("-", 2)
        if len(parts) == 3:
            return "apk", parts[0], f"{parts[1]}-{parts[2]}"
        return "apk", stem, ""
    for suf in (".deb", ".udeb"):
        if fn.endswith(suf):
            stem = fn[: -len(suf)]
            parts = stem.split("_")
            name = parts[0] if parts else stem
            version = parts[1] if len(parts) > 1 else ""
            return "apt", name, version
    return None
