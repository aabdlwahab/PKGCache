"""Role + upstream configuration, loaded from one YAML file with env overrides.

Two run modes share this:
  * single role  — PKGCACHE_ROLE=<role>      → load()      (dev / one role per proc)
  * all roles    — PKGCACHE_ROLE unset       → load_all()  (one container, 4 ports)

In all-roles mode each role caches under PKGCACHE_CACHE_ROOT/<subdir> (compose
mounts ./caches there), and the HTTPS roles terminate TLS in-process using the
PKGCACHE_TLS_CERT/KEY cert (no separate proxy needed).
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path

import yaml

# Default ports per role (overridable via the YAML `listen_port`).
_DEFAULT_PORTS = {"oci": 5000, "npm": 4873, "pypi": 3141, "apt": 3142}
# caches/<subdir> each role owns. apt holds both apt + apk (ecosystem column).
_ROLE_SUBDIR = {"oci": "docker", "npm": "npm", "pypi": "pip", "apt": "apt"}
# Roles fronted by HTTPS; apt is a plain-HTTP forward proxy and is never TLS.
_HTTPS_ROLES = {"oci", "npm", "pypi"}


@dataclass(frozen=True)
class Config:
    role: str                      # oci | npm | pypi | apt
    offline: bool                  # serve from cache only; never reach upstream
    cache_root: Path               # this role's cache tree (…/<subdir>)
    host: str
    port: int
    request_timeout: float         # generous so multi-GB wheels finish
    upstreams: dict[str, str] = field(default_factory=dict)   # oci: dest -> registry base
    indexes: dict[str, str] = field(default_factory=dict)     # pypi: index -> base url
    upstream: str | None = None                               # npm: single registry base
    tls_cert: str | None = None    # in-process TLS (HTTPS roles); None = plain HTTP
    tls_key: str | None = None


def _as_bool(v: str | None) -> bool:
    return str(v).strip().lower() in {"1", "true", "yes", "on"}


def _read() -> dict:
    cfg_path = os.environ.get("PKGCACHE_CONFIG")
    if cfg_path and Path(cfg_path).is_file():
        return yaml.safe_load(Path(cfg_path).read_text()) or {}
    return {}


def _build(role: str, data: dict, *, cache_root: Path, offline: bool,
           cert: str | None, key: str | None) -> Config:
    defaults = data.get("defaults", {}) or {}
    role_cfg = (data.get("roles", {}) or {}).get(role, {}) or {}
    port = int(os.environ.get("PKGCACHE_PORT") or role_cfg.get("listen_port") or _DEFAULT_PORTS[role])
    timeout = float(
        os.environ.get("PKGCACHE_REQUEST_TIMEOUT")
        or role_cfg.get("request_timeout") or defaults.get("request_timeout") or 1200
    )
    use_tls = role in _HTTPS_ROLES and cert and key
    return Config(
        role=role,
        offline=offline,
        cache_root=cache_root,
        host=os.environ.get("PKGCACHE_HOST", "0.0.0.0"),
        port=port,
        request_timeout=timeout,
        upstreams=dict(role_cfg.get("upstreams", {}) or {}),
        indexes=dict(role_cfg.get("indexes", {}) or {}),
        upstream=role_cfg.get("upstream"),
        tls_cert=cert if use_tls else None,
        tls_key=key if use_tls else None,
    )


def load() -> Config:
    """Single-role config (PKGCACHE_ROLE required). cache_root is used as-is."""
    role = os.environ.get("PKGCACHE_ROLE", "").strip().lower()
    if role not in _DEFAULT_PORTS:
        raise SystemExit(f"PKGCACHE_ROLE must be one of {sorted(_DEFAULT_PORTS)}; got {role!r}")
    return _build(
        role, _read(),
        cache_root=Path(os.environ.get("PKGCACHE_CACHE_ROOT", "/data")),
        offline=_as_bool(os.environ.get("OFFLINE", "0")),
        cert=os.environ.get("PKGCACHE_TLS_CERT") or None,
        key=os.environ.get("PKGCACHE_TLS_KEY") or None,
    )


def load_all() -> list[Config]:
    """One config per role (all four). cache_root is a BASE; each role gets
    base/<subdir>. Used when PKGCACHE_ROLE is unset → one container, four ports."""
    data = _read()
    base = Path(os.environ.get("PKGCACHE_CACHE_ROOT", "/caches"))
    offline = _as_bool(os.environ.get("OFFLINE", "0"))
    cert = os.environ.get("PKGCACHE_TLS_CERT") or None
    key = os.environ.get("PKGCACHE_TLS_KEY") or None
    return [
        _build(role, data, cache_root=base / _ROLE_SUBDIR[role],
               offline=offline, cert=cert, key=key)
        for role in ("oci", "npm", "pypi", "apt")
    ]
