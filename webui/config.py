"""Shared constants + paths for the control UI's API modules.

Split out of the old monolithic server.py so the HTTP layer (server.py) stays
thin and the data/jobs/live modules can be imported and tested independently.
gen_manifest gives us the CACHES path + the eco→(subdir, ecosystem) map; importing
it has no side effects.
"""
import os
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
# The cache state (DVC pointers + manifests + its own git history) lives in its
# own repo under caches/, separate from this code repo. The History panel reads
# that repo's log; manifests live inside it.
CACHE_REPO = ROOT / "caches"
MANIFESTS = CACHE_REPO / "manifests"
WEBROOT = pathlib.Path(__file__).resolve().parent

HOST = os.environ.get("UI_HOST", "0.0.0.0")
PORT = int(os.environ.get("UI_PORT", "8088"))

# git refuses a repo owned by another uid ("dubious ownership"); this UI usually
# runs as root in a container against a host-owned checkout. We only ever read
# our own repo, so trust it for every git call via env-based config (no global
# `git config` needed). Merge onto os.environ when invoking git. The mutating
# side keeps its own copy of this in ops.py so that module stays self-contained.
GIT_ENV = {
    "GIT_CONFIG_COUNT": "1",
    "GIT_CONFIG_KEY_0": "safe.directory",
    "GIT_CONFIG_VALUE_0": "*",
    # Never let a git command started inside caches/ walk UP into this code repo
    # (e.g. when the cache repo doesn't exist yet) and report the wrong history.
    "GIT_CEILING_DIRECTORIES": str(ROOT),
}

sys.path.insert(0, str(ROOT / "scripts"))
import gen_manifest  # noqa: E402  -- defines CACHES + ECOS

import projects  # noqa: E402  -- the project registry (ports per project)

ECOS = ("docker", "npm", "pip", "apt", "apk", "git", "files")

# Where clients pull from, per ecosystem — shown verbatim in the UI's endpoints panel.
ENDPOINTS = {
    "docker": "<host>:5000        (pull <host>:5000/{dockerhub,ghcr,quay}/<image>)",
    "npm": "https://<host>:4873/",
    "pip": "https://<host>:3141/root/pypi/+simple/",
    "apt": "http://<host>:3142/",
    "apk": "http://<host>:3142/        (apk: set http_proxy to this, HTTP repos)",
    "git": "https://<host>:3143/<upstream-host>/<owner>/<repo>.git   (insteadOf github.com etc.)",
    "files": "https://<host>:3144/<path>   (wget --ca-certificate=ca.crt; PUT needs the write token)",
}

# Each role runs in the single `pkgcache` container on its own port; we poll their
# /_progress endpoints and aggregate. HTTPS roles terminate TLS in-process with the
# private CA, so internal polls hit https:// with verification skipped. The
# `pkgcache` hostname only resolves inside the compose network.
PROGRESS_SOURCES = {
    "docker": "https://pkgcache:5000/v2/_progress",
    "npm": "https://pkgcache:4873/-/progress",
    "pip": "https://pkgcache:3141/+progress",
    "apt": "http://pkgcache:3142/acng-progress",
    "git": "https://pkgcache:3143/+progress",
    "files": "https://pkgcache:3144/+progress",
}

# Each role serves /healthz → {status, role, offline}. Probing these gives the
# real "N roles up" count and the true online/offline state (vs guessing from
# compose). Same hosts/ports as the progress feeds.
HEALTH_SOURCES = {
    "docker": "https://pkgcache:5000/healthz",
    "npm": "https://pkgcache:4873/healthz",
    "pip": "https://pkgcache:3141/healthz",
    "apt": "http://pkgcache:3142/healthz",
    "git": "https://pkgcache:3143/healthz",
    "files": "https://pkgcache:3144/healthz",
}

