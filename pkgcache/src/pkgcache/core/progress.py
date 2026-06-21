"""In-process download-progress registry — the unified replacement for the three
hand-applied upstream patches (zot /v2/_progress, verdaccio /-/progress,
apt-cacher-ng /acng-progress). pip gets a live feed for the first time too.

Each role exposes its own progress path (see Repository.progress_path); the JSON
shape is identical across roles and matches what webui/server.py already merges:

    {"downloads": [{id, name, downloaded, total, pct, status, updated}],
     "recent":    [{id, name, size, hit, time}]}

This is process-local state, which is exactly why a role must run a single uvicorn
worker (see app.py) — multiple workers would each hold a partial view.
"""
from __future__ import annotations

import time
from collections import deque
from dataclasses import dataclass, field

# Finished (complete/error) downloads linger this long in the snapshot so a poller
# on webui's ~1.5s cadence still catches them, then they're reaped.
_FINISHED_TTL = 30.0
_RECENT_MAX = 100


@dataclass
class _Download:
    id: str
    name: str
    total: int | None
    downloaded: int = 0
    status: str = "active"  # active | complete | error
    started: float = field(default_factory=time.time)  # set once — the stable sort key
    updated: float = field(default_factory=time.time)

    def as_json(self) -> dict:
        pct = None
        if self.total:
            pct = round(self.downloaded * 100.0 / self.total, 1)
        return {
            "id": self.id,
            "name": self.name,
            "downloaded": self.downloaded,
            "total": self.total,
            "pct": pct,
            "status": self.status,
            "updated": round(self.updated, 3),
        }


class Progress:
    """Tracks in-flight downloads and a rolling log of recent pulls (hit/miss)."""

    def __init__(self) -> None:
        self._active: dict[str, _Download] = {}
        self._recent: deque[dict] = deque(maxlen=_RECENT_MAX)

    # ---- in-flight downloads -------------------------------------------------
    def start(self, dl_id: str, name: str, total: int | None) -> None:
        self._active[dl_id] = _Download(id=dl_id, name=name, total=total)

    def update(self, dl_id: str, downloaded: int) -> None:
        d = self._active.get(dl_id)
        if d is not None:
            d.downloaded = downloaded
            d.updated = time.time()

    def complete(self, dl_id: str) -> None:
        d = self._active.get(dl_id)
        if d is not None:
            d.status = "complete"
            if d.total:
                d.downloaded = d.total
            d.updated = time.time()

    def error(self, dl_id: str) -> None:
        d = self._active.get(dl_id)
        if d is not None:
            d.status = "error"
            d.updated = time.time()

    # ---- recent pulls (log-like feed) ---------------------------------------
    def record_recent(
        self, dl_id: str, name: str, size: int | None, hit: bool, failed: bool = False
    ) -> None:
        """Append a feed entry. `failed=True` marks a pull that could not be served
        (offline cache miss, or upstream error) — the UI renders these as FAIL,
        distinct from a normal upstream MISS (hit=False, failed=False)."""
        self._recent.appendleft(
            {
                "id": dl_id, "name": name, "size": size,
                "hit": hit, "failed": failed, "time": round(time.time(), 3),
            }
        )

    # ---- snapshot ------------------------------------------------------------
    def snapshot(self) -> dict:
        now = time.time()
        # Reap finished entries past their TTL.
        for dl_id, d in list(self._active.items()):
            if d.status != "active" and now - d.updated > _FINISHED_TTL:
                del self._active[dl_id]
        # Stable order: oldest-first by start time, so each row holds its slot as it
        # progresses. (Sorting by last-updated made active rows jump around the panel
        # as each one received bytes — a download that just got a chunk leapt to the
        # top.) A finished row simply drops out when it's reaped or filtered.
        ordered = sorted(self._active.values(), key=lambda d: d.started)
        downloads = [d.as_json() for d in ordered]
        return {"downloads": downloads, "recent": list(self._recent)}
