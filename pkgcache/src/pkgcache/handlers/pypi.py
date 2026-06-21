"""PyPI Simple pull-through — replaces devpi, and fixes its defect.

The devpi problem: it only committed a mirrored file after a COMPLETE server-side
download and had no Range support, so multi-GB torch CUDA wheels were re-fetched on
every install. Here, the shared single-flight cache streams upstream→disk once,
commits atomically, and serves cached files with full Range support (FileResponse),
so uv/pip resume and parallel-download against the cache instead of forcing re-pulls.

Index selects the upstream (config `indexes`): root/pypi, root/pytorch-cu124, …
We rewrite the simple index so file links point back at this proxy, preserving the
PEP 503/691 attributes uv depends on (hashes, requires-python, yanked,
core-metadata) and proxying the PEP 658 `.metadata` sidecars.
"""
from __future__ import annotations

import html
import json
import re
from collections.abc import Iterable
from pathlib import Path
from urllib.parse import urljoin

import httpx
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, PlainTextResponse, Response
from starlette.routing import BaseRoute, Route

from ..core import Core
from ..core.ledger import ArtifactRecord
from .common import external_base, normalize_pypi_name, parse_dist_filename

_ANCHOR_RE = re.compile(r'<a\s+([^>]*?)href="([^"]+)"([^>]*)>([^<]*)</a>', re.I)
_JSON_ACCEPT = "application/vnd.pypi.simple.v1+json"


def _attr(a: str, name: str) -> str | None:
    """The value of HTML attribute `name` within the attribute string `a`, or None."""
    m = re.search(rf'{name}="([^"]*)"', a)
    return m.group(1) if m else None


def _norm_core_metadata(val: object) -> bool | dict:
    """Canonicalize a PEP 658/714 core-metadata marker into the PEP 691 shape.

    The HTML (PEP 503) form is a string attribute — either present-but-empty
    (== true) or "sha256=<hex>" — while PEP 691 JSON requires a bool or a
    {algorithm: hash} map. Returning a string here makes uv reject the index
    ("expected a boolean or map"), so normalize every source to bool|dict.
    """
    if val is True:
        return True
    if not val:  # None, False, "" → not present
        return False
    if isinstance(val, dict):
        return val
    algo, sep, digest = str(val).partition("=")
    return {algo: digest} if sep and digest else True


class PypiRepo:
    role = "pypi"
    progress_path = "/+progress"

    def client_endpoint(self, host: str) -> str:
        return f"https://{host}:3141/root/pypi/+simple/   (--index-url …/<index>/+simple/)"

    def mount(self, core: Core) -> list[BaseRoute]:
        self._core = core
        return [
            Route("/{index:path}/+simple/{project}/", self.simple, methods=["GET"]),
            Route("/{index:path}/+f/{project}/{filename}", self.file, methods=["GET", "HEAD"]),
        ]

    # ---- simple index --------------------------------------------------------
    async def simple(self, request: Request) -> Response:
        index = request.path_params["index"]
        project = normalize_pypi_name(request.path_params["project"])
        base = self._index_base(index)
        if base is None:
            return PlainTextResponse(f"unknown index {index}", status_code=404)

        cache_file = self._core.storage.safe_path(index, project, "simple.json")
        files = await self._load_simple(index, project, base, cache_file)
        if files is None:
            self._core.progress.record_recent(project, project, None, hit=False, failed=True)
            return PlainTextResponse(f"no cached index for {project}", status_code=404)

        ext = external_base(request)
        prefix = f"{ext}/{index}/+f/{project}"
        wants_json = _JSON_ACCEPT in request.headers.get("accept", "")
        return _render_json(project, files, prefix) if wants_json else _render_html(project, files, prefix)

    async def _load_simple(self, index, project, base, cache_file) -> list[dict] | None:
        page_url = f"{base}/{project}/"
        if not self._core.config.offline:
            try:
                r = await self._core.upstream.client.get(
                    page_url, headers={"Accept": f"{_JSON_ACCEPT}, text/html"}
                )
                if r.status_code == 200:
                    files = _parse_simple(r.content, r.headers.get("content-type", ""), page_url)
                    cache_file.parent.mkdir(parents=True, exist_ok=True)
                    tmp = cache_file.with_suffix(".json.part")
                    tmp.write_text(json.dumps(files))
                    tmp.replace(cache_file)
                    return files
            except httpx.HTTPError:
                pass  # fall back to cache below
        if cache_file.exists():
            return json.loads(cache_file.read_text())
        return None

    # ---- file (wheel / sdist / .metadata) -----------------------------------
    async def file(self, request: Request) -> Response:
        index = request.path_params["index"]
        project = normalize_pypi_name(request.path_params["project"])
        filename = request.path_params["filename"]
        base = self._index_base(index)
        if base is None:
            return PlainTextResponse(f"unknown index {index}", status_code=404)

        is_metadata = filename.endswith(".metadata")
        lookup_name = filename[: -len(".metadata")] if is_metadata else filename

        cache_file = self._core.storage.safe_path(index, project, "simple.json")
        files = await self._load_simple(index, project, base, cache_file)
        entry = next((f for f in (files or []) if f.get("filename") == lookup_name), None)
        if entry is None:
            self._core.progress.record_recent(filename, filename, None, hit=False, failed=True)
            return PlainTextResponse(f"unknown file {filename}", status_code=404)

        upstream_url = entry["url"] + (".metadata" if is_metadata else "")
        final_path = self._core.storage.safe_path(index, "+f", project, filename)
        expected = None if is_metadata else (entry.get("hashes") or {}).get("sha256")
        media_type = "text/plain" if is_metadata else "application/octet-stream"

        def on_commit(size: int, hexd: str):
            if is_metadata:
                return None  # sidecars aren't ledger artifacts
            name, ver, tag = parse_dist_filename(filename)
            if not name:
                return None
            return ArtifactRecord(
                ecosystem="pip", name=name, version=ver or "",
                digest=f"sha256:{hexd}", size=size, origin=upstream_url,
                path=str(final_path.relative_to(self._core.storage.root)), arch=tag,
            )

        client = self._core.upstream.client

        def opener():
            return client.stream("GET", upstream_url)

        return await self._core.cache.fetch(
            key=f"{index}/+f/{project}/{filename}",
            final_path=final_path,
            stream_opener=opener,
            name=filename,
            method=request.method,
            request=request,
            media_type=media_type,
            expected_sha256=expected,
            on_commit=on_commit,
        )

    # ---- helpers -------------------------------------------------------------
    def _index_base(self, index: str) -> str | None:
        return self._core.config.indexes.get(index)

    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        for f in sorted(cache_dir.glob("**/+f/**/*")):
            if not f.is_file() or f.name.endswith(".metadata"):
                continue
            name, ver, tag = parse_dist_filename(f.name)
            if not name:
                continue
            yield ArtifactRecord(
                ecosystem="pip", name=name, version=ver or "", size=f.stat().st_size,
                path=str(f.relative_to(cache_dir)), arch=tag,
            )


