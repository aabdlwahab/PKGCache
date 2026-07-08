"""Tests for the unified listener (pkgcache/unified.py).

Selection layer with fake RoleServers, then end-to-end ASGI over httpx's
ASGITransport with real cores, mirroring tests/test_router.py.

    cd pkgcache && .venv-test/bin/python -m pytest tests/test_unified.py -q
"""
from __future__ import annotations

import json

import httpx
import pytest

from pkgcache.router import RoleServer
from pkgcache.unified import UnifiedServer


class _FakeRole:
    """Stands in for a RoleServer: records the scopes it was called with."""

    def __init__(self, projects=("global",)) -> None:
        self._projects = set(projects)
        self.calls: list[dict] = []

    def serves(self, project: str) -> bool:
        return project in self._projects

    async def __call__(self, scope, receive, send) -> None:
        self.calls.append(scope)
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"text/plain")]})
        await send({"type": "http.response.body", "body": b"ok"})


def _servers(projects=("global", "gamma")):
    return {role: _FakeRole(projects) for role in ("oci", "npm", "pypi", "git", "files", "apt")}


async def _request(app, path, raw_path=None):
    sent = []

    async def receive():
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message):
        sent.append(message)

    scope = {"type": "http", "method": "GET", "path": path,
             "raw_path": (raw_path or path).encode(), "headers": [], "root_path": ""}
    await app(scope, receive, send)
    status = next(m["status"] for m in sent if m["type"] == "http.response.start")
    body = b"".join(m.get("body", b"") for m in sent if m["type"] == "http.response.body")
    return status, body


# ---- dispatch rules --------------------------------------------------------

async def test_v2_routes_to_oci():
    servers = _servers()
    u = UnifiedServer(servers)
    status, _ = await _request(u, "/v2/gamma/dockerhub/library/alpine/manifests/latest")
    assert status == 200
    assert len(servers["oci"].calls) == 1
    # The dispatcher passes the scope through untouched — the oci RoleServer does
    # its own image-name project peel.
    assert servers["oci"].calls[0]["path"] == "/v2/gamma/dockerhub/library/alpine/manifests/latest"


async def test_v2_ping_routes_to_oci():
    servers = _servers()
    u = UnifiedServer(servers)
    status, _ = await _request(u, "/v2/")
    assert status == 200 and len(servers["oci"].calls) == 1


async def test_path_roles_route_by_second_segment():
    servers = _servers()
    u = UnifiedServer(servers)
    for role, path in [("npm", "/gamma/npm/left-pad"),
                       ("pypi", "/global/pypi/root/pypi/+simple/idna/"),
                       ("git", "/gamma/git/github.com/o/r.git/info/refs"),
                       ("files", "/global/files/builds/app.tar.gz"),
                       ("apt", "/gamma/apt/acng-progress")]:
        status, _ = await _request(u, path)
        assert status == 200, path
        assert len(servers[role].calls) == 1, path


async def test_unknown_project_is_a_clear_404():
    u = UnifiedServer(_servers(projects=("global",)))
    status, body = await _request(u, "/ghost/npm/left-pad")
    assert status == 404
    assert b"unknown project 'ghost'" in body


async def test_unqualified_path_is_a_helpful_404():
    servers = _servers()
    u = UnifiedServer(servers)
    status, body = await _request(u, "/left-pad")   # bare npm-style path: not served here
    assert status == 404
    assert b"/<project>/<role>/" in body
    assert not servers["npm"].calls


async def test_absolute_form_proxy_request_points_at_apt_port():
    u = UnifiedServer(_servers())
    status, body = await _request(u, "/debian/pool/x.deb",
                                  raw_path="http://deb.debian.org/debian/pool/x.deb")
    assert status == 400
    assert b"3142" in body


async def test_healthz_and_index():
    u = UnifiedServer(_servers())
    status, body = await _request(u, "/healthz")
    assert status == 200 and json.loads(body)["server"] == "unified"
    status, body = await _request(u, "/")
    assert status == 200 and "npm" in json.loads(body)


# ---- end-to-end with real cores -------------------------------------------

@pytest.fixture
def registry(tmp_path, monkeypatch):
    reg = tmp_path / "projects.json"
    reg.write_text('{"projects": {"gamma": {}}, "tokens": {}}')
    monkeypatch.setenv("PKGCACHE_PROJECTS", str(reg))
    monkeypatch.setenv("PKGCACHE_CACHE_ROOT", str(tmp_path / "caches"))
    monkeypatch.setenv("OFFLINE", "1")
    return reg


async def _real_unified():
    from pkgcache.core.config import load_roles

    role_cfgs = load_roles()
    role_servers = {}
    for role, projects in role_cfgs.items():
        rs = RoleServer(role)
        await rs.reconcile(projects)
        role_servers[role] = rs
    return UnifiedServer(role_servers), role_servers


async def test_end_to_end_dispatch(registry):
    unified, role_servers = await _real_unified()
    try:
        transport = httpx.ASGITransport(app=unified)
        async with httpx.AsyncClient(transport=transport, base_url="https://pkgcache:8443") as c:
            # per-project healthz through the uniform admin form, for every role
            for role in ("oci", "npm", "pypi", "git", "files", "apt"):
                r = await c.get(f"/gamma/{role}/healthz")
                assert r.status_code == 200 and r.json()["project"] == "gamma", role
                r = await c.get(f"/global/{role}/healthz")
                assert r.status_code == 200 and r.json()["project"] == "global", role
            # docker ping + a pypi index list on the same port
            assert (await c.get("/v2/")).status_code == 200
            assert (await c.get("/global/pypi/+indexes")).status_code == 200
            assert (await c.get("/gamma/pypi/+indexes")).status_code == 200
            # ledger admin endpoints answer per project
            r = await c.get("/gamma/oci/+ledger/stats")
            assert r.status_code == 200
    finally:
        for rs in role_servers.values():
            await rs.aclose_all()
