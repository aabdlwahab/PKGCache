"""OCI / Docker pull-through — replaces zot.

Multi-upstream: clients pull <host>:5000/<dest>/<image> where dest ∈
{dockerhub, ghcr, quay}. Manifests and blobs are content-addressed in one CAS;
a tag→digest index (oci_tags in the ledger DB) lets the OFFLINE side resolve tags
with no upstream. One service + an OFFLINE flag replaces zot's two-service split.
"""
from __future__ import annotations

import hashlib
import json
import sqlite3
from collections.abc import Iterable
from pathlib import Path

import httpx
from starlette.requests import Request
from starlette.responses import JSONResponse, PlainTextResponse, Response
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.ledger import ArtifactRecord

_MANIFEST_ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.docker.distribution.manifest.v1+json",
])


class OciRepo:
    role = "oci"
    progress_path = "/v2/_progress"

    def client_endpoint(self, host: str) -> str:
        return f"{host}:5000   (pull {host}:5000/{{dockerhub,ghcr,quay}}/<image>)"

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        # child-manifest digest -> (name, tag, index_digest, arch, origin) for every
        # index we serve. A multi-arch tag pull fetches the platform's child manifest
        # by digest right after the index; this lets that digest pull back-fill the
        # tag row's real image size instead of creating a duplicate digest-keyed row.
        self._pending: dict[str, tuple[str, str, str, str | None, str]] = {}
        return [
            Route("/v2/", self.version, methods=["GET", "HEAD"]),
            Route("/v2/{name:path}/manifests/{ref}", self.manifest, methods=["GET", "HEAD"]),
            Route("/v2/{name:path}/blobs/{digest}", self.blob, methods=["GET", "HEAD"]),
            Route("/v2/{name:path}/tags/list", self.tags_list, methods=["GET"]),
        ]

    # ---- /v2/ version check --------------------------------------------------
    async def version(self, request: Request) -> Response:
        return Response(status_code=200, headers={"Docker-Distribution-API-Version": "registry/2.0"})

    # ---- manifests -----------------------------------------------------------
    async def manifest(self, request: Request) -> Response:
        routed = self._route(request.path_params["name"])
        if routed is None:
            return PlainTextResponse("unknown registry prefix", status_code=404)
        base, dest, repo = routed
        name = request.path_params["name"]
        ref = request.path_params["ref"]
        head = request.method == "HEAD"

        if ref.startswith("sha256:"):
            return await self._manifest_by_digest(base, repo, name, ref, head)

        # tag
        if self._core.config.offline:
            row = await self._core.ledger.aget_tag(dest, repo, ref)
            if row is None:
                self._core.progress.record_recent(name, f"{name}:{ref}", None, hit=False, failed=True)
                return PlainTextResponse("tag not cached", status_code=404)
            path = self._core.storage.blob_path(row["digest"])
            if not path.exists():
                self._core.progress.record_recent(name, f"{name}:{ref}", None, hit=False, failed=True)
                return PlainTextResponse("manifest blob missing", status_code=404)
            return _serve_bytes(path.read_bytes(), row["media_type"], row["digest"], head)

        url = f"{base}/v2/{repo}/manifests/{ref}"
        r = await self._fetch(url, request.headers.get("accept") or _MANIFEST_ACCEPT)
        if r.status_code != 200:
            return Response(r.text, status_code=r.status_code)
        body = r.content
        mt = r.headers.get("content-type") or _guess_media_type(body)
        digest = r.headers.get("docker-content-digest") or "sha256:" + hashlib.sha256(body).hexdigest()
        self._store_manifest(digest, body)
        await self._core.ledger.aset_tag(dest, repo, ref, digest, mt)
        # An image manifest carries the layer sizes, so we know the real image size
        # now; an index does not — register its children so the platform manifest's
        # later digest pull back-fills this row (size stays the manifest bytes until).
        size = _image_size(body)
        for child_digest, arch in _index_children(body):
            self._pending[child_digest] = (name, ref, digest, arch, url)
        if size is not None:
            # Single-arch image: the client re-pulls this same digest right after the
            # tag. Map it back to this tag row so that pull updates it in place rather
            # than adding a separate digest-keyed duplicate.
            self._pending[digest] = (name, ref, digest, None, url)
        await self._core.ledger.arecord(ArtifactRecord(
            ecosystem="docker", name=name, version=ref, digest=digest,
            size=size if size is not None else len(body), origin=url,
        ))
        return _serve_bytes(body, mt, digest, head)

    async def _manifest_by_digest(self, base, repo, name, digest, head) -> Response:
        path = self._core.storage.blob_path(digest)
        if path.exists():
            body = path.read_bytes()
            # Record on the hit too: an image cached before it had a ledger row (or
            # pulled by digest on a hit) would otherwise stay invisible in the UI,
            # since this is the only manifest request the client makes for it.
            await self._record_digest_manifest(name, digest, body, f"{base}/v2/{repo}/manifests/{digest}")
            return _serve_bytes(body, _guess_media_type(body), digest, head)
        if self._core.config.offline:
            self._core.progress.record_recent(name, f"{name}@{digest[7:19]}", None, hit=False, failed=True)
            return PlainTextResponse("manifest not cached", status_code=404)
        url = f"{base}/v2/{repo}/manifests/{digest}"
        r = await self._fetch(url, _MANIFEST_ACCEPT)
        if r.status_code != 200:
            return Response(r.text, status_code=r.status_code)
        body = r.content
        if hashlib.sha256(body).hexdigest() != digest.split(":", 1)[-1]:
            return PlainTextResponse("digest mismatch", status_code=502)
        self._store_manifest(digest, body)
        mt = r.headers.get("content-type") or _guess_media_type(body)
        await self._record_digest_manifest(name, digest, body, url)
        return _serve_bytes(body, mt, digest, head)

    async def _record_digest_manifest(self, name, digest, body, url) -> None:
        """Record a manifest fetched by digest. If it's a known index child, back-fill
        the parent tag row's real image size; otherwise (a bare `pull img@sha256:…`)
        give the image its own digest-keyed row so it shows up in the UI."""
        size = _image_size(body)
        pend = self._pending.pop(digest, None)
        if pend is not None:
            pname, tag, index_digest, arch, origin = pend
            if size is not None:
                await self._core.ledger.arecord(ArtifactRecord(
                    ecosystem="docker", name=pname, version=tag, digest=index_digest,
                    size=size, origin=origin, arch=arch,
                ))
            return
        if size is not None:  # a real image manifest pulled directly by digest
            await self._core.ledger.arecord(ArtifactRecord(
                ecosystem="docker", name=name, version=digest, digest=digest,
                size=size, origin=url,
            ))

    def _store_manifest(self, digest: str, body: bytes) -> None:
        path = self._core.storage.blob_path(digest)
        if path.exists():
            return
        tmp, f = self._core.storage.open_part(path)
        with f:
            f.write(body)
        self._core.storage.commit_part(tmp, path)

    # ---- blobs ---------------------------------------------------------------
    async def blob(self, request: Request) -> Response:
        routed = self._route(request.path_params["name"])
        if routed is None:
            return PlainTextResponse("unknown registry prefix", status_code=404)
        base, dest, repo = routed
        digest = request.path_params["digest"]
        final = self._core.storage.blob_path(digest)
        if self._core.config.offline and not final.exists():
            self._core.progress.record_recent(
                f"oci-blob/{digest}", f"{repo}@{digest[7:19]}", None, hit=False, failed=True)
            return PlainTextResponse("blob not cached", status_code=404)

        url = f"{base}/v2/{repo}/blobs/{digest}"
        headers = {"Docker-Content-Digest": digest}

        def opener():
            return _AuthStream(self._core, url)

        return await self._core.cache.fetch(
            key=f"oci-blob/{digest}",
            final_path=final,
            stream_opener=opener,
            name=f"{repo}@{digest[7:19]}",
            method=request.method,
            request=request,
            media_type="application/octet-stream",
            response_headers=headers,
            expected_sha256=digest.split(":", 1)[-1],
        )

    # ---- tags/list -----------------------------------------------------------
    async def tags_list(self, request: Request) -> Response:
        routed = self._route(request.path_params["name"])
        if routed is None:
            return PlainTextResponse("unknown registry prefix", status_code=404)
        base, dest, repo = routed
        if self._core.config.offline:
            tags = await self._core.ledger.alist_tags(dest, repo)
            return JSONResponse({"name": request.path_params["name"], "tags": tags})
        r = await self._fetch(f"{base}/v2/{repo}/tags/list", "application/json")
        if r.status_code != 200:
            return Response(r.text, status_code=r.status_code)
        return Response(r.content, media_type="application/json")

    # ---- helpers -------------------------------------------------------------
    def _route(self, name: str) -> tuple[str, str, str] | None:
        dest, _, repo = name.partition("/")
        base = self._core.config.upstreams.get(dest)
        if base is None or not repo:
            return None
        if dest == "dockerhub" and "/" not in repo:
            repo = "library/" + repo
        return base.rstrip("/"), dest, repo

    async def _fetch(self, url: str, accept: str) -> httpx.Response:
        client = self._core.upstream.client
        headers = {"Accept": accept}
        r = await client.get(url, headers=headers)
        if r.status_code == 401:
            authed = await self._core.upstream.authed_headers(r, headers)
            r = await client.get(url, headers=authed)
        return r

    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        """Rebuild docker artifacts from the oci_tags table (the tag→digest source
        of truth) + blob sizes on disk."""
        db = cache_dir / "ledger.db"
        if not db.exists():
            return
        conn = sqlite3.connect(db)
        conn.row_factory = sqlite3.Row
        try:
            for row in conn.execute("SELECT upstream, repo, tag, digest FROM oci_tags"):
                name = f"{row['upstream']}/{_strip_library(row['repo'])}"
                bp = self._core.storage.blob_path(row["digest"]) if hasattr(self, "_core") else None
                size = bp.stat().st_size if bp and bp.exists() else None
                yield ArtifactRecord(
                    ecosystem="docker", name=name, version=row["tag"],
                    digest=row["digest"], size=size,
                )
        finally:
            conn.close()


