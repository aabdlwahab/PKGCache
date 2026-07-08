"""Entrypoint.

Two modes:
  * PKGCACHE_ROLE set   → serve that one role (dev / one-role-per-process).
  * PKGCACHE_ROLE unset → serve everything on TWO ports:
      - the unified HTTPS port (PKGCACHE_UNIFIED_PORT, default 8443) carries oci,
        npm, pypi, git and files — /v2/… for docker, /<project>/<role>/… for the
        rest (see unified.py). TLS terminates in-process from the cert.
      - the apt/apk forward proxy stays on ITS own plain-HTTP port (default 3142):
        proxy clients (busybox wget, apt < 1.6) can't speak to a TLS proxy, so it
        can't ride the unified port.

A single event loop runs both uvicorn servers, so the in-process progress registry
and single-flight dedup keep their (per-role) single-worker semantics.

Multi-project: projects are NOT ports — a RoleServer per role (router.py)
dispatches each request to the right project's sub-app by path/name/proxy-user.
A lightweight supervisor re-reads the registry on an interval and tells each
RoleServer to add/drop project sub-apps live, WITHOUT restarting the process or
binding any port, so creating a project from the control UI never disturbs
in-flight downloads.
"""
from __future__ import annotations

import asyncio
import os
import signal

import uvicorn

from .app import build_app
from .core.config import load, load_roles
from .router import RoleServer
from .unified import UnifiedServer

# How often the supervisor re-reads the project registry to pick up new/removed
# projects. Small enough that "create project" feels live; large enough to be cheap.
_POLL = float(os.environ.get("PKGCACHE_PROJECT_POLL", "5"))

# The single client-facing HTTPS port (>1024 so the non-root container user can
# bind it; publish 443→this on the host for port-less docker URLs).
_UNIFIED_PORT = int(os.environ.get("PKGCACHE_UNIFIED_PORT", "8443"))
_APT_PORT = 3142


def _uv_server(app, *, host: str, port: int, cert: str | None, key: str | None) -> uvicorn.Server:
    ucfg = uvicorn.Config(app, host=host, port=port, log_level="info",
                          ssl_certfile=cert, ssl_keyfile=key)
    server = uvicorn.Server(ucfg)
    # We manage signals centrally (one handler for both servers), so stop each
    # server from installing its own competing handlers.
    server.install_signal_handlers = lambda: None
    return server


async def _serve_all() -> None:
    loop = asyncio.get_running_loop()

    # Build one RoleServer per role and prime it with global + every registered
    # project BEFORE serving, so the first request already has its core ready.
    role_cfgs = load_roles()
    role_servers: dict[str, RoleServer] = {}
    for role, projects in role_cfgs.items():
        rs = RoleServer(role)
        await rs.reconcile(projects)
        role_servers[role] = rs

    host = os.environ.get("PKGCACHE_HOST", "0.0.0.0")
    cert = os.environ.get("PKGCACHE_TLS_CERT") or None
    key = os.environ.get("PKGCACHE_TLS_KEY") or None

    unified = UnifiedServer(role_servers)
    listeners = [
        (f"unified:{_UNIFIED_PORT}{'(tls)' if cert else ''}",
         _uv_server(unified, host=host, port=_UNIFIED_PORT, cert=cert, key=key)),
        (f"apt:{_APT_PORT}",
         _uv_server(role_servers["apt"], host=host, port=_APT_PORT, cert=None, key=None)),
    ]

    stopping = False

    def stop_all() -> None:
        nonlocal stopping
        stopping = True
        for _, s in listeners:
            s.should_exit = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop_all)

    async def run(label: str, server: uvicorn.Server) -> None:
        try:
            await server.serve()
        except SystemExit as exc:
            # uvicorn signals a startup failure (e.g. the port is already bound) with
            # sys.exit(), which is a BaseException — without this clause it would
            # escape the Exception guard below and take BOTH listeners down.
            print(f"pkgcache: server {label} failed to start (exit {exc.code})")
        except Exception as exc:  # noqa: BLE001 - one bad port must not kill the other
            print(f"pkgcache: server {label} exited with error: {exc}")

    tasks = [asyncio.create_task(run(label, s)) for label, s in listeners]
    print("pkgcache serving → " + ", ".join(label for label, _ in listeners))

    # Supervisor: re-read the registry and reconcile each role's project set.
    while not stopping:
        await asyncio.sleep(_POLL)
        if stopping:
            break
        try:
            desired = load_roles()
        except Exception as exc:  # noqa: BLE001 - a bad registry write shouldn't crash us
            print(f"pkgcache: project registry reload failed: {exc}")
            continue
        for role, rs in role_servers.items():
            try:
                await rs.reconcile(desired[role])
            except Exception as exc:  # noqa: BLE001 - a bad project shouldn't stop others
                print(f"pkgcache: reconcile {role} failed: {exc}")

    await asyncio.gather(*tasks, return_exceptions=True)


def main() -> None:
    if os.environ.get("PKGCACHE_ROLE", "").strip():
        cfg = load()
        uvicorn.run(build_app(cfg), host=cfg.host, port=cfg.port, workers=1,
                    log_level="info", ssl_certfile=cfg.tls_cert, ssl_keyfile=cfg.tls_key)
    else:
        asyncio.run(_serve_all())


if __name__ == "__main__":
    main()
