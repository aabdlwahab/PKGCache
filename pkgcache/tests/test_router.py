"""Tests for the per-role project router (pkgcache/router.py).

Two layers:
  * pure selection — the strategy that maps a request scope to (project, new_scope),
    with no cores built (fakes stand in for the mounted sub-apps); and
  * end-to-end ASGI — a real RoleServer with real global + project sub-apps, driven
    over httpx's ASGITransport, to prove the prefix is stripped, root_path is set,
    and each request reaches the right project's core.

    cd pkgcache && .venv-test/bin/python -m pytest tests/test_router.py -q
"""
from __future__ import annotations

import base64
import os
import tempfile

import httpx
import pytest

from pkgcache.handlers.common import external_base
from pkgcache.router import RoleServer, _basic_user


def _scope(path="/", headers=None, raw_path=None):
    return {
        "type": "http",
        "method": "GET",
        "path": path,
        "raw_path": (raw_path if raw_path is not None else path).encode(),
        "headers": headers or [],
        "root_path": "",
    }


def _server(role, *projects):
    """A RoleServer with the given project names present as opaque sub-apps (enough
    for the membership checks the selection strategy makes)."""
    rs = RoleServer(role)
    rs._apps = {"global": object(), **{p: object() for p in projects}}
    return rs


# ---- path roles (npm/pypi/git/files) -------------------------------------

def test_path_role_strips_project_prefix_and_sets_root_path():
    rs = _server("pypi", "gamma")
    proj, scope = rs._select(_scope("/gamma/pypi/root/pypi/+simple/idna/"))
    assert proj == "gamma"
    assert scope["path"] == "/root/pypi/+simple/idna/"
    assert scope["root_path"] == "/gamma/pypi"
    assert scope["raw_path"] == b"/root/pypi/+simple/idna/"


def test_path_role_bare_project_root_becomes_slash():
    rs = _server("npm", "gamma")
    proj, scope = rs._select(_scope("/gamma/npm"))
    assert proj == "gamma"
    assert scope["path"] == "/"
    assert scope["root_path"] == "/gamma/npm"


def test_path_role_global_passthrough_unchanged():
    rs = _server("npm", "gamma")
    proj, scope = rs._select(_scope("/left-pad"))
    assert proj == "global"
    assert scope["path"] == "/left-pad"
    assert scope.get("root_path", "") == ""


def test_path_role_unknown_project_is_global():
    rs = _server("npm", "gamma")
    # 'ghost' is not registered → treated as a package name on the global app.
    proj, scope = rs._select(_scope("/ghost/npm"))
    assert proj == "global"
    assert scope["path"] == "/ghost/npm"


def test_path_role_wrong_second_segment_is_global():
    # A real npm package request that merely shares a project's first segment must
    # NOT be captured: the second segment has to equal the role name.
    rs = _server("npm", "gamma")
    proj, _ = rs._select(_scope("/gamma"))          # unscoped package "gamma"
    assert proj == "global"
    proj, _ = rs._select(_scope("/gamma/-/progress"))  # not ".../npm/..."
    assert proj == "global"


def test_explicit_global_prefix_alias():
    rs = _server("pypi", "gamma")
    proj, scope = rs._select(_scope("/global/pypi/root/pypi/+simple/idna/"))
    assert proj == "global"
    assert scope["path"] == "/root/pypi/+simple/idna/"
    assert scope["root_path"] == "/global/pypi"


# ---- oci ------------------------------------------------------------------

def test_oci_strips_project_from_image_name():
    rs = _server("oci", "gamma")
    proj, scope = rs._select(_scope("/v2/gamma/dockerhub/library/alpine/manifests/latest"))
    assert proj == "gamma"
    assert scope["path"] == "/v2/dockerhub/library/alpine/manifests/latest"
    assert scope["raw_path"] == b"/v2/dockerhub/library/alpine/manifests/latest"
    assert scope["pkgcache_oci_project"] == "gamma"
    assert scope.get("root_path", "") == ""  # OCI does not rewrite response URLs


def test_oci_version_ping_and_progress_stay_global():
    rs = _server("oci", "gamma")
    assert rs._select(_scope("/v2/"))[0] == "global"
    assert rs._select(_scope("/v2/_progress"))[0] == "global"


def test_oci_global_upstream_alias_not_a_project():
    rs = _server("oci", "gamma")
    proj, scope = rs._select(_scope("/v2/dockerhub/library/alpine/manifests/latest"))
    assert proj == "global"
    assert scope["path"] == "/v2/dockerhub/library/alpine/manifests/latest"


def test_oci_project_progress_path():
    rs = _server("oci", "gamma")
    proj, scope = rs._select(_scope("/v2/gamma/_progress"))
    assert proj == "gamma"
    assert scope["path"] == "/v2/_progress"