def _parse_simple(content: bytes, content_type: str, page_url: str) -> list[dict]:
    """Normalize PEP 691 JSON or PEP 503 HTML into {filename,url,hashes,…} rows."""
    files: list[dict] = []
    if "json" in (content_type or "").lower():
        data = json.loads(content)
        for f in data.get("files", []):
            files.append({
                "filename": f.get("filename"),
                "url": urljoin(page_url, f.get("url", "")).split("#", 1)[0],
                "hashes": f.get("hashes") or {},
                "requires_python": f.get("requires-python"),
                "yanked": f.get("yanked", False),
                "core_metadata": _norm_core_metadata(
                    f.get("core-metadata", f.get("dist-info-metadata"))),
            })
        return files
    text = content.decode("utf-8", "replace")
    for pre, href, post, fn in _ANCHOR_RE.findall(text):
        attrs = pre + post
        full = urljoin(page_url, href)
        url, _, frag = full.partition("#")
        hashes = {}
        if frag.startswith("sha256="):
            hashes["sha256"] = frag[len("sha256="):]
        rp = _attr(attrs, "data-requires-python")
        cm = _attr(attrs, "data-core-metadata")
        if cm is None:
            cm = _attr(attrs, "data-dist-info-metadata")
        if cm == "":  # attribute present but no hash → available, hash unknown
            cm = True
        files.append({
            "filename": (fn.strip() or url.rsplit("/", 1)[-1]),
            "url": url,
            "hashes": hashes,
            "requires_python": html.unescape(rp) if rp else None,
            "yanked": "data-yanked" in attrs,
            "core_metadata": _norm_core_metadata(cm),
        })
    return files


def _render_html(project: str, files: list[dict], prefix: str) -> HTMLResponse:
    rows = []
    for f in files:
        href = f"{prefix}/{f['filename']}"
        sha = (f.get("hashes") or {}).get("sha256")
        if sha:
            href += f"#sha256={sha}"
        attrs = ""
        if f.get("requires_python"):
            attrs += f' data-requires-python="{html.escape(f["requires_python"])}"'
        if f.get("yanked"):
            attrs += ' data-yanked=""'
        if f.get("core_metadata"):
            cm = f["core_metadata"]
            val = "true" if cm is True else (f"sha256={cm['sha256']}" if isinstance(cm, dict) and cm.get("sha256") else "true")
            attrs += f' data-core-metadata="{val}"'
        rows.append(f'<a href="{href}"{attrs}>{html.escape(f["filename"])}</a><br/>')
    body = (
        '<!DOCTYPE html><html><head><meta name="pypi:repository-version" content="1.1">'
        f"<title>Links for {project}</title></head><body><h1>Links for {project}</h1>\n"
        + "\n".join(rows)
        + "\n</body></html>\n"
    )
    return HTMLResponse(body)


def _render_json(project: str, files: list[dict], prefix: str) -> JSONResponse:
    out = []
    for f in files:
        out.append({
            "filename": f["filename"],
            "url": f"{prefix}/{f['filename']}",
            "hashes": f.get("hashes") or {},
            "requires-python": f.get("requires_python"),
            "yanked": f.get("yanked", False),
            "core-metadata": f.get("core_metadata") or False,
        })
    payload = {"meta": {"api-version": "1.1"}, "name": project, "files": out}
    return JSONResponse(payload, media_type=_JSON_ACCEPT)
