"""Warm the cache from a uv.lock, then rewrite it to pull through PKGCache.

A uv.lock pins, for every registry package, the exact file set (all platforms /
pythons) with URLs + sha256 hashes. To take a project air-gapped we must cache
*those exact files* — re-resolving by name would only fetch the current
platform's wheels and miss the rest. So this module:

  * parses the lock into its registry packages (LockParser),
  * maps each package's upstream registry to a configured PKGCache index, read
    from the pypi role's /+indexes (IndexMap),
  * drives the pypi proxy's +f endpoint to pull each file into the cache exactly
    as a real `uv sync` would — reusing the proxy's single-flight, hash
    verification and ledger (Proxy.warm), and
  * rewrites the lock so every file URL + registry points at this cache, leaving
    hashes intact so uv still verifies bytes (LockRewriter).

A spike confirmed uv installs straight from the rewritten URLs (`uv sync
--frozen`) and accepts the rewritten lock as consistent (`uv sync --locked`)
when its index is pointed at the same PKGCache base. Stdlib only — the control
plane carries no third-party dependencies.
"""
from __future__ import annotations

import json
import os
import re
import ssl
import tomllib
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass

# uv.lock schema major we understand. The `revision` field moves independently and
# does not change the parts we read (package source + file url/hash); a new
# `version` might, so we refuse it loudly rather than mis-rewrite.
SUPPORTED_VERSION = 1

# Short read for control endpoints; generous pull for files (multi-GB CUDA wheels).
_READ_TIMEOUT = 15
_WARM_TIMEOUT = 1800
# Files warm concurrently — the pulls are network-bound and the proxy single-flights
# each one, so a bounded fan-out is the win. Tunable for big/slow upstreams.
_WARM_WORKERS = int(os.environ.get("PKGCACHE_WARM_WORKERS", "8"))

_NORMALIZE_RE = re.compile(r"[-_.]+")


class LockError(ValueError):
    """The uploaded uv.lock is malformed or an unsupported version."""


class WarmError(RuntimeError):
    """The pypi proxy became unreachable mid-warm (distinct from a per-file 404)."""


@dataclass(frozen=True)
class LockedFile:
    filename: str
    url: str
    hash: str | None
    size: int | None


@dataclass(frozen=True)
class WarmResult:
    filename: str
    ok: bool
    detail: str


@dataclass(frozen=True)
class Package:
    name: str
    registry: str
    files: list[LockedFile]

    @property
    def project(self) -> str:
        """PEP 503 normalized name — the path segment PKGCache serves files under."""
        return _NORMALIZE_RE.sub("-", self.name).lower()


class LockParser:
    """Turns uv.lock text into its registry packages. Non-registry sources
    (virtual/git/path/url/editable) are not cacheable and are dropped — the
    rewriter leaves them untouched in the output."""

    def parse(self, text: str) -> list[Package]:
        try:
            data = tomllib.loads(text)
        except tomllib.TOMLDecodeError as exc:
            raise LockError(f"not a valid uv.lock (TOML parse failed: {exc})") from exc
        version = data.get("version")
        if version != SUPPORTED_VERSION:
            raise LockError(f"unsupported uv.lock version {version!r} (expected {SUPPORTED_VERSION})")
        packages = []
        for raw in data.get("package", []):
            registry = (raw.get("source") or {}).get("registry")
            if not registry:
                continue
            files = self._files(raw)
            if files:
                packages.append(Package(name=raw["name"], registry=registry, files=files))
        return packages

    def _files(self, raw: dict) -> list[LockedFile]:
        entries = []
        sdist = raw.get("sdist")
        if isinstance(sdist, dict):
            entries.append(sdist)
        entries.extend(raw.get("wheels") or [])
        return [self._file(e) for e in entries if e.get("url")]

    def _file(self, entry: dict) -> LockedFile:
        url = entry["url"]
        filename = url.rsplit("/", 1)[-1]
        return LockedFile(filename=filename, url=url, hash=entry.get("hash"), size=entry.get("size"))


