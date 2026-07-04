"""Per-role request routing to per-project sub-apps.

Historically each (project, role) got its own port; a project was a set of six
ports carved from a pool. This module replaces that: ONE server per role listens
on the role's default port, and every project is reached on that same port,
distinguished by the request itself:

  * npm / pypi / git / files — a `/<project>/<role>/…` PATH prefix. The prefix is
    stripped and stashed in scope["root_path"] so the sub-app matches its normal
    routes and external_base() re-emits project-scoped links.
  * oci — the project rides the image NAME: `/v2/<project>/<dest>/<image>/…`
    (Docker can't be given a base path, so the segment lives inside the repo name).
    The segment is stripped back to `/v2/<dest>/<image>/…` for the sub-app; the
    project is stashed so tags/list can re-prefix the echoed `name`.
  * apt — a forward proxy with no room in the URL for a project, so the project
    rides the proxy username: `http_proxy=http://<project>@host:3142`. Read from
    Proxy-Authorization; the password is ignored (it's a label, not auth).

Anything that doesn't match a registered project (or names `global`) is served by
the global sub-app unchanged, so every original URL keeps working.

A RoleServer owns the Core lifecycle for all its projects (mounted sub-apps do NOT
receive Starlette lifespan events), adding/removing projects live as the registry
changes — no port bind, no restart.
"""
from __future__ import annotations

import asyncio
import base64

from starlette.applications import Starlette

from .app import build_app, close_core, start_core_tasks
from .core import Core
from .core.config import GLOBAL, Config

# Roles whose project prefix is a leading `/<project>/<role>` path segment.
_PATH_ROLES = {"npm", "pypi", "git", "files"}


def _basic_user(value: bytes) -> str | None:
    """The username from a `Basic base64(user:pass)` Proxy-Authorization value."""
    try:
        scheme, _, b64 = value.decode("latin-1").partition(" ")
        if scheme.lower() != "basic":
            return None
        return base64.b64decode(b64).decode("utf-8", "replace").partition(":")[0] or None
    except (ValueError, UnicodeDecodeError):
        return None


class RoleServer:
    """ASGI app for one role's port. Dispatches each request to a project sub-app."""

    def __init__(self, role: str) -> None:
        self.role = role
        self._apps: dict[str, Starlette] = {}
        self._cores: dict[str, Core] = {}
        self._tasks: dict[str, list[asyncio.Task]] = {}
        self._lock = asyncio.Lock()

    # ---- ASGI ----------------------------------------------------------------
    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] == "lifespan":
            await self._lifespan(scope, receive, send)
            return
        project, new_scope = self._select(scope)
        app = self._apps.get(project) or self._apps.get(GLOBAL)
        if app is None:  # no global yet (pre-reconcile) — refuse rather than crash
            await _text(send, 503, b"pkgcache: role server not ready")
            return
        await app(new_scope, receive, send)

    async def _lifespan(self, scope, receive, send) -> None:
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                await self.aclose_all()
                await send({"type": "lifespan.shutdown.complete"})
                return

    # ---- project reconciliation ---------------------------------------------
    async def reconcile(self, cfgs: dict[str, Config]) -> None:
        """Make the running project set match `cfgs` (which always includes global):
        build+start cores for new projects, close+drop cores for removed ones."""
        async with self._lock:
            for name, cfg in cfgs.items():
                if name not in self._apps:
                    self._add(name, cfg)
            for name in list(self._apps):
                if name not in cfgs:
                    await self._remove(name)

    async def aclose_all(self) -> None:
        async with self._lock:
            for name in list(self._apps):
                await self._remove(name)

    def _add(self, name: str, cfg: Config) -> None:
        app = build_app(cfg, manage_lifecycle=False)
        core = app.state.core
        self._apps[name] = app
        self._cores[name] = core
        self._tasks[name] = start_core_tasks(core, cfg)

    async def _remove(self, name: str) -> None:
        self._apps.pop(name, None)
        core = self._cores.pop(name, None)
        tasks = self._tasks.pop(name, None)
        if core is not None:
            await close_core(tasks or [], core)

    # ---- selection -----------------------------------------------------------
    def _select(self, scope) -> tuple[str, dict]:
        if self.role == "oci":
            return self._select_oci(scope)
        if self.role == "apt":
            return self._select_apt(scope)
        return self._select_path(scope)

    def _select_path(self, scope) -> tuple[str, dict]:
        path = scope.get("path", "/")
        segs = path.lstrip("/").split("/", 2)
        # /<project>/<role>/… (and the explicit /global/<role>/… alias) → sub-app.
        if len(segs) >= 2 and segs[1] == self.role and segs[0] in self._apps:
            return self._strip_prefix(scope, f"/{segs[0]}/{self.role}", segs[0])
        return GLOBAL, scope

    def _select_oci(self, scope) -> tuple[str, dict]:
        path = scope.get("path", "/")
        if not path.startswith("/v2/"):
            return GLOBAL, scope  # /v2/ ping, /v2/_progress, /healthz
        seg, sep, tail = path[len("/v2/"):].partition("/")
        if sep and seg in self._apps and seg != GLOBAL:
            new = self._replace_path(scope, "/v2/" + seg, "/v2")
            new["pkgcache_oci_project"] = seg  # tags/list re-prefixes its `name`
            return seg, new
        return GLOBAL, scope

    def _select_apt(self, scope) -> tuple[str, dict]:
        # Real apt/apk clients are forward proxies with no room in the URL for a
        # project, so the project rides the proxy username.
        for k, v in scope.get("headers", []):
            if k == b"proxy-authorization":
                proj = _basic_user(v)
                if proj and proj in self._apps and proj != GLOBAL:
                    return proj, scope  # apt scope is untouched (forward-proxy target)
                break
        # Internal pollers (progress/health) can't set a proxy user, so also accept
        # the uniform `/<project>/apt/…` path form. A real proxied request arrives
        # with an absolute-URL raw_path, so it never starts with this prefix.
        path = scope.get("path", "/")
        segs = path.lstrip("/").split("/", 2)
        if len(segs) >= 2 and segs[1] == "apt" and segs[0] in self._apps:
            return self._strip_prefix(scope, f"/{segs[0]}/apt", segs[0])
        return GLOBAL, scope

    # ---- scope rewriting -----------------------------------------------------
    def _strip_prefix(self, scope, prefix: str, project: str) -> tuple[str, dict]:
        new = self._replace_path(scope, prefix, "")
        new["root_path"] = scope.get("root_path", "") + prefix
        return project, new

    @staticmethod
    def _replace_path(scope, prefix: str, replacement: str) -> dict:
        """Copy `scope` with `prefix` at the start of path/raw_path swapped for
        `replacement` (default ""). Project/role/image segments are plain ASCII with
        no percent-encoding, so byte-slicing raw_path by the prefix is safe."""
        new = dict(scope)
        path = scope.get("path", "/")
        new["path"] = replacement + path[len(prefix):] or "/"
        raw = scope.get("raw_path")
        if raw:
            pb = prefix.encode("latin-1")
            if raw.startswith(pb):
                new["raw_path"] = (replacement.encode("latin-1") + raw[len(pb):]) or b"/"
        return new


async def _text(send, status: int, body: bytes) -> None:
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"text/plain; charset=utf-8")]})
    await send({"type": "http.response.body", "body": body})
