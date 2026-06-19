"""The unified contract every ecosystem implements. The rest of the system
(app.py role selection, gen_manifest export, the webui, checkpoint) depends only
on this interface — never on a specific ecosystem. Adding a 5th cache (crates.io,
Go modules, Maven, RubyGems, …) means implementing this and registering it.
"""
from __future__ import annotations

from collections.abc import Iterable
from pathlib import Path
from typing import TYPE_CHECKING, Protocol, runtime_checkable

from starlette.routing import BaseRoute

from .ledger import ArtifactRecord

if TYPE_CHECKING:  # avoid a runtime import cycle
    from . import Core


@runtime_checkable
class Repository(Protocol):
    # ---- identity & wiring (read by app.py / webui / checkpoint) ----
    role: str             # PKGCACHE_ROLE selector: "oci" | "npm" | "pypi" | "apt"
    progress_path: str    # endpoint the webui polls ("/v2/_progress", "/-/progress", …)

    def client_endpoint(self, host: str) -> str:
        """Human 'pull from here' hint shown in the webui endpoints panel."""
        ...

    def mount(self, core: "Core") -> list[BaseRoute]:
        """Routes this repo serves, built on the shared core. The pull-through path
        calls core.cache.fetch(..., on_commit=...) so the SQLite ledger is populated
        natively on every successful cache write — no post-hoc walk."""
        ...

    def rebuild_ledger(self, cache_dir: Path) -> Iterable[ArtifactRecord]:
        """Reconstruct ledger rows by scanning the on-disk cache. Used only by
        `gen_manifest.py --rebuild` when the DB drifts — never on the hot path."""
        ...
