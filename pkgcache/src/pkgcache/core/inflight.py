"""Single-flight downloads with tail-follow streaming.

One background task per cache key streams upstream → a sibling .part file, hashing
and reporting progress as it goes, and on full success verifies + atomically
commits + records the ledger. Any number of client requests for the same key are
*readers* that tail-follow the growing file (progressive delivery) and converge on
the committed file. Because the download lives in its own task, it keeps filling
the cache even if the triggering client disconnects.
"""
from __future__ import annotations

import asyncio
import contextlib
import hashlib
import time
from collections.abc import AsyncIterator, Awaitable, Callable
from pathlib import Path
from typing import Any

CHUNK = 1 << 16  # 64 KiB
# Only sample bandwidth from misses large enough that transfer time (not latency /
# TLS handshake) dominates — small files would skew the upstream-throughput estimate.
_BW_MIN_BYTES = 2 << 20  # 2 MiB


class IntegrityError(Exception):
    pass


class Download:
    def __init__(
        self,
        *,
        key: str,
        final_path: Path,
        stream_opener: Callable[[], Any],   # returns an async CM yielding httpx.Response
        storage,
        progress,
        ledger,
        name: str,
        expected_sha256: str | None,
        expected_size: int | None,
        on_commit: Callable[[int, str], Any] | None,
        on_finish: Callable[[str], None],
        stats=None,
        eco: str | None = None,
    ) -> None:
        self.key = key
        self.final_path = final_path
        self.tmp_path: Path | None = None
        self._stream_opener = stream_opener
        self._storage = storage
        self._progress = progress
        self._ledger = ledger
        self._stats = stats
        self._eco = eco
        self._name = name
        self._expected_sha256 = expected_sha256
        self._expected_size = expected_size
        self._on_commit = on_commit
        self._on_finish = on_finish

        self.total: int | None = None
        self.media_type: str | None = None
        self.written: int = 0
        self.done: bool = False
        self.error: BaseException | None = None
        self.sha256: str | None = None
        self.headers_ready = asyncio.Event()  # set once total/media_type are known
        self._cond = asyncio.Condition()
        self._task: asyncio.Task | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run())

    async def _pulse(self) -> None:
        async with self._cond:
            self._cond.notify_all()

    async def wait(self) -> None:
        async with self._cond:
            await self._cond.wait()

    async def completion(self) -> None:
        async with self._cond:
            while not self.done:
                await self._cond.wait()

    async def _run(self) -> None:
        h = hashlib.sha256()
        tmp = None
        f = None
        try:
            async with self._stream_opener() as resp:
                resp.raise_for_status()
                cl = resp.headers.get("content-length")
                self.total = int(cl) if cl and cl.isdigit() else None
                self.media_type = resp.headers.get("content-type")
                self._progress.start(self.key, self._name, self.total)
                self.headers_ready.set()
                tmp, fh = self._storage.open_part(self.final_path)
                self.tmp_path = tmp
                f = fh
                await self._pulse()  # readers may now open tmp
                t0 = time.monotonic()  # measure pure transfer (post-connect) for bandwidth
                async for chunk in resp.aiter_bytes(CHUNK):
                    f.write(chunk)
                    h.update(chunk)
                    self.written += len(chunk)
                    self._progress.update(self.key, self.written)
                    await self._pulse()
            f.flush()
            f.close()
            f = None

            hexd = h.hexdigest()
            if self._expected_sha256 and hexd != self._expected_sha256.lower():
                raise IntegrityError(f"sha256 mismatch for {self.key}")
            if self._expected_size is not None and self.written != self._expected_size:
                raise IntegrityError(f"size mismatch for {self.key}")
            if self.total is not None and self.written != self.total:
                raise IntegrityError(f"truncated download for {self.key}")

            self._storage.commit_part(tmp, self.final_path)
            self.sha256 = hexd
            if self._on_commit is not None:
                rec = self._on_commit(self.written, hexd)
                if rec is not None:
                    await self._ledger.arecord(rec)
            self._progress.complete(self.key)
            self._progress.record_recent(self.key, self._name, self.written, hit=False)
            if self._stats is not None and self._eco:
                # A miss = bytes fetched from upstream (counts against "bytes saved").
                self._stats.traffic(self._eco, hit=False, nbytes=self.written)
                elapsed = time.monotonic() - t0
                if self.written >= _BW_MIN_BYTES and elapsed > 0:
                    self._stats.bandwidth(self.written / elapsed, source="passive")
        except BaseException as e:  # noqa: BLE001 — surface to readers, clean up
            self.error = e
            self._progress.error(self.key)
            if f is not None:
                with contextlib.suppress(Exception):
                    f.close()
            if tmp is not None:
                with contextlib.suppress(OSError):
                    Path(tmp).unlink()
        finally:
            self.done = True
            self.headers_ready.set()  # unblock any waiter even on early failure
            await self._pulse()
            self._on_finish(self.key)

    async def reader(self) -> AsyncIterator[bytes]:
        """Tail-follow the growing file until the download completes."""
        # Wait until the .part exists (or the download already finished/failed).
        while self.tmp_path is None and not self.done and self.error is None:
            await self.wait()
        path = self.final_path if self.final_path.exists() else self.tmp_path
        if path is None or not Path(path).exists():
            if self.error is not None:
                raise self.error
            return
        f = open(path, "rb")
        pos = 0
        try:
            while True:
                written, done, err = self.written, self.done, self.error
                if pos < written:
                    f.seek(pos)
                    data = f.read(written - pos)
                    if data:
                        pos += len(data)
                        yield data
                        continue
                if done:
                    f.seek(pos)
                    data = f.read()
                    if data:
                        yield data
                    break
                if err is not None:
                    break
                async with self._cond:
                    if self.written == written and not self.done:
                        await self._cond.wait()
        finally:
            f.close()


class InflightRegistry:
    """Maps cache key → live Download. Entries self-remove when the task ends."""

    def __init__(self) -> None:
        self._downloads: dict[str, Download] = {}

    def get(self, key: str) -> Download | None:
        return self._downloads.get(key)

    def start(
        self,
        key: str,
        make: Callable[[Callable[[str], None]], Download],
    ) -> Download:
        existing = self._downloads.get(key)
        if existing is not None:
            return existing
        dl = make(self._remove)
        self._downloads[key] = dl
        dl.start()
        return dl

    def _remove(self, key: str) -> None:
        self._downloads.pop(key, None)
