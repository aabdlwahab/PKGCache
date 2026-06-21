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

ECOS = ("docker", "npm", "pip", "apt", "apk")

# Where clients pull from, per ecosystem — shown verbatim in the UI's endpoints panel.
ENDPOINTS = {
    "docker": "<host>:5000        (pull <host>:5000/{dockerhub,ghcr,quay}/<image>)",
    "npm": "https://<host>:4873/",
    "pip": "https://<host>:3141/root/pypi/+simple/",
    "apt": "http://<host>:3142/",
    "apk": "http://<host>:3142/        (apk: set http_proxy to this, HTTP repos)",
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
}

# Each role serves /healthz → {status, role, offline}. Probing these gives the
# real "N roles up" count and the true online/offline state (vs guessing from
# compose). Same hosts/ports as the progress feeds.
HEALTH_SOURCES = {
    "docker": "https://pkgcache:5000/healthz",
    "npm": "https://pkgcache:4873/healthz",
    "pip": "https://pkgcache:3141/healthz",
    "apt": "http://pkgcache:3142/healthz",
}

# ---- per-project derivation ----------------------------------------------
# The dicts above describe the GLOBAL project (the default ports). A named project
# serves the SAME paths on its own allocated ports, so we derive its progress /
# health / endpoint URLs from the registry instead of hard-coding them. role→eco
# label (oci↔docker, pypi↔pip) comes from the registry's ROLE_SUBDIR.
# Keyed by eco label (the role→eco map turns oci→docker, pypi→pip).
_PROGRESS_PATH = {"docker": "/v2/_progress", "npm": "/-/progress",
                  "pip": "/+progress", "apt": "/acng-progress"}


def _eco_ports(project):
    """{eco_label: (scheme, port)} for a project (apt is plain HTTP, rest HTTPS)."""
    role_ports = projects.ports(project)
    out = {}
    for role, eco in projects.ROLE_SUBDIR.items():
        scheme = "http" if role == "apt" else "https"
        out[eco] = (scheme, role_ports[role])
    return out


def progress_sources(project=projects.GLOBAL):
    """{eco: progress URL} on the `pkgcache` container, for THIS project's ports."""
    if project == projects.GLOBAL:
        return dict(PROGRESS_SOURCES)
    return {
        eco: f"{scheme}://pkgcache:{port}{_PROGRESS_PATH[eco]}"
        for eco, (scheme, port) in _eco_ports(project).items()
    }


def health_sources(project=projects.GLOBAL):
    """{eco: /healthz URL} on the `pkgcache` container, for THIS project's ports."""
    if project == projects.GLOBAL:
        return dict(HEALTH_SOURCES)
    return {
        eco: f"{scheme}://pkgcache:{port}/healthz"
        for eco, (scheme, port) in _eco_ports(project).items()
    }


def endpoints(project=projects.GLOBAL):
    """Client-facing pull URLs per ecosystem, shown in the UI. Global keeps its
    hand-written hints; a named project gets the same shapes on its own ports."""
    if project == projects.GLOBAL:
        return dict(ENDPOINTS)
    ep = _eco_ports(project)
    _, oci = ep["docker"]
    _, npm = ep["npm"]
    _, pip = ep["pip"]
    _, apt = ep["apt"]
    return {
        "docker": f"<host>:{oci}        (pull <host>:{oci}/{{dockerhub,ghcr,quay}}/<image>)",
        "npm": f"https://<host>:{npm}/",
        "pip": f"https://<host>:{pip}/root/pypi/+simple/",
        "apt": f"http://<host>:{apt}/",
        "apk": f"http://<host>:{apt}/        (apk: set http_proxy to this, HTTP repos)",
    }