class IndexMap:
    """Translates a lock's upstream registry URL into a configured PKGCache index,
    built by inverting the pypi role's {index: upstream} map."""

    def __init__(self, mapping: dict[str, str]) -> None:
        self._by_upstream = {self._norm(up): idx for idx, up in mapping.items()}

    def index(self, registry: str) -> str | None:
        return self._by_upstream.get(self._norm(registry))

    @staticmethod
    def _norm(url: str) -> str:
        return (url or "").rstrip("/")


class Proxy:
    """Adapter to one project's pypi role. Reads its index map / online state and
    drives the +f endpoint to pull a file through into the cache."""

    def __init__(self, base: str, context: ssl.SSLContext | None = None) -> None:
        self._base = base.rstrip("/")
        # Internal call to a role that terminates TLS with the private CA; the live
        # poller skips verification the same way (see services/livefeed.py).
        self._context = context or ssl._create_unverified_context()

    def offline(self) -> bool:
        with self._open("/healthz", _READ_TIMEOUT) as resp:
            return bool(json.loads(resp.read().decode("utf-8")).get("offline"))

    def indexes(self) -> dict[str, str]:
        with self._open("/+indexes", _READ_TIMEOUT) as resp:
            return json.loads(resp.read().decode("utf-8"))

    def warm(self, index: str, project: str, filename: str) -> int:
        """GET the file through +f so the proxy caches it; drain (don't buffer) the
        body and return the HTTP status. A 404/5xx is a per-file failure (returned);
        an unreachable proxy is fatal (raised)."""
        path = f"/{index}/+f/{project}/{filename}"
        try:
            with self._open(path, _WARM_TIMEOUT) as resp:
                while resp.read(1 << 16):
                    pass
                return resp.status
        except urllib.error.HTTPError as exc:
            return exc.code
        except urllib.error.URLError as exc:
            raise WarmError(f"pypi proxy unreachable at {self._base}: {exc.reason}") from exc

    def _open(self, path: str, timeout: int):
        req = urllib.request.Request(self._base + path, headers={"Accept": "application/json"})
        return urllib.request.urlopen(req, timeout=timeout, context=self._context)


class Warmer:
    """Warms a set of locked files through the proxy with a bounded thread pool,
    yielding a WarmResult per file as it completes (network-bound work, and each
    Proxy.warm opens its own connection, so threads give real concurrency)."""

    def __init__(self, proxy: Proxy, workers: int = _WARM_WORKERS) -> None:
        self._proxy = proxy
        self._workers = workers

    def warm(self, items: list[tuple[str, str, str]]):
        """items: (index, project, filename) triples. Yields in completion order,
        not input order."""
        if not items:
            return
        with ThreadPoolExecutor(max_workers=min(self._workers, len(items))) as pool:
            futures = [pool.submit(self._one, *item) for item in items]
            for future in as_completed(futures):
                yield future.result()

    def _one(self, index: str, project: str, filename: str) -> WarmResult:
        # A dead proxy mid-warm surfaces as a per-file failure (the up-front online
        # check already confirmed reachability), so one fan-out worker raising can't
        # abort the others — the caller still sees the full tally.
        try:
            status = self._proxy.warm(index, project, filename)
        except WarmError as exc:
            return WarmResult(filename, False, f"unreachable: {exc}")
        return WarmResult(filename, status == 200, "" if status == 200 else f"[{status}]")


class LockRewriter:
    """Rewrites lock text so every registry + file URL points at this cache,
    preserving hashes (uv still verifies bytes) and all formatting by replacing
    only the unique quoted URL tokens."""

    def rewrite(self, text: str, packages: list[Package], index_map: IndexMap, base: str) -> str:
        """`base` is the public scheme://host:port[/<project>/pypi] the client will
        reach this cache's pypi role on — file/registry URLs are re-pointed under it,
        hashes preserved so uv still verifies bytes."""
        base = base.rstrip("/")
        for pkg in packages:
            index = index_map.index(pkg.registry)
            if index is None:
                continue
            text = text.replace(f'"{pkg.registry}"', f'"{base}/{index}/+simple"')
            for f in pkg.files:
                text = text.replace(f'"{f.url}"', f'"{base}/{index}/+f/{pkg.project}/{f.filename}"')
        return text
