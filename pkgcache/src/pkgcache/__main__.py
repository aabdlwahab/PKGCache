"""Entrypoint.

Two modes:
  * PKGCACHE_ROLE set   → serve that one role (dev / one-role-per-process).
  * PKGCACHE_ROLE unset → serve ALL roles in ONE process, each on its own port
    (the protocols can't share a port: OCI owns /v2/ at the root and apt is a
    forward proxy). The HTTPS roles terminate TLS in-process from the cert, so no
    separate TLS proxy is needed — one container.

A single event loop runs every uvicorn server, so the in-process progress registry
and single-flight dedup keep their (per-role) single-worker semantics.

Multi-project: load_all() returns the global four roles plus four per registered
project (see core/config.py). A lightweight supervisor re-reads the registry on an
interval and binds the ports of newly-created projects — and drops ports of removed
ones — WITHOUT restarting the process, so creating a project from the control UI
never disturbs in-flight downloads on other ports. (The pool's host ports are
published up front by compose, so no container recreate is needed either.)
"""
from __future__ import annotations

import asyncio
import os
import signal

import uvicorn

from .app import build_app
from .core.config import Config, load, load_all

# How often the supervisor re-reads the project registry to pick up new/removed
# projects. Small enough that "create project" feels live; large enough to be cheap.
_POLL = float(os.environ.get("PKGCACHE_PROJECT_POLL", "5"))


def _server(cfg: Config) -> uvicorn.Server:
    ucfg = uvicorn.Config(
        build_app(cfg),
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
    return f"{cfg.project}/{cfg.role}:{cfg.port}{'(tls)' if cfg.tls_cert else ''}"


async def _serve_all(initial: list[Config]) -> None:
    loop = asyncio.get_running_loop()
    servers: dict[int, uvicorn.Server] = {}   # port -> running server
    tasks: dict[int, asyncio.Task] = {}       # port -> its serve() task
    stopping = False

    def stop_all() -> None:
        nonlocal stopping
        stopping = True
        for s in servers.values():
            s.should_exit = True

    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop_all)

    async def run(server: uvicorn.Server, cfg: Config) -> None:
        try:
            await server.serve()
        except Exception as exc:  # noqa: BLE001 - one bad port must not kill the rest
            print(f"pkgcache: server {_label(cfg)} exited with error: {exc}")

    def start(cfg: Config) -> None:
        try:
            server = _server(cfg)
        except Exception as exc:  # noqa: BLE001 - skip a misconfigured role/project
            print(f"pkgcache: could not build server {_label(cfg)}: {exc}")
            return
        servers[cfg.port] = server
        tasks[cfg.port] = asyncio.create_task(run(server, cfg))

    for cfg in initial:
        start(cfg)
    print("pkgcache serving → " + ", ".join(_label(c) for c in initial))

    # Supervisor: diff the registry against what's running and reconcile.
    while not stopping:
        await asyncio.sleep(_POLL)
        if stopping:
            break
        try:
            desired = {c.port: c for c in load_all()}
        except Exception as exc:  # noqa: BLE001 - a bad registry write shouldn't crash us
            print(f"pkgcache: project registry reload failed: {exc}")
            continue
        for port, cfg in desired.items():
            if port not in servers:
                print(f"pkgcache: binding new project port {_label(cfg)}")
                start(cfg)
        for port in list(servers):
            if port not in desired:
                print(f"pkgcache: releasing removed project port {port}")
                servers[port].should_exit = True
        # Reap finished tasks so a re-created project (same port) can rebind.
        for port, task in list(tasks.items()):
            if task.done():
                tasks.pop(port, None)
                servers.pop(port, None)

    await asyncio.gather(*tasks.values(), return_exceptions=True)


def main() -> None:
    if os.environ.get("PKGCACHE_ROLE", "").strip():
        cfg = load()
        uvicorn.run(build_app(cfg), host=cfg.host, port=cfg.port, workers=1,
                    log_level="info", ssl_certfile=cfg.tls_cert, ssl_keyfile=cfg.tls_key)
    else:
        asyncio.run(_serve_all(load_all()))


if __name__ == "__main__":
    main()
