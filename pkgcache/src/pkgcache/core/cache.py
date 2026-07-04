"""The pull-through facade handlers call: hit → serve; miss → single-flight fetch
with progressive streaming. Wraps storage + inflight + progress + ledger.
"""
from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

from starlette.requests import Request
from starlette.responses import Response, StreamingResponse

from .inflight import Download, InflightRegistry
from .storage import Storage


class Cache:
    def __init__(self, storage: Storage, inflight: InflightRegistry, progress, ledger, stats) -> None:
        self.storage = storage
        self.inflight = inflight
        self.progress = progress
        self.ledger = ledger
        self.stats = stats

    async def fetch(
        self,
        *,
        key: str,
        final_path: Path,
        stream_opener: Callable[[], Any],
        name: str,
        method: str = "GET",
        request: Request | None = None,
        media_type: str | None = None,
        response_headers: dict | None = None,
        expected_sha256: str | None = None,
        expected_size: int | None = None,
        on_commit: Callable[[int, str], Any] | None = None,
        eco: str | None = None,
    ) -> Response:
        """Serve `key` from cache, fetching once through `stream_opener` on a miss.

        `on_commit(size, sha256_hex)` runs after a successful atomic commit and may
        return an ArtifactRecord to write to the ledger. `eco` tags the byte traffic
        for the stats tab (hit rate / bytes saved); the miss path also samples
        upstream bandwidth. Per-package *access* counts are recorded by the handlers
        (they know the package identity, not just the file)."""
        # ---- hit -------------------------------------------------------------
        if final_path.exists():
            size = final_path.stat().st_size
            self.progress.record_recent(key, name, size, hit=True)
            if eco:
                self.stats.traffic(eco, hit=True, nbytes=size)
            return Storage.file_response(
                final_path, media_type=media_type, headers=response_headers, method=method
            )

        # ---- miss: single-flight --------------------------------------------
        dl = self.inflight.start(
            key,
            lambda on_finish: Download(
                key=key,
                final_path=final_path,
                stream_opener=stream_opener,
                storage=self.storage,
                progress=self.progress,
                ledger=self.ledger,
                stats=self.stats,
                eco=eco,
                name=name,
                expected_sha256=expected_sha256,
                expected_size=expected_size,
                on_commit=on_commit,
                on_finish=on_finish,
            ),
        )

        # HEAD or a resume (Range) can't ride the progressive stream cleanly — wait
        # for the committed file, then let FileResponse handle Range/HEAD.
        has_range = request is not None and "range" in request.headers
        if method == "HEAD" or has_range:
            await dl.completion()
            if dl.error is not None or not final_path.exists():
                self.progress.record_recent(key, name, None, hit=False, failed=True)
                raise dl.error or RuntimeError(f"upstream fetch failed: {key}")
            return Storage.file_response(
                final_path,
                media_type=media_type or dl.media_type,
                headers=response_headers,
                method=method,
            )

        # Progressive delivery: wait until upstream headers are known, then stream.
        await dl.headers_ready.wait()
        if dl.error is not None and dl.written == 0:
            self.progress.record_recent(key, name, None, hit=False, failed=True)
            raise dl.error
        headers = dict(response_headers or {})
        if dl.total is not None:
            headers["Content-Length"] = str(dl.total)
        return StreamingResponse(
            dl.reader(),
            media_type=media_type or dl.media_type or "application/octet-stream",
            headers=headers,
        )
