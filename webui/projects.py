"""The project registry: the single source of truth for what projects exist, the
ports each one's URLs live on, and where its cache + version-control repo sit.

One central pkgcache process serves every project; a "project" is just a set of
ports (one per role) + a cache dir + its own git+DVC repo. The GLOBAL project is
implicit and pinned to the default ports — it is never stored here and never
allocated, so the original URLs keep working untouched.

Stored as JSON (not YAML) because the control UI is deliberately stdlib-only and
the pkgcache side already has `json`; both processes read the SAME file:

    {
      "pool": {"start": 20000, "end": 20999},
      "projects": {"projA": {"oci": 20000, "npm": 20001, "pypi": 20002, "apt": 20003}}
    }

Ports are allocated ONCE at create time and persisted, so a project's URLs never
drift across restarts. The reserved default ports are implicit (the global
project), so they are skipped by the allocator.

This module is shared by ops.py (per-project VC + shuttle) and the HTTP layer
(project CRUD + scoped reads); pkgcache reads the same file independently in
pkgcache/core/config.py.
"""
import json
import os
import pathlib
import re
import socket
import threading

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The global project's cache repo (unchanged). Per-project repos live beside it
# under caches/projects/<name>/ — each its own git+DVC repo, so a per-project
# checkpoint / rollback / shuttle only ever touches that project's state.
CACHE_REPO = ROOT / "caches"
PROJECTS_SUBDIR = "projects"

# The registry file. Same env var pkgcache reads (PKGCACHE_PROJECTS), so the two
# processes always agree; defaults to config/projects.json in the repo.
REGISTRY = pathlib.Path(os.environ.get("PKGCACHE_PROJECTS") or (ROOT / "config" / "projects.json"))

GLOBAL = "global"  # the implicit default project (default ports, caches/ repo)

# pkgcache role names (what the registry + pkgcache config key on) and the four
# cache subdirs / UI ecosystem labels they map to. apt's subdir also holds apk.
ROLES = ("oci", "npm", "pypi", "apt")
ROLE_PORT = {"oci": 5000, "npm": 4873, "pypi": 3141, "apt": 3142}  # global / reserved
ROLE_SUBDIR = {"oci": "docker", "npm": "npm", "pypi": "pip", "apt": "apt"}

# Pool of host ports the allocator hands out (4 per project → ~25 projects by
# default). compose publishes this exact range up front, so creating a project
# needs no container recreate. Widen both together (registry "pool" + compose
# `ports:`) for more projects.
POOL_DEFAULT = {"start": 20000, "end": 20099}

# Project names ride in URLs and on the filesystem, so keep them DNS/path-safe:
# lowercase alnum + dashes, 1–40 chars, no leading/trailing dash.
_NAME_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$")


class ProjectError(ValueError):
    """A bad project request (invalid name, duplicate, pool exhausted). The HTTP
    layer turns this into a 400."""


def validate_name(name):
    name = (name or "").strip()
    if not _NAME_RE.fullmatch(name):
        raise ProjectError(
            "project name must be 1–40 chars, lowercase letters/digits/dashes, "
            "no leading or trailing dash"
        )
    if name == GLOBAL:
        raise ProjectError(f"'{GLOBAL}' is the reserved default project")
    return name


# ---- registry I/O --------------------------------------------------------

def load_registry():
    """The registry dict, with defaults filled in. A MISSING file is a legitimate
    first run → empty registry. A file that EXISTS but is unreadable/corrupt is NOT
    treated as empty: doing so would make the allocator believe no projects exist,
    hand out ports already in use, and overwrite the real entries on the next save
    (this is exactly how two projects ended up on identical ports). Fail loudly so a
    mutation aborts instead of clobbering."""
    data = {}
    if REGISTRY.is_file():
        try:
            data = json.loads(REGISTRY.read_text()) or {}
        except (OSError, ValueError) as exc:
            raise ProjectError(
                f"project registry {REGISTRY} is unreadable/corrupt ({exc}); refusing "
                f"to proceed — fix or remove the file"
            ) from exc
    data.setdefault("pool", dict(POOL_DEFAULT))
    data.setdefault("projects", {})
    return data


def save_registry(data):
    """Persist the registry atomically (temp→rename) so a crash never leaves a
    half-written file the other process would fail to parse. The temp name is unique
    per writer so two concurrent saves can't corrupt a shared .new file (the rename
    is atomic; the write to the temp is not)."""
    REGISTRY.parent.mkdir(parents=True, exist_ok=True)
    tmp = REGISTRY.with_name(f"{REGISTRY.name}.{os.getpid()}.{threading.get_ident()}.new")
    tmp.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    os.replace(tmp, REGISTRY)


