"""Upstream HTTP: a shared pooled httpx client plus the generic anonymous Bearer
token dance used by OCI registries (Docker Hub / ghcr / quay).

Bodies are ALWAYS streamed (never buffered) so multi-GB blobs/wheels are safe.
Anonymous pulls only — no credentials (private images are out of scope; Docker Hub
anon rate limits are accepted, mitigated by caching each image once + prefetch).
"""
from __future__ import annotations

import re
import time

import httpx

_WWW_AUTH_RE = re.compile(r'(\w+)="([^"]*)"')


class Upstream:
    def __init__(self, *, timeout: float) -> None:
        # Generous read timeout so a slow multi-GB fetch finishes; verify TLS.
        # Accept-Encoding: identity — we are a byte-faithful cache. If we let httpx
        # transparently decompress, the body bytes would differ from the upstream
        # Content-Length (and from the index-declared hash), truncating clients.
        self._client = httpx.AsyncClient(
            timeout=httpx.Timeout(timeout, connect=30.0),
            follow_redirects=True,
            verify=True,
            headers={"User-Agent": "pkgcache/0.1", "Accept-Encoding": "identity"},
        )
        self._tokens: dict[str, tuple[str, float]] = {}  # scope -> (token, expiry)

    async def aclose(self) -> None:
        await self._client.aclose()

    @property
    def client(self) -> httpx.AsyncClient:
        return self._client

    # ---- OCI bearer-token auth ----------------------------------------------
    async def _bearer_for(self, www_authenticate: str) -> str | None:
        """Resolve an anonymous Bearer token from a 401's WWW-Authenticate header."""
        params = dict(_WWW_AUTH_RE.findall(www_authenticate))
        realm = params.get("realm")
        if not realm:
            return None
        scope = params.get("scope", "")
        cache_key = f"{realm}|{params.get('service','')}|{scope}"
        tok = self._tokens.get(cache_key)
        if tok and tok[1] > time.time():
            return tok[0]
        q = {}
        if params.get("service"):
            q["service"] = params["service"]
        if scope:
            q["scope"] = scope
        r = await self._client.get(realm, params=q)
        r.raise_for_status()
        data = r.json()
        token = data.get("token") or data.get("access_token")
        if not token:
            return None
        ttl = float(data.get("expires_in", 300))
        self._tokens[cache_key] = (token, time.time() + ttl * 0.8)
        return token

    async def authed_headers(self, prior_401: httpx.Response | None,
                             base: dict | None = None) -> dict:
        headers = dict(base or {})
        if prior_401 is not None:
            challenge = prior_401.headers.get("www-authenticate", "")
            if challenge.lower().startswith("bearer"):
                token = await self._bearer_for(challenge)
                if token:
                    headers["Authorization"] = f"Bearer {token}"
        return headers
