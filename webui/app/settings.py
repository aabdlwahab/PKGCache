"""Backend-wide constants and paths — the one leaf module every layer may import.

Holds only values (paths, host/port, git trust env, the ecosystem label list); no
behaviour and no internal imports, so it can never be part of an import cycle. The
eco→(subdir, ecosystem) mapping and the cache-tree root live in app.manifest, which
scripts/gen_manifest.py also imports as the single source of truth."""
import os
import pathlib
import urllib.parse

# webui/app/settings.py → app/ → webui/ → repo root.
ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
CA_CERT = pathlib.Path(os.environ.get("UI_CA_CERT", str(ROOT / "certs" / "ca.crt")))

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
if (ROOT_USER is None) != (ROOT_PASSWORD is None):
    raise RuntimeError("UI_ROOT_USER and UI_ROOT_PASSWORD must be set together")

# Opaque server-side session: an HttpOnly cookie carrying a random token the webui
# maps to a username in memory (a restart logs everyone out — acceptable for an ops
# console, and it keeps sessions instantly revocable with no signing-key handling).
SESSION_COOKIE = "pkgcache_session"
SESSION_TTL = int(os.environ.get("UI_SESSION_TTL", str(12 * 3600)))  # seconds
MAX_JSON_BYTES = int(os.environ.get("UI_MAX_JSON_MB", "64")) * 1024 * 1024
if SESSION_TTL <= 0:
    raise RuntimeError("UI_SESSION_TTL must be positive")
if MAX_JSON_BYTES <= 0:
    raise RuntimeError("UI_MAX_JSON_MB must be positive")

# Set this when TLS terminates in front of the plain-HTTP console hop. Besides
# documenting the one accepted browser origin, it lets CSRF checks work when the
# internal proxy scheme differs from the external scheme.
PUBLIC_ORIGIN = os.environ.get("UI_PUBLIC_ORIGIN", "").strip().rstrip("/") or None
if PUBLIC_ORIGIN:
    parsed_origin = urllib.parse.urlsplit(PUBLIC_ORIGIN)
    if (
        parsed_origin.scheme not in ("http", "https")
        or parsed_origin.hostname is None
        or parsed_origin.username is not None
        or parsed_origin.password is not None
        or parsed_origin.path
        or parsed_origin.query
        or parsed_origin.fragment
    ):
        raise RuntimeError("UI_PUBLIC_ORIGIN must be an HTTP(S) origin without a path")

# HTTPS public origins default to Secure cookies. An explicit setting remains
# available for unusual proxy topologies.
cookie_secure = os.environ.get("UI_COOKIE_SECURE", "").strip()
COOKIE_SECURE = (
    cookie_secure.lower() in {"1", "true", "yes", "on"}
    if cookie_secure
    else bool(PUBLIC_ORIGIN and parsed_origin.scheme == "https")
)

# Whether to trust the fronting proxy's X-Forwarded-For / X-Forwarded-Proto. The
# shipped console (nginx) OVERWRITES X-Forwarded-For with the real peer, so this is
# safe and lets per-client login throttling and the CSRF scheme check see the real
# client. Set UI_TRUST_PROXY=0 when webui is reachable WITHOUT such a proxy: an
# untrusted client can otherwise spoof its throttle key (defeating brute-force
# lockout) via the header. On by default because webui is internal-only by design.
TRUST_PROXY = os.environ.get("UI_TRUST_PROXY", "1").strip().lower() not in {"0", "false", "no", "off"}

# Anonymous read-only console. When auth is enabled, an un-authenticated caller is
# normally rejected (401) on every endpoint. With this on, a caller WITHOUT a session
# may still perform safe reads (GET/HEAD) — browse packages, stats, downloads, health
# — while every mutation (checkpoint/export/mode, project + account management, token
# rotation, artifact upload/delete) still requires a login. Off by default so turning
# on auth locks the console down fully; opt in when you want a public read-only view.
ANON_READ = os.environ.get("UI_ANON_READ", "0").strip().lower() in {"1", "true", "yes", "on"}

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
