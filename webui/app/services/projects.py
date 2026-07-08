"""The project registry: the single source of truth for what projects exist and
where each one's cache + version-control repo sits.

One central pkgcache process serves every project on TWO ports: the unified HTTPS
port carries docker (/v2/…, project in the image name) plus npm/pypi/git/files
(always fully qualified `/<project>/<role>/…`, global included as `/global/…`), and
the apt/apk forward proxy has its own plain-HTTP port (project = proxy username).
See pkgcache/unified.py + pkgcache/router.py. A "project" is thus just a name + a
cache dir + its own git+DVC repo. The GLOBAL project is never stored in the
registry; on the wire it is simply the reserved name `global`.

Stored as JSON (not YAML) because the control UI is deliberately stdlib-only and
the pkgcache side already has `json`; both processes read the SAME file:

    {
      "projects": {"projA": {}, "projB": {}},
      "tokens":   {"projA": "<files write token>", "global": "…"},
      "offline":  {"projA": true},
      "owners":   {"projA": "alice"}
    }

"offline" holds per-project SOFT offline flags (global included; absent = online):
the pkgcache supervisor applies a change on its next registry poll, serving that
one project cache-only with no restart. The OFFLINE env on the cache container is
the separate instance-wide HARD mode (the air-gap guarantee) and always wins.

"owners" maps a project to the username that owns it (an admin or superuser). An
ABSENT owner means superuser-owned — which is how global and every pre-auth project
read, so ownership needs no migration. pkgcache ignores this map entirely; only the
webui's authorization layer reads it.

Project entries are now empty objects (no ports to allocate); the name is all the
routing needs. Older registries that still carry per-project port maps and a "pool"
block are read fine — the ports are simply ignored.

This module is shared by the operations service (per-project VC + shuttle) and the
HTTP layer (project CRUD + scoped reads); pkgcache reads the same file independently
in pkgcache/core/config.py."""
import os
import re
import secrets

from app import settings
from app.errors import ApiError
from app.gateways import registry as _registry

ROOT = settings.ROOT

# The global project's cache repo (unchanged). Per-project repos live beside it
# under caches/projects/<name>/ — each its own git+DVC repo, so a per-project
# checkpoint / rollback / shuttle only ever touches that project's state. Kept as a
# rebindable module attribute so tests can point it at a sandbox.
CACHE_REPO = settings.CACHE_REPO
PROJECTS_SUBDIR = "projects"

# Registry file I/O lives in the registry gateway (shared config/projects.json, the
# same file pkgcache reads). Re-exported here so callers — and tests — keep using
# projects.load_registry / projects.save_registry unchanged.
load_registry = _registry.load
save_registry = _registry.save

GLOBAL = "global"  # the implicit default project (default ports, caches/ repo)

# pkgcache role names (what the registry + pkgcache config key on) and the six
# cache subdirs / UI ecosystem labels they map to. apt's subdir also holds apk.
# Every HTTPS role shares the ONE unified port; apt keeps its own plain-HTTP proxy
# port (a TLS forward proxy would break busybox/apk and apt < 1.6). The map stays
# keyed by role so URL builders don't special-case anything but the scheme.
ROLES = ("oci", "npm", "pypi", "apt", "git", "files")
UNIFIED_PORT = int(os.environ.get("PKGCACHE_UNIFIED_PORT", "8443"))
APT_PORT = 3142
ROLE_PORT = {role: (APT_PORT if role == "apt" else UNIFIED_PORT) for role in ROLES}
ROLE_SUBDIR = {"oci": "docker", "npm": "npm", "pypi": "pip", "apt": "apt", "git": "git", "files": "files"}

# A project name is the FIRST path/name segment for every role (/<project>/<role>/…
# for npm/pypi/git/files, /v2/<project>/… in an OCI image name, the proxy username
# for apt), so it must be legal as a Docker image-name component AND a URL segment:
# lowercase alnum separated by single . _ or -, 1–40 chars. This is stricter than
# the old dash-only rule because the OCI image-name grammar is the tightest client.
_NAME_RE = re.compile(r"^[a-z0-9]+(?:[._-][a-z0-9]+)*$")

