"""Role + upstream configuration, loaded from one YAML file with env overrides.

Two run modes share this:
  * single role  — PKGCACHE_ROLE=<role>      → load()       (dev / one role per proc)
  * all roles    — PKGCACHE_ROLE unset       → load_roles() (one container, 2 ports:
                   the unified HTTPS port + the apt proxy port — see unified.py)

In all-roles mode each role caches under PKGCACHE_CACHE_ROOT/<subdir> (compose
mounts ./caches there), and the HTTPS roles terminate TLS in-process using the
PKGCACHE_TLS_CERT/KEY cert (no separate proxy needed).

Multi-project: a single process can serve extra *projects* beyond the default
("global") one. Each project in the registry (PKGCACHE_PROJECTS — the same JSON
file the control UI manages) gets its own cache tree at
PKGCACHE_CACHE_ROOT/projects/<name>/<subdir>, so its content and version control
are isolated from global's. Projects are NOT separate ports: load_roles() returns
one Config per (role, project) all sharing the role's default port, and the router
(pkgcache/router.py) dispatches by request path/name/proxy-user. global is unchanged
(root URLs, PKGCACHE_CACHE_ROOT/<subdir>).
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

import yaml

# Default ports per role (overridable via the YAML `listen_port`).
_DEFAULT_PORTS = {"oci": 5000, "npm": 4873, "pypi": 3141, "apt": 3142, "git": 3143, "files": 3144}
# caches/<subdir> each role owns. apt holds both apt + apk (ecosystem column).
_ROLE_SUBDIR = {"oci": "docker", "npm": "npm", "pypi": "pip", "apt": "apt", "git": "git", "files": "files"}
# Roles fronted by HTTPS; apt is a plain-HTTP forward proxy and is never TLS.
_HTTPS_ROLES = {"oci", "npm", "pypi", "git", "files"}
# Every role served in all-roles mode (order = startup order).
_ALL_ROLES = ("oci", "npm", "pypi", "apt", "git", "files")

# The implicit default project: default ports, cache tree directly under the root.
GLOBAL = "global"
# Per-project cache trees live under <root>/projects/<name>/<subdir>.
_PROJECTS_SUBDIR = "projects"
# One sha256 content-addressed store shared by every project+role, a sibling of the
# per-project trees under the cache root, so an artifact one project fetched is not
# re-downloaded or re-stored for the next. Disable with PKGCACHE_CAS=0.
_CAS_SUBDIR = ".cas"


@dataclass(frozen=True)
class Config:
    role: str                      # oci | npm | pypi | apt
    offline: bool                  # serve from cache only; never reach upstream
    project: str                   # "global" or a registry project name
    cache_root: Path               # this role's cache tree (…/<subdir>)
    cas_root: Path | None          # shared sha256 content store (cross-project dedup); None = off
    host: str
    port: int
    request_timeout: float         # generous so multi-GB wheels finish
    upstreams: dict[str, str] = field(default_factory=dict)   # oci: dest -> registry base
    indexes: dict[str, str] = field(default_factory=dict)     # pypi: index -> base url
    upstream: str | None = None                               # npm: single registry base
    tls_cert: str | None = None    # in-process TLS (HTTPS roles); None = plain HTTP
    tls_key: str | None = None
    refs_ttl: float = 60.0         # git: seconds an advertised ref set is trusted before re-fetch
    max_upload_packs: int = 8      # git: concurrent upload-pack (pack computation) cap
    max_upload_mb: int = 0         # files: reject PUTs larger than this (0 = unlimited)


def _as_bool(v: str | None) -> bool:
    return str(v).strip().lower() in {"1", "true", "yes", "on"}


def _read() -> dict:
    cfg_path = os.environ.get("PKGCACHE_CONFIG")
    if cfg_path and Path(cfg_path).is_file():
        return yaml.safe_load(Path(cfg_path).read_text()) or {}
    return {}


def _build(role: str, data: dict, *, cache_root: Path, offline: bool,
           cert: str | None, key: str | None, project: str = GLOBAL,
           port: int | None = None, cas_root: Path | None = None) -> Config:
    defaults = data.get("defaults", {}) or {}
    role_cfg = (data.get("roles", {}) or {}).get(role, {}) or {}
    # A project gets its assigned registry port; global falls back to env / YAML /
    # default. PKGCACHE_PORT only ever overrides the single-role (load()) path.
    if port is None:
        port = int(os.environ.get("PKGCACHE_PORT") or role_cfg.get("listen_port") or _DEFAULT_PORTS[role])
    timeout = float(
        os.environ.get("PKGCACHE_REQUEST_TIMEOUT")
        or role_cfg.get("request_timeout") or defaults.get("request_timeout") or 1200
    )
    use_tls = role in _HTTPS_ROLES and cert and key
    return Config(
        role=role,
        offline=offline,
        project=project,
        cache_root=cache_root,
        cas_root=cas_root,
        host=os.environ.get("PKGCACHE_HOST", "0.0.0.0"),
        port=port,
        request_timeout=timeout,
        upstreams=dict(role_cfg.get("upstreams", {}) or {}),
        indexes=dict(role_cfg.get("indexes", {}) or {}),
        upstream=role_cfg.get("upstream"),
        tls_cert=cert if use_tls else None,
        tls_key=key if use_tls else None,
        refs_ttl=float(role_cfg.get("refs_ttl") or 60),
        max_upload_packs=int(role_cfg.get("max_upload_packs") or 8),
        max_upload_mb=int(role_cfg.get("max_upload_mb") or 0),
    )


def _cas_enabled() -> bool:
    """The CAS is on by default; PKGCACHE_CAS=0/false/no/off turns it off."""
    return os.environ.get("PKGCACHE_CAS", "1").strip().lower() not in {"0", "false", "no", "off"}


def load() -> Config:
    """Single-role config (PKGCACHE_ROLE required). cache_root is used as-is."""
    role = os.environ.get("PKGCACHE_ROLE", "").strip().lower()
    if role not in _DEFAULT_PORTS:
        raise SystemExit(f"PKGCACHE_ROLE must be one of {sorted(_DEFAULT_PORTS)}; got {role!r}")
    cache_root = Path(os.environ.get("PKGCACHE_CACHE_ROOT", "/data"))
    return _build(
        role, _read(),
        cache_root=cache_root,
        cas_root=(cache_root / _CAS_SUBDIR) if _cas_enabled() else None,
        offline=_as_bool(os.environ.get("OFFLINE", "0")),
        cert=os.environ.get("PKGCACHE_TLS_CERT") or None,
        key=os.environ.get("PKGCACHE_TLS_KEY") or None,
    )


def _registry() -> dict:
    """The parsed project registry the control UI manages (PKGCACHE_PROJECTS, JSON):
    the "projects" name map plus the webui-owned side maps we consume ("offline").
    Missing/unreadable → empty (just global, online). Read fresh on each call so the
    supervisor in __main__ picks up changes made at runtime."""
    path = os.environ.get("PKGCACHE_PROJECTS")
    if path and Path(path).is_file():
        try:
            return json.loads(Path(path).read_text()) or {}
        except (OSError, ValueError):
            return {}
    return {}


# The files role verifies write tokens against the SAME registry file the webui
# writes (PKGCACHE_PROJECTS). Re-reading it per PUT would be wasteful, and baking
# the token into the frozen Config would go stale on rotation — so cache the parsed
# tokens map keyed by the file's mtime+size, re-parsing only when it changes. A
# webui rotation is then picked up within one write, with no restart/rebind.
_TOKENS_CACHE: dict = {"key": None, "tokens": {}}


def files_token(project: str) -> str | None:
    """The write token for a project's files role, or None if unset. mtime-cached."""
    path = os.environ.get("PKGCACHE_PROJECTS")
    if not path:
        return None
    p = Path(path)
    try:
        stat = p.stat()
    except OSError:
        return None
    key = (stat.st_mtime_ns, stat.st_size)
    if _TOKENS_CACHE["key"] != key:
        try:
            data = json.loads(p.read_text()) or {}
            _TOKENS_CACHE["tokens"] = data.get("tokens", {}) or {}
        except (OSError, ValueError):
            _TOKENS_CACHE["tokens"] = {}
        _TOKENS_CACHE["key"] = key
    return _TOKENS_CACHE["tokens"].get(project)


