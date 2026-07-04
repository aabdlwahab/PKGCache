"""Entrypoint.

Two modes:
  * PKGCACHE_ROLE set   → serve that one role (dev / one-role-per-process).
  * PKGCACHE_ROLE unset → serve ALL roles in ONE process, each on its own port
    (the protocols can't share a port: OCI owns /v2/ at the root and apt is a
    forward proxy). The HTTPS roles terminate TLS in-process from the cert, so no
    separate TLS proxy is needed — one container.

A single event loop runs every uvicorn server, so the in-process progress registry
and single-flight dedup keep their (per-role) single-worker semantics.

Multi-project: there is exactly ONE server per role (six total), each on the role's
default port. Projects are NOT separate ports — a RoleServer (see router.py)
dispatches each request to the right project's sub-app by path/name/proxy-user, and
falls back to global. A lightweight supervisor re-reads the registry on an interval
and tells each RoleServer to add/drop project sub-apps live, WITHOUT restarting the
process or binding any new port, so creating a project from the control UI never
disturbs in-flight downloads.
"""
from __future__ import annotations

import asyncio
import os
import signal

import uvicorn

from .app import build_app
from .core.config import GLOBAL, Config, load, load_roles
from .router import RoleServer

# How often the supervisor re-reads the project registry to pick up new/removed
# projects. Small enough that "create project" feels live; large enough to be cheap.
_POLL = float(os.environ.get("PKGCACHE_PROJECT_POLL", "5"))


def _server(app, cfg: Config) -> uvicorn.Server:
    ucfg = uvicorn.Config(
        app,
        host=cfg.host,
        port=cfg.port,
        log_level="info",
        ssl_certfile=cfg.tls_cert,
        ssl_keyfile=cfg.tls_key,
    )
    server = uvicorn.Server(ucfg)
    # We manage signals centrally (one handler for all servers), so stop each
    # server from installing its own competing handlers.
    server.install_signal_handlers = lambda: None
    return server


def _label(cfg: Config) -> str:
    return f"{cfg.role}:{cfg.port}{'(tls)' if cfg.tls_cert else ''}"


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

    servers: list[tuple[str, Config, uvicorn.Server]] = []
    for role, rs in role_servers.items():
        g = role_cfgs[role][GLOBAL]
        try:
            servers.append((role, g, _server(rs, g)))
        except Exception as exc:  # noqa: BLE001 - one bad port must not kill the rest
            print(f"pkgcache: could not build server {_label(g)}: {exc}")

    stopping = False

    def stop_all() -> None:
        nonlocal stopping
        stopping = True
        for _, _, s in servers:
            s.should_exit = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop_all)

    async def run(server: uvicorn.Server, cfg: Config) -> None:
        try:
            await server.serve()
        except Exception as exc:  # noqa: BLE001 - one bad port must not kill the rest
            print(f"pkgcache: server {_label(cfg)} exited with error: {exc}")

    tasks = [asyncio.create_task(run(s, g)) for _, g, s in servers]
    print("pkgcache serving → " + ", ".join(_label(g) for _, g, _ in servers))

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
