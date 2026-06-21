"""The registry: the single place the system enumerates ecosystems.

app.py instantiates REPOSITORIES[role] PER APP; gen_manifest.py iterates it to
export manifests; the webui can derive its progress paths + endpoint hints from it.
Adding a 5th ecosystem is: implement Repository, add it here, add a compose service.

This maps role → handler CLASS, not a shared instance. A handler holds per-app
state (its bound `core` → storage/ledger/cache_root), and one process serves many
(project, role) apps, so each app MUST get its own instance — a shared singleton
would have every project's requests land in whichever app mounted last.
"""
from __future__ import annotations

from .core.repository import Repository
from .handlers.apt import AptRepo
from .handlers.npm import NpmRepo
from .handlers.oci import OciRepo
from .handlers.pypi import PypiRepo

REPOSITORIES: dict[str, type[Repository]] = {
    cls.role: cls for cls in (OciRepo, NpmRepo, PypiRepo, AptRepo)
}
