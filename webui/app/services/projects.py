"""The project registry: the single source of truth for what projects exist and
where each one's cache + version-control repo sits.

One central pkgcache process serves every project. Projects are NO LONGER separate
ports: all six roles listen on their default ports, and a project is reached on
those same ports via a path/name/proxy-user prefix (see pkgcache/router.py) —
`/<project>/<role>/…` for npm/pypi/git/files, `/v2/<project>/…` in an OCI image
name, and the proxy username for apt. A "project" is thus just a name + a cache dir
+ its own git+DVC repo. The GLOBAL project is implicit (no prefix) and never stored.

Stored as JSON (not YAML) because the control UI is deliberately stdlib-only and
the pkgcache side already has `json`; both processes read the SAME file:

    {
      "projects": {"projA": {}, "projB": {}},
      "tokens":   {"projA": "<files write token>", "global": "…"}
    }

Project entries are now empty objects (no ports to allocate); the name is all the
routing needs. Older registries that still carry per-project port maps and a "pool"
block are read fine — the ports are simply ignored.

This module is shared by the operations service (per-project VC + shuttle) and the
HTTP layer (project CRUD + scoped reads); pkgcache reads the same file independently
in pkgcache/core/config.py."""
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
# Every role listens on ONE fixed port shared by all projects (the project is in
# the request path/name/proxy-user, not the port), so these ports are constants now.
ROLES = ("oci", "npm", "pypi", "apt", "git", "files")
ROLE_PORT = {"oci": 5000, "npm": 4873, "pypi": 3141, "apt": 3142, "git": 3143, "files": 3144}
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
    """The path prefix a project's role is reached under, relative to the role's
    port. Global → "" (unchanged root URLs). A named project → "/<project>/<role>",
    except OCI, where the project rides the image name under the protocol-fixed /v2
    root ("/v2/<project>"). For apt, real clients pass the project as the proxy
    username; the "/<project>/apt" path form this returns is used only for internal
    progress polling (the router accepts both)."""
    if not project or project == GLOBAL:
        return ""
    if role == "oci":
        return f"/v2/{project}"
    return f"/{project}/{role}"


def exists(project, registry=None):
    if project == GLOBAL:
        return True
    registry = registry if registry is not None else load_registry()
    return project in registry["projects"]


def list_projects():
    """[{name, ports, repo}] for every project including the implicit global one,
    so the UI can render one entry per project (global first). `ports` is the shared
    default-port map (all projects share ports; they differ by URL prefix)."""
    registry = load_registry()
    out = [{
        "name": GLOBAL,
        "ports": dict(ROLE_PORT),
        "repo": str(CACHE_REPO),
        "default": True,
    }]
    for name in sorted(registry["projects"]):
        out.append({
            "name": name,
            "ports": dict(ROLE_PORT),
            "repo": str(repo_dir(name)),
            "default": False,
        })
    return out


# ---- mutations -----------------------------------------------------------
# Each mutation holds _registry.LOCK across the whole load→mutate→save so two
# concurrent creates/deletes can't race on the read-modify-write (the webui is the
# only writer; pkgcache only reads, over the gateway's atomic temp→rename).


def create(name, *, probe=True):
    """Register a new project and create its cache subdirs so a checkpoint (and the
    live cache reads) work immediately. Returns the new project's record. There are
    no ports to allocate — the pkgcache process notices the new name on its next
    registry poll and starts routing `/<name>/<role>/…` to it, no restart or rebind.
    `probe` is accepted for call-site compatibility and ignored."""
    name = validate_name(name)
    with _registry.LOCK:
        registry = load_registry()
        if name in registry["projects"]:
            raise ProjectError(f"project already exists: {name}")
        registry["projects"][name] = {}
        save_registry(registry)
    # Pre-create the per-role cache subdirs so the first checkpoint has something to
    # `dvc add` even before any artifact is cached, and the live reads don't 404.
    base = repo_dir(name)
    for subdir in ROLE_SUBDIR.values():
        (base / subdir).mkdir(parents=True, exist_ok=True)
    return {"name": name, "ports": dict(ROLE_PORT), "repo": str(base)}


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
        registry.get("tokens", {}).pop(name, None)  # drop its files write token too
        save_registry(registry)
    return {"name": name, "repo": str(repo_dir(name))}


# ---- files write tokens --------------------------------------------------
# The files role is the only write path. GET/HEAD stay anonymous; PUT/DELETE
# require a per-project token generated here and stored in the SAME registry file
# pkgcache reads (it verifies against it, mtime-cached — see core/config.files_token).

def _valid_token_project(project):
    """A project a token may belong to: the implicit global, or a registered name."""
    if project == GLOBAL:
        return GLOBAL
    return validate_name(project)


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
    project = _valid_token_project(project)
    token = secrets.token_urlsafe(32)
    with _registry.LOCK:
        registry = load_registry()
        if project != GLOBAL and project not in registry["projects"]:
            raise ProjectError(f"no such project: {project}")
        registry.setdefault("tokens", {})[project] = token
        save_registry(registry)
    return token
