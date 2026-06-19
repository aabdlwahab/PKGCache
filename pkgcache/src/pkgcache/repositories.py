"""The registry: the single place the system enumerates ecosystems.

app.py mounts REPOSITORIES[role]; gen_manifest.py iterates it to export manifests;
the webui can derive its progress paths + endpoint hints from it. Adding a 5th
ecosystem is: implement Repository, add it here, add a compose service.
"""
from __future__ import annotations

from .core.repository import Repository
from .handlers.apt import AptRepo
from .handlers.npm import NpmRepo
from .handlers.oci import OciRepo
from .handlers.pypi import PypiRepo

REPOSITORIES: dict[str, Repository] = {
    r.role: r for r in (OciRepo(), NpmRepo(), PypiRepo(), AptRepo())
}
