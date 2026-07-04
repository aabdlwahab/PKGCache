"""In-process usage stats, flushed to the ledger periodically.

Recording a request must NOT touch SQLite on the hot path (a uv sync fires
thousands of file GETs in a burst), so we accumulate deltas in memory and a
background task (see app.py) flushes them to the ledger every ~30 s and on
shutdown. Worst case a crash loses the last window — fine for usage stats.

Three things are tracked, all keyed for the leaderboard / hit-rate / time-saved
views the stats tab renders:
  * access   — per (ecosystem, package) request count + last-access time (LRU + leaderboard)
  * traffic  — per-ecosystem hit/miss counts and bytes (hit rate + bytes saved)
  * bandwidth— upstream throughput samples (passive from misses, active from speed tests)
"""
from __future__ import annotations

import threading
import time


class Stats:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._access: dict[tuple[str, str], list] = {}   # (eco,name) -> [count, last_ts]
        self._traffic: dict[str, list] = {}              # eco -> [hit_c, hit_b, miss_c, miss_b]
        self._bandwidth: list[tuple[float, float, str]] = []  # (ts, bps, source)

    # ---- recording (cheap, in-memory; called on the event loop) -------------
    def access(self, ecosystem: str, name: str) -> None:
        with self._lock:
            a = self._access.setdefault((ecosystem, name), [0, 0.0])
            a[0] += 1
            a[1] = time.time()

    def traffic(self, ecosystem: str, *, hit: bool, nbytes: int) -> None:
        with self._lock:
            t = self._traffic.setdefault(ecosystem, [0, 0, 0, 0])
            if hit:
                t[0] += 1
                t[1] += nbytes
            else:
                t[2] += 1
                t[3] += nbytes

    def bandwidth(self, bps: float, source: str = "passive") -> None:
        if bps <= 0:
            return
        with self._lock:
            self._bandwidth.append((time.time(), bps, source))

    # ---- flush --------------------------------------------------------------
    def _drain(self) -> tuple[dict, dict, list]:
        with self._lock:
            access = {k: (v[0], v[1]) for k, v in self._access.items()}
            traffic = {k: tuple(v) for k, v in self._traffic.items()}
            bandwidth = list(self._bandwidth)
            self._access.clear()
            self._traffic.clear()
            self._bandwidth.clear()
        return access, traffic, bandwidth

    async def flush(self, ledger) -> None:
        """Persist accumulated deltas. Run off the event loop (sqlite is blocking)."""
        import asyncio

        access, traffic, bandwidth = self._drain()
        if not (access or traffic or bandwidth):
            return
        try:
            await asyncio.to_thread(ledger.apply_stats, access, traffic, bandwidth)
        except Exception:  # noqa: BLE001 — never let a flush failure crash the loop
            # Best-effort: drop this window rather than re-buffer (avoids unbounded growth).
            pass