# ---- per-project derivation ----------------------------------------------
# The dicts above describe the GLOBAL project (root URLs, no prefix). Every project
# now shares those same ports; a named project is reached by a URL PREFIX on the
# same port (see pkgcache/router.py + projects.role_prefix). We derive its progress
# and endpoint URLs by inserting that prefix. Health is per-SERVER now (all projects
# share one process per role), so it stays the global set for everyone.
#
# _PROGRESS_PATH is keyed by eco (docker/npm/pip/apt/git/files) and is the path the
# role's sub-app serves; _ECO_ROLE maps eco → pkgcache role for prefix building.
_PROGRESS_PATH = {"docker": "/v2/_progress", "npm": "/-/progress",
                  "pip": "/+progress", "apt": "/acng-progress", "git": "/+progress",
                  "files": "/+progress"}
_ECO_ROLE = {eco: role for role, eco in projects.ROLE_SUBDIR.items()}  # docker→oci, pip→pypi, …
_ECO_SCHEME = {eco: ("http" if role == "apt" else "https") for eco, role in _ECO_ROLE.items()}


def _project_progress_path(project, eco):
    """External progress path for a project's role, accounting for the router's
    prefix-strip. OCI's routes are pinned under the protocol-fixed /v2 root, so the
    project segment is inserted right after /v2; the others take a leading prefix."""
    rel = _PROGRESS_PATH[eco]
    if project == projects.GLOBAL:
        return rel
    role = _ECO_ROLE[eco]
    if role == "oci":
        return f"/v2/{project}" + rel[len("/v2"):]   # /v2/_progress → /v2/<project>/_progress
    return f"/{project}/{role}{rel}"                 # /<project>/<role>/…


def progress_sources(project=projects.GLOBAL):
    """{eco: progress URL} on the `pkgcache` container for THIS project (per-project
    progress registries live behind the shared per-role ports, reached by prefix)."""
    if project == projects.GLOBAL:
        return dict(PROGRESS_SOURCES)
    return {
        eco: f"{_ECO_SCHEME[eco]}://pkgcache:{projects.ROLE_PORT[_ECO_ROLE[eco]]}"
             f"{_project_progress_path(project, eco)}"
        for eco in _PROGRESS_PATH
    }


def health_sources(project=projects.GLOBAL):
    """{eco: /healthz URL} on the `pkgcache` container. All projects share one server
    per role, so health is per-server: the global set answers for every project."""
    return dict(HEALTH_SOURCES)


def pypi_internal(project=projects.GLOBAL):
    """(internal base URL on the `pkgcache` container, public path prefix) for a
    project's pypi role. The lock warmer drives the internal base to pull each locked
    file into the cache; the public prefix is what the rewritten lock's URLs carry
    (empty for global, `/<project>/pypi` for a named project)."""
    prefix = projects.role_prefix(project, "pypi")
    return f"https://pkgcache:{projects.ROLE_PORT['pypi']}{prefix}", prefix


def endpoints(project=projects.GLOBAL):
    """Client-facing pull URLs per ecosystem, shown in the UI. Global keeps its
    hand-written hints; a named project gets the same shapes with its URL prefix
    (apt/apk carry the project as the proxy username, since a forward proxy has no
    path to prefix)."""
    if project == projects.GLOBAL:
        return dict(ENDPOINTS)
    oci = projects.ROLE_PORT["oci"]
    npm = f"/{project}/npm"
    pip = f"/{project}/pypi"
    return {
        "docker": f"<host>:{oci}        (pull <host>:{oci}/{project}/{{dockerhub,ghcr,quay}}/<image>)",
        "npm": f"https://<host>:{projects.ROLE_PORT['npm']}{npm}/",
        "pip": f"https://<host>:{projects.ROLE_PORT['pypi']}{pip}/root/pypi/+simple/",
        "apt": f"http://{project}@<host>:{projects.ROLE_PORT['apt']}/        (apt: proxy username = project)",
        "apk": f"http://{project}@<host>:{projects.ROLE_PORT['apt']}/        (apk: http_proxy with this user)",
        "git": f"https://<host>:{projects.ROLE_PORT['git']}/{project}/git/<upstream-host>/<owner>/<repo>.git",
        "files": f"https://<host>:{projects.ROLE_PORT['files']}/{project}/files/<path>   (wget; PUT needs the write token)",
    }