class _AuthStream:
    """Async context manager: stream a blob, retrying once with a Bearer token."""

    def __init__(self, core: Core, url: str) -> None:
        self._core = core
        self._url = url
        self._cm = None

    async def __aenter__(self) -> httpx.Response:
        client = self._core.upstream.client
        self._cm = client.stream("GET", self._url)
        resp = await self._cm.__aenter__()
        if resp.status_code == 401:
            await self._cm.__aexit__(None, None, None)
            authed = await self._core.upstream.authed_headers(resp, {})
            self._cm = client.stream("GET", self._url, headers=authed)
            resp = await self._cm.__aenter__()
        return resp

    async def __aexit__(self, *exc) -> None:
        if self._cm is not None:
            await self._cm.__aexit__(*exc)


def _serve_bytes(body: bytes, media_type: str | None, digest: str, head: bool) -> Response:
    headers = {"Docker-Content-Digest": digest}
    if head:
        headers["Content-Length"] = str(len(body))
        return Response(status_code=200, media_type=media_type, headers=headers)
    return Response(body, media_type=media_type, headers=headers)


def _image_size(body: bytes) -> int | None:
    """Total cached image size from an image manifest: config blob + all layer blobs
    (the compressed sizes actually stored). Returns None for an index or non-manifest
    JSON, whose own size we can't know until a platform child manifest is seen."""
    try:
        m = json.loads(body)
    except (ValueError, TypeError):
        return None
    layers = m.get("layers")
    if not isinstance(layers, list):
        return None
    total = sum(l.get("size", 0) for l in layers if isinstance(l, dict))
    cfg = m.get("config")
    if isinstance(cfg, dict):
        total += cfg.get("size", 0) or 0
    return total or None


def _index_children(body: bytes) -> list[tuple[str, str | None]]:
    """(child digest, arch) for the real platform sub-manifests of an image index.
    Attestation/provenance entries (platform.architecture == 'unknown') are skipped."""
    try:
        m = json.loads(body)
    except (ValueError, TypeError):
        return []
    out = []
    for d in m.get("manifests", []) or []:
        if not isinstance(d, dict):
            continue
        dig = d.get("digest")
        arch = (d.get("platform") or {}).get("architecture")
        if dig and arch != "unknown":
            out.append((dig, arch))
    return out


def _guess_media_type(body: bytes) -> str:
    try:
        mt = json.loads(body).get("mediaType")
        if mt:
            return mt
    except (ValueError, AttributeError):
        pass
    return "application/vnd.oci.image.manifest.v1+json"


def _strip_library(repo: str) -> str:
    return repo[len("library/"):] if repo.startswith("library/") else repo