# Names a project may NOT take, because the routers give them another meaning on a
# shared port: the role names themselves (second path segment), the OCI upstream
# aliases (first image-name segment on the global oci app), the global project, the
# default pypi index prefix, and the registry API root. Reserving them keeps
# /<project>/<role>/… and /v2/<project>/… unambiguous.
RESERVED_NAMES = frozenset({
    GLOBAL, "root", "v2",
    "oci", "npm", "pypi", "apt", "git", "files",
    "dockerhub", "ghcr", "quay",
})


class ProjectError(ApiError):
    """A bad project request (invalid name, duplicate, reserved name). Maps to a
    400 via the ApiError contract."""


def validate_name(name):
    name = (name or "").strip()
    if not (1 <= len(name) <= 40) or not _NAME_RE.fullmatch(name):
        raise ProjectError(
            "project name must be 1–40 chars, lowercase letters/digits separated by "
            "single '.', '_' or '-' (no leading/trailing/doubled separators)"
        )
    if name in RESERVED_NAMES:
        raise ProjectError(f"'{name}' is a reserved name")
    return name


# ---- queries -------------------------------------------------------------

def repo_dir(project):
    """The git+DVC repo dir for a project. Global → caches/ (unchanged)."""
    if not project or project == GLOBAL:
        return CACHE_REPO
    return CACHE_REPO / PROJECTS_SUBDIR / project


def ports(project, registry=None):
    """{role: port} for a project. Every project shares the fixed default ports now
    (the project lives in the request path/name, not the port), so this is the same
    map for all — kept as a function so callers that derive URLs stay unchanged.
    Still validates the project exists so a typo surfaces as a 400."""
    if not project or project == GLOBAL:
        return dict(ROLE_PORT)
    registry = registry if registry is not None else load_registry()
    if project not in registry["projects"]:
        raise ProjectError(f"no such project: {project}")
    return dict(ROLE_PORT)


def role_prefix(project, role):
    """The path prefix a project's role is reached under on its port — UNIFORM for
    every role and every project (global included): "/<project>/<role>". On the
    unified port every npm/pypi/git/files URL is fully qualified this way; for oci
    and apt this form is the internal ADMIN surface (healthz, progress, +ledger,
    +maintain — the routers strip it), while their client protocols carry the
    project differently (oci: in the image name under /v2; apt: proxy username)."""
    return f"/{project or GLOBAL}/{role}"


def exists(project, registry=None):
    if project == GLOBAL:
        return True
    registry = registry if registry is not None else load_registry()
    return project in registry["projects"]


def is_offline(project, registry=None):
    """Whether the project's soft offline flag is set. This is the flag the webui
    controls; the effective mode also ORs in the cache container's OFFLINE env,
    which health probes (not this registry) report."""
    registry = registry if registry is not None else load_registry()
    return bool(registry.get("offline", {}).get(project))


def owner(project, registry=None):
    """The username that owns a project, or None for superuser-owned (global and any
    project created before ownership existed). The authorization layer maps None to
    'only a superuser may touch this'."""
    registry = registry if registry is not None else load_registry()
    return registry.get("owners", {}).get(project)


def list_projects():
    """[{name, ports, repo, offline}] for every project including the implicit
    global one, so the UI can render one entry per project (global first). `ports`
    is the shared default-port map (all projects share ports; they differ by URL
    prefix); `offline` is the project's soft flag."""
    registry = load_registry()
    out = [{
        "name": GLOBAL,
        "ports": dict(ROLE_PORT),
        "repo": str(CACHE_REPO),
        "default": True,
        "offline": is_offline(GLOBAL, registry),
        "owner": owner(GLOBAL, registry),
    }]
    for name in sorted(registry["projects"]):
        out.append({
            "name": name,
            "ports": dict(ROLE_PORT),
            "repo": str(repo_dir(name)),
            "default": False,
            "offline": is_offline(name, registry),
            "owner": owner(name, registry),
        })
    return out


# ---- mutations -----------------------------------------------------------
# Each mutation holds _registry.LOCK across the whole load→mutate→save so two
# concurrent creates/deletes can't race on the read-modify-write (the webui is the
# only writer; pkgcache only reads, over the gateway's atomic temp→rename).


