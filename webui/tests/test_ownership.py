"""Tests for project ownership + route authorization (Phase 3): with auth ENABLED,
each cache/ops route enforces the owner/report/superuser matrix through dispatch.

    cd webui && python3 -m unittest tests.test_ownership -v

Sessions are minted directly (login itself is covered in test_auth); the focus here
is the authorization decision. Only routes whose controllers do no network work after
the guard are driven (endpoints/token/mode/create/list/jobs), so the assertions are
about the guard, not dvc/docker."""
import importlib
import io
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable

from app import settings  # noqa: E402
from app.gateways import users as users_store  # noqa: E402
from app.services.accounts import Account, Accounts  # noqa: E402
from app.services.passwords import PasswordHasher  # noqa: E402
from app.services.sessions import Sessions  # noqa: E402

_ROOT = Account("root", "superuser", None, builtin=True)


class _FakeJobs:
    def __init__(self):
        self.started = []

    def start(self, action, params):
        self.started.append((action, params))
        return 1

    def snapshot(self):
        return {"jobs": []}

    def get(self, jid, offset=0):
        return {"id": jid}


class _Handler:
    """Drives dispatch without a socket, carrying the auth services + a cookie."""

    def __init__(self, accounts, sessions, jobs, body=b"", cookie=""):
        self.accounts, self.sessions, self.jobs = accounts, sessions, jobs
        self.live = self.reads = None
        self.client_address = ("127.0.0.1", 1)
        self.headers = {"Content-Length": str(len(body))}
        if cookie:
            self.headers["Cookie"] = cookie
        self.rfile = io.BytesIO(body)
        self.sent = None

    def send_json(self, obj, code=200, headers=None):
        self.sent = (code, obj)

    def send_download(self, path, filename):
        self.sent = (200, {"download": str(path)})


class OwnershipTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        base = Path(self.tmp.name)
        os.environ["PKGCACHE_PROJECTS"] = str(base / "projects.json")
        os.environ["UI_USERS"] = str(base / "users.json")
        from app.services import projects
        importlib.reload(projects)
        projects.CACHE_REPO = base / "caches"
        self.projects = projects
        from app.api import routes
        self.routes = routes

        self.accounts = Accounts(users_store, PasswordHasher(), "root", "rootpass12")
        self.sessions = Sessions(ttl=100)
        self.jobs = _FakeJobs()

        # alice + amy are admins; bob reports to alice. proja is alice's, projb amy's.
        self.accounts.create(_ROOT, "alice", "adminpass1", "admin")
        self.accounts.create(_ROOT, "amy", "adminpass2", "admin")
        self.accounts.create(Account("alice", "admin", None), "bob", "bobsecret1", "user")
        projects.create("proja", owner="alice")
        projects.create("projb", owner="amy")

    def tearDown(self):
        os.environ.pop("PKGCACHE_PROJECTS", None)
        os.environ.pop("UI_USERS", None)
        self.tmp.cleanup()

    def _do(self, method, path, actor=None, body=None):
        raw = json.dumps(body).encode() if body is not None else b""
        cookie = f"{settings.SESSION_COOKIE}={self.sessions.create(actor)}" if actor else ""
        h = _Handler(self.accounts, self.sessions, self.jobs, body=raw, cookie=cookie)
        self.routes.dispatch(h, method, path)
        return h.sent

    def _code(self, *args, **kw):
        return self._do(*args, **kw)[0]

    # ---- view ----------------------------------------------------------------
    def test_view_allowed_for_owner_report_and_superuser(self):
        for actor in ("alice", "bob", "root"):
            self.assertEqual(self._code("GET", "/api/endpoints?project=proja", actor), 200, actor)

    def test_view_denied_for_other_admin_and_foreign_report(self):
        self.assertEqual(self._code("GET", "/api/endpoints?project=proja", "amy"), 403)
        self.assertEqual(self._code("GET", "/api/endpoints?project=projb", "bob"), 403)

    def test_global_is_superuser_only(self):
        self.assertEqual(self._code("GET", "/api/endpoints", "root"), 200)      # global
        self.assertEqual(self._code("GET", "/api/endpoints", "alice"), 403)

    def test_view_requires_a_session(self):
        self.assertEqual(self._code("GET", "/api/endpoints?project=proja"), 401)

    # ---- operate -------------------------------------------------------------
    def test_operate_allowed_for_owner_and_superuser(self):
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "alice",
                                    {"target": "offline"}), 200)
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "root",
                                    {"target": "online"}), 200)

    def test_operate_denied_for_report_and_other_admin(self):
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "bob",
                                    {"target": "offline"}), 403)   # report = view only
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "amy",
                                    {"target": "offline"}), 403)

    # ---- create --------------------------------------------------------------
    def test_create_denied_for_user_allowed_for_admin(self):
        self.assertEqual(self._code("POST", "/api/projects", "bob", {"name": "x"}), 403)
        self.assertEqual(self._code("POST", "/api/projects", "alice", {"name": "newproj"}), 201)
        self.assertEqual(self.projects.owner("newproj"), "alice")

    # ---- list filtering ------------------------------------------------------
    def _names(self, actor):
        code, obj = self._do("GET", "/api/projects", actor)
        self.assertEqual(code, 200)
        return {p["name"] for p in obj["projects"]}

    def test_list_is_filtered_by_visibility(self):
        self.assertEqual(self._names("alice"), {"proja"})
        self.assertEqual(self._names("bob"), {"proja"})
        self.assertEqual(self._names("amy"), {"projb"})
        self.assertEqual(self._names("root"), {"global", "proja", "projb"})

    # ---- reassign ownership --------------------------------------------------
    def test_superuser_reassigns_owner(self):
        self.assertEqual(self._code("POST", "/api/projects/proja/owner", "alice",
                                    {"owner": "amy"}), 403)   # not a superuser
        self.assertEqual(self._code("POST", "/api/projects/proja/owner", "root",
                                    {"owner": "amy"}), 200)
        # amy now operates proja; alice no longer can.
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "amy",
                                    {"target": "offline"}), 200)
        self.assertEqual(self._code("POST", "/api/projects/proja/mode", "alice",
                                    {"target": "online"}), 403)

    def test_reassign_rejects_non_manager_owner(self):
        self.assertEqual(self._code("POST", "/api/projects/proja/owner", "root",
                                    {"owner": "bob"}), 400)     # bob is a user
        self.assertEqual(self._code("POST", "/api/projects/proja/owner", "root",
                                    {"owner": "ghost"}), 400)

    # ---- jobs (action-scoped) ------------------------------------------------
    def test_job_authorization_by_action(self):
        # checkpoint = owner-level
        self.assertEqual(self._code("POST", "/api/jobs", "alice",
                                    {"action": "checkpoint", "project": "proja"}), 200)
        self.assertEqual(self._code("POST", "/api/jobs", "amy",
                                    {"action": "checkpoint", "project": "proja"}), 403)
        # lockwarm = view-level (a report may warm)
        self.assertEqual(self._code("POST", "/api/jobs", "bob",
                                    {"action": "lockwarm", "project": "proja"}), 200)
        # instance-wide mode = superuser only
        self.assertEqual(self._code("POST", "/api/jobs", "alice", {"action": "mode"}), 403)
        self.assertEqual(self._code("POST", "/api/jobs", "root", {"action": "mode"}), 200)

    # ---- token ---------------------------------------------------------------
    def test_token_rotate_is_owner_status_is_view(self):
        self.assertEqual(self._code("POST", "/api/token", "alice", {"project": "proja"}), 200)
        self.assertEqual(self._code("POST", "/api/token", "bob", {"project": "proja"}), 403)
        self.assertEqual(self._code("GET", "/api/token?project=proja", "bob"), 200)


class EnforcementDisabledTests(unittest.TestCase):
    """With no root and an empty store, auth is not configured → the guards are no-ops
    and the routes stay open, exactly as before the feature (the migration property)."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        base = Path(self.tmp.name)
        os.environ["PKGCACHE_PROJECTS"] = str(base / "projects.json")
        os.environ["UI_USERS"] = str(base / "users.json")
        from app.services import projects
        importlib.reload(projects)
        projects.CACHE_REPO = base / "caches"
        self.projects = projects
        from app.api import routes
        self.routes = routes
        self.accounts = Accounts(users_store, PasswordHasher(), None, None)  # disabled
        self.sessions = Sessions(ttl=100)

    def tearDown(self):
        os.environ.pop("PKGCACHE_PROJECTS", None)
        os.environ.pop("UI_USERS", None)
        self.tmp.cleanup()

    def test_routes_open_without_a_session(self):
        h = _Handler(self.accounts, self.sessions, _FakeJobs())
        self.routes.dispatch(h, "GET", "/api/endpoints")
        self.assertEqual(h.sent[0], 200)
        # me reports that auth is off so the console can skip login.
        h2 = _Handler(self.accounts, self.sessions, _FakeJobs())
        self.routes.dispatch(h2, "GET", "/api/me")
        self.assertEqual(h2.sent, (200, {"auth_enabled": False, "authenticated": False}))


if __name__ == "__main__":
    unittest.main()