# ---- queries -------------------------------------------------------------

def repo_dir(project):
    """The git+DVC repo dir for a project. Global → caches/ (unchanged)."""
    if not project or project == GLOBAL:
        return CACHE_REPO
    return CACHE_REPO / PROJECTS_SUBDIR / project


def ports(project, registry=None):
    """{role: port} for a project. Global → the reserved default ports."""
    if not project or project == GLOBAL:
        return dict(ROLE_PORT)
    registry = registry if registry is not None else load_registry()
    p = registry["projects"].get(project)
    if p is None:
        raise ProjectError(f"no such project: {project}")
    return {role: p[role] for role in ROLES}


def exists(project, registry=None):
    if project == GLOBAL:
        return True
    registry = registry if registry is not None else load_registry()
    return project in registry["projects"]


def list_projects():
    """[{name, ports, repo}] for every project including the implicit global one,
    so the UI can render one entry per project (global first)."""
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
            "ports": ports(name, registry),
            "repo": str(repo_dir(name)),
            "default": False,
        })
    return out


# ---- allocation ----------------------------------------------------------

def _port_free(port):
    """True if nothing on the host is currently bound to this TCP port. Best-effort
    belt-and-braces on top of the registry's own bookkeeping, so we never hand out
    a port some unrelated service grabbed."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("0.0.0.0", port))
            return True
        except OSError:
            return False


def _allocate(registry, *, probe=True):
    """Pick the four lowest free ports in the pool (one per role) for a new project.

    In use = the reserved default ports + every port already assigned to a project;
    optionally also skip anything currently bound on the host. Raises if the pool
    can't supply four. Returns {role: port}."""
    pool = registry["pool"]
    start, end = int(pool["start"]), int(pool["end"])
    in_use = set(ROLE_PORT.values())
    for p in registry["projects"].values():
        in_use.update(p[role] for role in ROLES if role in p)

    chosen = {}
    role_iter = iter(ROLES)
    role = next(role_iter, None)
    for port in range(start, end + 1):
        if role is None:
            break
        if port in in_use:
            continue
        if probe and not _port_free(port):
            in_use.add(port)
            continue
        chosen[role] = port
        in_use.add(port)
        role = next(role_iter, None)
    if role is not None:
        raise ProjectError(
            f"port pool {start}-{end} is exhausted — free a project or widen the pool"
        )
    return chosen


# ---- mutations -----------------------------------------------------------

# The API server is multi-threaded (ThreadingHTTPServer), so two concurrent
# creates/deletes would otherwise race: each reads the same pre-write snapshot,
# allocates the SAME "lowest free" ports, and saves — leaving two projects on
# identical ports. Serialize the whole load→allocate→save read-modify-write so
# allocation always sees every already-committed project. The webui is the only
# writer process (pkgcache only reads, over save_registry's atomic temp→rename),
# so an in-process lock is sufficient.
_LOCK = threading.Lock()


def create(name, *, probe=True):
    """Allocate ports for a new project, persist them, and create its cache subdirs
    so a checkpoint (and the live cache reads) work immediately. Returns the new
    project's record. The pkgcache process notices the new ports on its next poll
    and starts serving them; no container recreate is needed (the pool range is
    published up front)."""
    name = validate_name(name)
    with _LOCK:
        registry = load_registry()
        if name in registry["projects"]:
            raise ProjectError(f"project already exists: {name}")
        assigned = _allocate(registry, probe=probe)
        registry["projects"][name] = assigned
        save_registry(registry)
    # Pre-create the per-role cache subdirs so the first checkpoint has something to
    # `dvc add` even before any artifact is cached, and the live reads don't 404.
    base = repo_dir(name)
    for subdir in ROLE_SUBDIR.values():
        (base / subdir).mkdir(parents=True, exist_ok=True)
    return {"name": name, "ports": assigned, "repo": str(base)}


def delete(name):
    """Remove a project from the registry (freeing its ports back to the pool). The
    on-disk cache tree is left in place — deleting cached bytes is a separate,
    explicit step the operator takes, never a side effect of dropping the entry."""
    name = validate_name(name)
    with _LOCK:
        registry = load_registry()
        if name not in registry["projects"]:
            raise ProjectError(f"no such project: {name}")
        del registry["projects"][name]
        save_registry(registry)
    return {"name": name, "repo": str(repo_dir(name))}
