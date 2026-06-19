"""Entrypoint.

Two modes:
  * PKGCACHE_ROLE set   → serve that one role (dev / one-role-per-process).
  * PKGCACHE_ROLE unset → serve ALL four roles in ONE process, each on its own
    port (the protocols can't share a port: OCI owns /v2/ at the root and apt is a
    forward proxy). The HTTPS roles terminate TLS in-process from the cert, so no
    separate TLS proxy is needed — one container.

A single event loop runs all four uvicorn servers, so the in-process progress
registry and single-flight dedup keep their (per-role) single-worker semantics.
"""
from __future__ import annotations

import asyncio
import os
import signal

import uvicorn

from .app import build_app
from .core.config import Config, load, load_all


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


async def _serve_all(configs: list[Config]) -> None:
    servers = [_server(c) for c in configs]
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, lambda: [setattr(s, "should_exit", True) for s in servers])
    roles = ", ".join(f"{c.role}:{c.port}{'(tls)' if c.tls_cert else ''}" for c in configs)
    print(f"pkgcache serving roles → {roles}")
    await asyncio.gather(*(s.serve() for s in servers))


def main() -> None:
    if os.environ.get("PKGCACHE_ROLE", "").strip():
        cfg = load()
        uvicorn.run(build_app(cfg), host=cfg.host, port=cfg.port, workers=1,
                    log_level="info", ssl_certfile=cfg.tls_cert, ssl_keyfile=cfg.tls_key)
    else:
        asyncio.run(_serve_all(load_all()))


if __name__ == "__main__":
    main()
