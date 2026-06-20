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
}

sys.path.insert(0, str(ROOT / "scripts"))
import gen_manifest  # noqa: E402  -- defines CACHES + ECOS

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