# ---- apt ------------------------------------------------------------------

def _basic(user, pw=""):
    return b"Basic " + base64.b64encode(f"{user}:{pw}".encode())


def test_apt_project_from_proxy_username():
    rs = _server("apt", "gamma")
    scope = _scope("http://deb.debian.org/debian/pool/main/x.deb",
                   headers=[(b"proxy-authorization", _basic("gamma"))],
                   raw_path="http://deb.debian.org/debian/pool/main/x.deb")
    proj, out = rs._select(scope)
    assert proj == "gamma"
    assert out is scope  # forward-proxy scope untouched


def test_apt_unknown_user_is_global():
    rs = _server("apt", "gamma")
    scope = _scope("http://x/y", headers=[(b"proxy-authorization", _basic("ghost"))],
                   raw_path="http://x/y")
    assert rs._select(scope)[0] == "global"


def test_apt_no_auth_is_global():
    rs = _server("apt", "gamma")
    scope = _scope("http://x/y", raw_path="http://x/y")
    assert rs._select(scope)[0] == "global"


def test_apt_internal_path_form_for_polling():
    rs = _server("apt", "gamma")
    proj, scope = rs._select(_scope("/gamma/apt/acng-progress"))
    assert proj == "gamma"
    assert scope["path"] == "/acng-progress"
    assert scope["root_path"] == "/gamma/apt"


def test_basic_user_decoding():
    assert _basic_user(_basic("gamma", "ignored")) == "gamma"
    assert _basic_user(b"Bearer abc") is None
    assert _basic_user(b"garbage") is None


# ---- external_base honors the router's root_path -------------------------

def test_external_base_appends_root_path():
    req = httpx.Request("GET", "https://cache:4873/gamma/npm/left-pad")
    scope = {"type": "http", "headers": [(b"host", b"cache:4873")],
             "root_path": "/gamma/npm", "scheme": "https", "path": "/left-pad",
             "query_string": b"", "server": ("cache", 4873)}
    from starlette.requests import Request
    assert external_base(Request(scope)) == "https://cache:4873/gamma/npm"


def test_external_base_global_has_no_prefix():
    from starlette.requests import Request
    scope = {"type": "http", "headers": [(b"host", b"cache:4873")],
             "root_path": "", "scheme": "https", "path": "/left-pad",
             "query_string": b"", "server": ("cache", 4873)}
    assert external_base(Request(scope)) == "https://cache:4873"


# ---- end-to-end ASGI dispatch --------------------------------------------

@pytest.mark.asyncio
async def test_healthz_routes_to_the_right_project(tmp_path, monkeypatch):
    from pkgcache.core.config import load_roles

    reg = tmp_path / "projects.json"
    reg.write_text('{"projects": {"gamma": {}}, "tokens": {}}')
    monkeypatch.setenv("PKGCACHE_PROJECTS", str(reg))
    monkeypatch.setenv("PKGCACHE_CACHE_ROOT", str(tmp_path / "caches"))
    monkeypatch.setenv("OFFLINE", "1")  # no upstream / speedtest during the test

    role_cfgs = load_roles()
    rs = RoleServer("pypi")
    await rs.reconcile(role_cfgs["pypi"])
    try:
        transport = httpx.ASGITransport(app=rs)
        async with httpx.AsyncClient(transport=transport, base_url="https://pkgcache:3141") as c:
            g = await c.get("/healthz")
            assert g.status_code == 200 and g.json()["project"] == "global"
            p = await c.get("/gamma/pypi/healthz")
            assert p.status_code == 200 and p.json()["project"] == "gamma"
            # An unknown project falls through to global (no such route there → 404).
            u = await c.get("/ghost/pypi/healthz")
            assert u.status_code == 404
    finally:
        await rs.aclose_all()


@pytest.mark.asyncio
async def test_project_removed_on_reconcile(tmp_path, monkeypatch):
    from pkgcache.core.config import load_roles

    reg = tmp_path / "projects.json"
    reg.write_text('{"projects": {"gamma": {}}, "tokens": {}}')
    monkeypatch.setenv("PKGCACHE_PROJECTS", str(reg))
    monkeypatch.setenv("PKGCACHE_CACHE_ROOT", str(tmp_path / "caches"))
    monkeypatch.setenv("OFFLINE", "1")

    rs = RoleServer("pypi")
    await rs.reconcile(load_roles()["pypi"])
    assert "gamma" in rs._apps
    # Drop the project from the registry and reconcile again → its sub-app is gone.
    reg.write_text('{"projects": {}, "tokens": {}}')
    await rs.reconcile(load_roles()["pypi"])
    try:
        assert "gamma" not in rs._apps
        assert "global" in rs._apps
    finally:
        await rs.aclose_all()
