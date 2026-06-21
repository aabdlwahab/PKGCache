"""npm pull-through — replaces Verdaccio.

Fetch the packument from the uplink, rewrite every versions.*.dist.tarball to point
back at this proxy (so tarballs are fetched through us), cache the raw upstream doc,
and serve. Tarballs stream through the shared single-flight cache.
"""
from __future__ import annotations

import json
from collections.abc import Iterable
from pathlib import Path
from urllib.parse import unquote

import httpx
from starlette.requests import Request
from starlette.responses import JSONResponse, PlainTextResponse, Response
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.ledger import ArtifactRecord
from .common import external_base


class NpmRepo:
    role = "npm"
    progress_path = "/-/progress"

    def client_endpoint(self, host: str) -> str:
        return f"https://{host}:4873/"

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        self._upstream = (core.config.upstream or "https://registry.npmjs.org").rstrip("/")
        return [
            Route("/@{scope}/{pkg}/-/{filename}", self.tarball, methods=["GET", "HEAD"]),
            Route("/{pkg}/-/{filename}", self.tarball, methods=["GET", "HEAD"]),
            Route("/@{scope}/{pkg}", self.metadata, methods=["GET"]),
            Route("/{pkg}", self.metadata, methods=["GET"]),
        ]

    def _pkgname(self, request: Request) -> str:
        scope = request.path_params.get("scope")
        pkg = unquote(request.path_params["pkg"])
        return f"@{scope}/{pkg}" if scope else pkg

    # ---- packument -----------------------------------------------------------
    async def metadata(self, request: Request) -> Response:
        name = self._pkgname(request)
        doc = await self._load_doc(name)
        if doc is None:
            self._core.progress.record_recent(name, name, None, hit=False, failed=True)
            return PlainTextResponse(f"no cached metadata for {name}", status_code=404)
        ext = external_base(request)
        for meta in (doc.get("versions") or {}).values():
            dist = meta.get("dist") or {}
            tb = dist.get("tarball")
            if tb:
                dist["tarball"] = f"{ext}/{name}/-/{tb.rsplit('/', 1)[-1]}"
        return JSONResponse(doc)

    async def _load_doc(self, name: str) -> dict | None:
        cache_file = self._core.storage.safe_path(name, "metadata.json")
        if not self._core.config.offline:
            try:
                r = await self._core.upstream.client.get(f"{self._upstream}/{name}")
                if r.status_code == 200:
                    cache_file.parent.mkdir(parents=True, exist_ok=True)
                    tmp = cache_file.with_suffix(".json.part")
                    tmp.write_bytes(r.content)
                    tmp.replace(cache_file)
                    return json.loads(r.content)
            except httpx.HTTPError:
                pass  # upstream unreachable/errored — fall back to the cached doc below
        if cache_file.exists():
            return json.loads(cache_file.read_text())
        return None

    # ---- tarball -------------------------------------------------------------
    async def tarball(self, request: Request) -> Response:
        name = self._pkgname(request)
        filename = request.path_params["filename"]
        doc = await self._load_doc(name)
        version, url, dist = _find_tarball(doc, filename) if doc else (None, None, None)
        if url is None:
            self._core.progress.record_recent(filename, filename, None, hit=False, failed=True)
            return PlainTextResponse(f"unknown tarball {filename}", status_code=404)

        final_path = self._core.storage.safe_path(name, "-", filename)

        def on_commit(size: int, hexd: str):
            return ArtifactRecord(
                ecosystem="npm", name=name, version=version or "",
                digest=f"sha256:{hexd}", size=size, origin=url,
                path=str(final_path.relative_to(self._core.storage.root)),
                extra={k: dist.get(k) for k in ("shasum", "integrity") if dist.get(k)} or None,
            )

        client = self._core.upstream.client

        def opener():
            return client.stream("GET", url)

        return await self._core.cache.fetch(
            key=f"npm/{name}/-/{filename}",
            final_path=final_path,
            stream_opener=opener,
            name=filename,
            method=request.method,
            request=request,
            media_type="application/octet-stream",
            on_commit=on_commit,
        )

    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        for tgz in sorted(cache_dir.glob("**/-/*.tgz")):
            # <pkgdir>/-/<file>.tgz ; pkg name is the dir tree above '-'
            pkg_parts = tgz.relative_to(cache_dir).parts[:-2]
            name = "/".join(pkg_parts)
            stem = tgz.name[: -len(".tgz")]
            version = stem.rsplit("-", 1)[-1] if "-" in stem else ""
            yield ArtifactRecord(
                ecosystem="npm", name=name, version=version, size=tgz.stat().st_size,
                path=str(tgz.relative_to(cache_dir)),
            )


def _find_tarball(doc: dict, filename: str) -> tuple[str | None, str | None, dict | None]:
    for version, meta in (doc.get("versions") or {}).items():
        dist = meta.get("dist") or {}
        tb = dist.get("tarball")
        if tb and tb.rsplit("/", 1)[-1] == filename:
            return version, tb, dist
    return None, None, None
