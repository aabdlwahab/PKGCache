"""The unified listener: every HTTPS role on ONE port.

The five HTTPS roles (oci, npm, pypi, git, files) share a single TLS port
(PKGCACHE_UNIFIED_PORT, default 8443). Their namespaces can't collide:

  /v2/…                      → oci   (protocol-pinned root; the project rides the
                                      image name: /v2/<project>/dockerhub/…)
  /<project>/<role>/…        → npm / pypi / git / files (and the uniform admin form
                                      for oci and apt) — ALWAYS fully qualified;
                                      global is /global/<role>/…
  /healthz                   → this listener's own health (roles + project count)
  /                          → a small JSON index of the URL shapes above

apt/apk is NOT here: it's a forward proxy, and proxy clients (busybox wget, apt
< 1.6) can't speak to a TLS proxy — it keeps its own plain-HTTP port (:3142).
An absolute-form request-target (a proxy client aimed at this port by mistake)
gets a 400 pointing at the apt port.

The dispatcher owns nothing itself: it holds the same RoleServer instances the
supervisor reconciles, so there is exactly ONE Core per (project, role) in the
process — the ledger single-writer and single-flight semantics hold by
construction. Its lifespan shutdown closes those RoleServers' cores (apt's own
listener handles apt's).
"""
from __future__ import annotations

import json

from .router import RoleServer

# Path roles reachable as /<project>/<role>/… on the unified port. oci and apt are
# included for the uniform ADMIN form (healthz/progress/+ledger); their client
# protocols use /v2/… and the :3142 proxy respectively.
_ROLES = ("oci", "npm", "pypi", "git", "files", "apt")


class UnifiedServer:
    """ASGI app for the unified port, dispatching to the per-role RoleServers."""

    def __init__(self, role_servers: dict[str, RoleServer]) -> None:
        # All six RoleServers are known so the uniform admin form covers apt too;
        # only the five HTTPS roles' cores belong to this listener's lifecycle
        # (apt's plain-HTTP listener owns apt's).
        self._servers = role_servers
        self._owned = [rs for role, rs in role_servers.items() if role != "apt"]

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] == "lifespan":
            await self._lifespan(scope, receive, send)
            return
        if scope["type"] != "http":
            await send({"type": "websocket.close"})  # no websocket endpoints exist
            return

        raw = scope.get("raw_path") or b""
        if raw.startswith((b"http://", b"https://")):
            await _text(send, 400,
                        b"this is the cache's unified HTTPS port, not the apt/apk "
                        b"forward proxy - point http_proxy at port 3142")
            return

        path = scope.get("path", "/")
        if path == "/v2" or path.startswith("/v2/"):
            await self._servers["oci"](scope, receive, send)
            return
        if path == "/healthz":
            await self._healthz(send)
            return
        if path == "/":
            await self._index(send)
            return

        segs = path.lstrip("/").split("/", 2)
        if len(segs) >= 2 and segs[1] in self._servers:
            rs = self._servers[segs[1]]
            if not rs.serves(segs[0]):
                await _text(send, 404,
                            f"unknown project '{segs[0]}' - create it in the console, "
                            f"or use /global/{segs[1]}/...".encode())
                return
            await rs(scope, receive, send)
            return

        await _text(send, 404,
                    b"not found - URLs here are /<project>/<role>/... "
                    b"(roles: npm, pypi, git, files) or /v2/... for docker; "
                    b"the default project is 'global'")

    async def _lifespan(self, scope, receive, send) -> None:
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                for rs in self._owned:
                    await rs.aclose_all()
                await send({"type": "lifespan.shutdown.complete"})
                return

    async def _healthz(self, send) -> None:
        body = json.dumps({
            "status": "ok",
            "server": "unified",
            "roles": [r for r in _ROLES if r != "apt"],
        }).encode()
        await _json(send, 200, body)

    async def _index(self, send) -> None:
        body = json.dumps({
            "docker": "/v2/ (pull <host>:<port>/[<project>/]{dockerhub,ghcr,quay}/<image>)",
            "npm": "/<project>/npm/",
            "pypi": "/<project>/pypi/<index>/+simple/",
            "git": "/<project>/git/<upstream-host>/<owner>/<repo>.git",
            "files": "/<project>/files/<path>",
            "apt": "forward proxy on port 3142 (project = proxy username)",
            "default_project": "global",
        }, indent=2).encode()
        await _json(send, 200, body)


async def _json(send, status: int, body: bytes) -> None:
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": body})


async def _text(send, status: int, body: bytes) -> None:
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"text/plain; charset=utf-8")]})
    await send({"type": "http.response.body", "body": body})
