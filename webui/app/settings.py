"""Backend-wide constants and paths — the one leaf module every layer may import.

Holds only values (paths, host/port, git trust env, the ecosystem label list); no
behaviour and no internal imports, so it can never be part of an import cycle. The
eco→(subdir, ecosystem) mapping and the cache-tree root live in app.manifest, which
scripts/gen_manifest.py also imports as the single source of truth."""
import os
import pathlib

# webui/app/settings.py → app/ → webui/ → repo root.
ROOT = pathlib.Path(__file__).resolve().parent.parent.parent

# The cache state (DVC pointers + manifests + its own git history) lives in its own
# repo under caches/, separate from this code repo. The History panel reads that
# repo's log; manifests live inside it.
CACHE_REPO = ROOT / "caches"
MANIFESTS = CACHE_REPO / "manifests"

HOST = os.environ.get("UI_HOST", "0.0.0.0")
PORT = int(os.environ.get("UI_PORT", "8088"))

# Where the pkgcache process answers (ledger reads, health probes, git maintenance,
# artifact uploads). Defaults to the compose-network alias; point it at localhost —
# or wherever the cache runs — when the backend runs outside the compose network
# (e.g. scripts/serve-ui.sh on the host). UI_-prefixed because the bare
# PKGCACHE_HOST is already the cache process's own BIND address.
PKGCACHE_HOST = os.environ.get("UI_PKGCACHE_HOST", "pkgcache")

# ---- auth (Phase 2) ------------------------------------------------------
# The break-glass superuser: verified from the environment at login and NEVER
# written to the users store. Account management creates ordinary stored accounts;
# this one is the always-present root that can't be demoted, deleted, or shadowed.
# Unset → no root (auth is effectively unconfigured until a superuser exists).
ROOT_USER = os.environ.get("UI_ROOT_USER") or None
ROOT_PASSWORD = os.environ.get("UI_ROOT_PASSWORD") or None

# Opaque server-side session: an HttpOnly cookie carrying a random token the webui
# maps to a username in memory (a restart logs everyone out — acceptable for an ops
# console, and it keeps sessions instantly revocable with no signing-key handling).
SESSION_COOKIE = "pkgcache_session"
SESSION_TTL = int(os.environ.get("UI_SESSION_TTL", str(12 * 3600)))  # seconds

# Mark the session cookie Secure once the console terminates TLS (Phase 5). Off by
# default so the cookie survives today's plain-HTTP console hop.
COOKIE_SECURE = os.environ.get("UI_COOKIE_SECURE", "0").strip().lower() in {"1", "true", "yes", "on"}

# The seven UI ecosystem labels (apt + apk share the apt subdir/ledger). The
# canonical eco→(subdir, ecosystem) mapping is app.manifest.ECOS.
ECOS = ("docker", "npm", "pip", "apt", "apk", "git", "files")

# git refuses a repo owned by another uid ("dubious ownership"); this UI usually
# runs as root in a container against a host-owned checkout. We only ever read our
# own repo, so trust it for every git call via env-based config (no global `git
# config` needed). Merge onto os.environ when invoking git. The mutating side
# (app.gateways.proc) keeps the same trust env so that boundary stays self-contained.
GIT_ENV = {
    "GIT_CONFIG_COUNT": "1",
    "GIT_CONFIG_KEY_0": "safe.directory",
    "GIT_CONFIG_VALUE_0": "*",
    # Never let a git command started inside caches/ walk UP into this code repo
    # (e.g. when the cache repo doesn't exist yet) and report the wrong history.
    "GIT_CEILING_DIRECTORIES": str(ROOT),
}