def create(name, *, probe=True, owner=None):
    """Register a new project and create its cache subdirs so a checkpoint (and the
    live cache reads) work immediately. Returns the new project's record. `owner` (the
    creating admin/superuser) is recorded so the authorization layer can gate the
    project; None leaves it superuser-owned. There are no ports to allocate — the
    pkgcache process notices the new name on its next registry poll and starts routing
    `/<name>/<role>/…` to it, no restart or rebind. `probe` is accepted for call-site
    compatibility and ignored."""
    name = validate_name(name)
    with _registry.LOCK:
        registry = load_registry()
        if name in registry["projects"]:
            raise ProjectError(f"project already exists: {name}")
        registry["projects"][name] = {}
        if owner:
            registry.setdefault("owners", {})[name] = owner
        save_registry(registry)
    # Pre-create the per-role cache subdirs so the first checkpoint has something to
    # `dvc add` even before any artifact is cached, and the live reads don't 404.
    base = repo_dir(name)
    for subdir in ROLE_SUBDIR.values():
        (base / subdir).mkdir(parents=True, exist_ok=True)
    return {"name": name, "ports": dict(ROLE_PORT), "repo": str(base), "owner": owner}


def delete(name):
    """Remove a project from the registry. The on-disk cache tree is left in place —
    deleting cached bytes is a separate, explicit step the operator takes, never a
    side effect of dropping the entry."""
    name = validate_name(name)
    with _registry.LOCK:
        registry = load_registry()
        if name not in registry["projects"]:
            raise ProjectError(f"no such project: {name}")
        del registry["projects"][name]
        registry.get("tokens", {}).pop(name, None)   # drop its files write token too
        registry.get("offline", {}).pop(name, None)  # …and its soft offline flag
        registry.get("owners", {}).pop(name, None)   # …and its ownership record
        save_registry(registry)
    return {"name": name, "repo": str(repo_dir(name))}


def set_owner(name, username):
    """Reassign a project to a different owner (a superuser action). Validates the
    project exists; the caller validates that `username` is an admin/superuser (that
    check needs the accounts store, which this module deliberately does not know)."""
    name = validate_name(name)
    with _registry.LOCK:
        registry = load_registry()
        if name not in registry["projects"]:
            raise ProjectError(f"no such project: {name}")
        registry.setdefault("owners", {})[name] = username
        save_registry(registry)
    return {"name": name, "owner": username}


def set_offline(name, offline):
    """Set/clear a project's soft offline flag (global included). A registry write
    the always-on cache process applies on its next poll (~5s): that one project
    serves cache-only, others are untouched, nothing restarts. Stored sparsely —
    online means no entry, so the file never accumulates no-op false rows."""
    name = _validate_project(name)
    with _registry.LOCK:
        registry = load_registry()
        if name != GLOBAL and name not in registry["projects"]:
            raise ProjectError(f"no such project: {name}")
        flags = registry.setdefault("offline", {})
        if offline:
            flags[name] = True
        else:
            flags.pop(name, None)
        save_registry(registry)
    return {"name": name, "offline": bool(offline)}


def _validate_project(project):
    """A project a per-project setting (write token, offline flag) may belong to:
    the implicit global, or a syntactically valid name. Existence is checked by the
    mutation itself, under the registry lock."""
    if project == GLOBAL:
        return GLOBAL
    return validate_name(project)


# ---- files write tokens --------------------------------------------------
# The files role is the only write path. GET/HEAD stay anonymous; PUT/DELETE
# require a per-project token generated here and stored in the SAME registry file
# pkgcache reads (it verifies against it, mtime-cached — see core/config.files_token).


def has_write_token(project, registry=None):
    registry = registry if registry is not None else load_registry()
    return bool(registry.get("tokens", {}).get(project))


def write_token(project, registry=None):
    """The stored write token for a project (or None). Internal — used by the webui
    upload proxy to inject the Bearer header so the browser never holds the token.
    Never exposed by a read API."""
    registry = registry if registry is not None else load_registry()
    return registry.get("tokens", {}).get(project)


def rotate_write_token(project):
    """Generate (or replace) the project's files write token; return it ONCE.
    The value is stored but never handed back by a read API — copy it now."""
    project = _validate_project(project)
    token = secrets.token_urlsafe(32)
    with _registry.LOCK:
        registry = load_registry()
        if project != GLOBAL and project not in registry["projects"]:
            raise ProjectError(f"no such project: {project}")
        registry.setdefault("tokens", {})[project] = token
        save_registry(registry)
    return token
