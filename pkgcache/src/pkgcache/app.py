"""Build the ASGI app for the selected role.

The role's progress endpoint and /healthz are registered FIRST so they win over
greedy handler routes (the apt forward-proxy catch-all and npm's /{pkg}).
"""
from __future__ import annotations

import asyncio
import contextlib
import json
import os
import time

import httpx
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, StreamingResponse
from starlette.routing import Route

from .core import Core, build_core
from .core.config import GLOBAL, Config, load
from .repositories import REPOSITORIES

# How often the SSE progress stream re-emits a snapshot to connected clients.
_SSE_POLL_SECONDS = 1.0
# How often in-memory usage stats are flushed to the ledger (see core/stats.py).
_STATS_FLUSH_SECONDS = float(os.environ.get("PKGCACHE_STATS_FLUSH_INTERVAL", "30"))
# Scheduled active speed test (one per process, on the global pypi role when online):
# downloads a known file to keep a fresh upstream-bandwidth estimate even in quiet
# periods with no cache misses to sample passively. Empty URL disables it.
_SPEEDTEST_URL = os.environ.get(
    "PKGCACHE_SPEEDTEST_URL", "https://speed.cloudflare.com/__down?bytes=25000000"
)
_SPEEDTEST_SECONDS = float(os.environ.get("PKGCACHE_SPEEDTEST_INTERVAL", "1800"))


def build_app(config: Config | None = None, *, manage_lifecycle: bool = True) -> Starlette:
    """Build the ASGI app for one (project, role). `app.state.core` is the bound Core.

    manage_lifecycle=True attaches a Starlette lifespan that runs the core's
    background tasks and closes it on shutdown — used when the app is served
    directly by uvicorn (single-role dev mode). When the app is mounted as a
    per-project sub-app inside a RoleServer, Starlette does NOT fire a mounted
    app's lifespan, so the RoleServer owns the core lifecycle instead via
    start_core_tasks()/close_core(); pass manage_lifecycle=False there.
    """
    config = config or load()
    repo_cls = REPOSITORIES.get(config.role)
    if repo_cls is None:
        raise SystemExit(f"no Repository registered for role {config.role!r}")
    # A FRESH handler per app: it binds this app's core (storage/ledger/cache_root).
    # One process serves many (project, role) apps, so a shared instance would let
    # the last-mounted core capture every project's requests.
    repo = repo_cls()
    core = build_core(config)

    routes = [
        Route(repo.progress_path, _progress_endpoint(core), methods=["GET"]),
        Route("/healthz", _healthz(config), methods=["GET"]),
        # Ledger admin surface for the control UI — registered BEFORE the handler
        # routes so the apt catch-all / npm's /{pkg} can't shadow them. The webui
        # reads these instead of opening ledger.db directly.
        Route("/+ledger/artifacts", _ledger_artifacts(core), methods=["GET"]),
        Route("/+ledger/stats", _ledger_stats(core), methods=["GET"]),
        *repo.mount(core),
    ]

    @contextlib.asynccontextmanager
    async def lifespan(app):
        tasks = start_core_tasks(core, config)
        try:
            yield
        finally:
            await close_core(tasks, core)

    app = Starlette(routes=routes, lifespan=lifespan if manage_lifecycle else None)
    app.state.core = core
    return app


def start_core_tasks(core: Core, config: Config) -> list[asyncio.Task]:
    """Start a core's background loops (stats flush, plus the one-per-process active
    speed test on the global pypi role). Returns the tasks to cancel at shutdown.
    Must be called with a running event loop."""
    tasks = [asyncio.create_task(_flush_loop(core))]
    if (config.role == "pypi" and config.project == GLOBAL
            and not config.offline and _SPEEDTEST_URL):
        tasks.append(asyncio.create_task(_speedtest_loop(core)))
    return tasks


async def close_core(tasks: list[asyncio.Task], core: Core) -> None:
    """Cancel a core's background tasks and close it (final stats flush + close)."""
    for t in tasks:
        t.cancel()
    await asyncio.gather(*tasks, return_exceptions=True)
    await core.aclose()


async def _flush_loop(core: Core) -> None:
    """Persist accumulated usage stats to the ledger on a fixed cadence."""
    while True:
        await asyncio.sleep(_STATS_FLUSH_SECONDS)
        await core.stats.flush(core.ledger)


async def _speedtest_loop(core: Core) -> None:
    """Periodically measure upstream bandwidth by streaming a known file, so the
    'time saved' estimate stays fresh even when no cache misses are sampling it."""
    while True:
        await asyncio.sleep(_SPEEDTEST_SECONDS)
        try:
            t0 = time.monotonic()
            got = 0
            async with core.upstream.client.stream("GET", _SPEEDTEST_URL) as r:
                if r.status_code != 200:
                    continue
                async for chunk in r.aiter_bytes(1 << 16):
                    got += len(chunk)
            elapsed = time.monotonic() - t0
            if got > 0 and elapsed > 0:
                core.stats.bandwidth(got / elapsed, source="active")
        except (httpx.HTTPError, OSError):
            pass  # a failed probe just means no fresh active sample this round


def _healthz(config: Config):
    async def healthz(request: Request) -> JSONResponse:
        return JSONResponse({
            "status": "ok",
            "role": config.role,
            "project": config.project,
            "offline": config.offline,
        })

    return healthz


def _ledger_artifacts(core: Core):
    """GET /+ledger/artifacts?eco=&q=&sort=&page=&page_size= → artifact rows from this
    (project, role) ledger. page_size<=0 (or omitted for the manifest view) returns
    the full inventory. sqlite runs in a worker thread."""
    def _int(v, default):
        try:
            return int(v)
        except (TypeError, ValueError):
            return default

    async def endpoint(request: Request) -> JSONResponse:
        p = request.query_params
        rows = await core.ledger.aquery(
            ecosystem=p.get("eco") or None,
            q=p.get("q") or None,
            sort=p.get("sort", "name"),
            page=_int(p.get("page"), 1),
            page_size=_int(p.get("page_size"), 0),
        )
        return JSONResponse({"artifacts": rows})

    return endpoint


def _ledger_stats(core: Core):
    """GET /+ledger/stats → this ledger's per-ecosystem usage aggregates + bandwidth
    samples, for the control UI to combine across roles. sqlite in a worker thread."""
    async def endpoint(request: Request) -> JSONResponse:
        return JSONResponse(await core.ledger.astats())

    return endpoint


def _progress_endpoint(core: Core):
    async def endpoint(request: Request):
        if request.query_params.get("sse"):
            async def gen():
                while True:
                    yield f"data: {json.dumps(core.progress.snapshot())}\n\n"
                    await asyncio.sleep(_SSE_POLL_SECONDS)

            return StreamingResponse(gen(), media_type="text/event-stream")
        return JSONResponse(core.progress.snapshot())

    return endpoint
