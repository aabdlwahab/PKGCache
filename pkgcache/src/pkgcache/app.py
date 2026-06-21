"""Build the ASGI app for the selected role.

The role's progress endpoint and /healthz are registered FIRST so they win over
greedy handler routes (the apt forward-proxy catch-all and npm's /{pkg}).
"""
from __future__ import annotations

import asyncio
import contextlib
import json

from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, StreamingResponse
from starlette.routing import Route

from .core import Core, build_core
from .core.config import Config, load
from .repositories import REPOSITORIES

# How often the SSE progress stream re-emits a snapshot to connected clients.
_SSE_POLL_SECONDS = 1.0


def build_app(config: Config | None = None) -> Starlette:
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
        *repo.mount(core),
    ]

    @contextlib.asynccontextmanager
    async def lifespan(app):
        yield
        await core.aclose()

    app = Starlette(routes=routes, lifespan=lifespan)
    app.state.core = core
    return app


def _healthz(config: Config):
    async def healthz(request: Request) -> JSONResponse:
        return JSONResponse({
            "status": "ok",
            "role": config.role,
            "project": config.project,
            "offline": config.offline,
        })

    return healthz


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