def load_roles() -> dict[str, dict[str, Config]]:
    """Per-role config sets to serve in one process, keyed {role: {project: Config}}.

    One server per role listens on that role's default port; every project shares
    that port and is distinguished by request path/name/proxy-user (see router.py),
    NOT by a per-project port. So each project's Config for a role carries the same
    default port (used only for logging) but its own cache tree at
    base/projects/<name>/<subdir>. `global` is always present with the base tree.

    Read fresh each call so the entrypoint's supervisor can diff this against the
    running role servers and add/drop projects without a restart or rebind."""
    data = _read()
    base = Path(os.environ.get("PKGCACHE_CACHE_ROOT", "/caches"))
    cert = os.environ.get("PKGCACHE_TLS_CERT") or None
    key = os.environ.get("PKGCACHE_TLS_KEY") or None
    registry = _registry()
    names = sorted(registry.get("projects", {}) or {})
    # A project is offline when the instance is (the OFFLINE env — the air-gap hard
    # mode), OR the registry's instance-wide "*" soft flag is set (the webui's mode
    # op — overrides every project while set, without touching their own flags), OR
    # its own per-project soft flag is set; the webui flips the flags (global
    # included) and the supervisor applies them on its next poll.
    env_offline = _as_bool(os.environ.get("OFFLINE", "0"))
    soft = registry.get("offline", {}) or {}
    instance_offline = env_offline or bool(soft.get("*"))
    # ONE content store for the whole instance (all projects + roles), so identical
    # bytes are held once regardless of which project or ecosystem fetched them.
    cas_root = (base / _CAS_SUBDIR) if _cas_enabled() else None

    out: dict[str, dict[str, Config]] = {}
    for role in _ALL_ROLES:
        port = _DEFAULT_PORTS[role]
        projects = {
            GLOBAL: _build(role, data, cache_root=base / _ROLE_SUBDIR[role],
                           offline=instance_offline or bool(soft.get(GLOBAL)),
                           cert=cert, key=key, port=port, cas_root=cas_root),
        }
        for name in names:
            projects[name] = _build(
                role, data, cache_root=base / _PROJECTS_SUBDIR / name / _ROLE_SUBDIR[role],
                offline=instance_offline or bool(soft.get(name)),
                cert=cert, key=key, project=name, port=port, cas_root=cas_root,
            )
        out[role] = projects
    return out
