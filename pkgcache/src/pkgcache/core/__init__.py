"""Shared core: the primitives every handler is built from.

`Core` bundles the wired-up singletons (config, storage, ledger, upstream,
progress, cache) and is handed to each Repository's mount().
"""
from __future__ import annotations

from dataclasses import dataclass

from .cache import Cache
from .config import Config
from .inflight import InflightRegistry
from .ledger import Ledger
from .progress import Progress
from .stats import Stats
from .storage import Storage
from .upstream import Upstream

__all__ = ["Core", "build_core", "Config", "Ledger", "Storage", "Upstream", "Progress", "Stats", "Cache"]


@dataclass
class Core:
    config: Config
    storage: Storage
    ledger: Ledger
    upstream: Upstream
    progress: Progress
    stats: Stats
    cache: Cache

    async def aclose(self) -> None:
        await self.stats.flush(self.ledger)  # persist the final window before closing
        await self.upstream.aclose()
        self.ledger.close()


def build_core(config: Config) -> Core:
    storage = Storage(config.cache_root, cas_root=config.cas_root)
    storage.gc_parts()  # clear interrupted-download leftovers on startup
    ledger = Ledger(config.cache_root / "ledger.db")
    upstream = Upstream(timeout=config.request_timeout, offline=config.offline)
    progress = Progress()
    stats = Stats()
    cache = Cache(storage, InflightRegistry(), progress, ledger, stats)
    return Core(
        config=config,
        storage=storage,
        ledger=ledger,
        upstream=upstream,
        progress=progress,
        stats=stats,
        cache=cache,
    )
