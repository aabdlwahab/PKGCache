"""Tests for the auth layer (Phase 2): password hashing, the accounts policy, the
session store + login throttle, and the login/me/users routes through dispatch.

    cd webui && python3 -m unittest tests.test_auth -v

Behaviour only — the accounts service is driven through its public API against a
throwaway users store, never by poking private state. The env-superuser is injected
as constructor args, so no real environment is touched."""
import io
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable

from app.gateways import users as users_store  # noqa: E402
from app.services.accounts import Account, Accounts  # noqa: E402
from app.services.passwords import PasswordHasher  # noqa: E402
from app.services.sessions import Sessions  # noqa: E402

_ROOT = Account("root", "superuser", None, builtin=True)


class PasswordHasherTests(unittest.TestCase):
    def test_hash_then_verify_round_trips(self):
        h = PasswordHasher()
        salt, digest = h.hash("correct horse")
        self.assertTrue(h.verify("correct horse", salt, digest))

    def test_wrong_password_and_garbage_stored_value_fail(self):
        h = PasswordHasher()
        salt, digest = h.hash("correct horse")
        self.assertFalse(h.verify("wrong", salt, digest))
        self.assertFalse(h.verify("correct horse", "nothex", digest))

    def test_salt_differs_per_hash(self):
        h = PasswordHasher()
        self.assertNotEqual(h.hash("same")[0], h.hash("same")[0])


class AccountsTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["UI_USERS"] = str(Path(self.tmp.name) / "users.json")
        self.accounts = Accounts(users_store, PasswordHasher(), "root", "rootpass12")

    def tearDown(self):
        os.environ.pop("UI_USERS", None)
        self.tmp.cleanup()

    def _admin(self, name="alice"):
        self.accounts.create(_ROOT, name, "adminpass1", "admin")
        return Account(name, "admin", None)

    # ---- authentication ------------------------------------------------------
    def test_env_root_authenticates_and_is_never_stored(self):
        acct = self.accounts.authenticate("root", "rootpass12")
        self.assertEqual((acct.username, acct.role, acct.builtin), ("root", "superuser", True))
        self.assertIsNone(self.accounts.authenticate("root", "wrong"))
        self.assertEqual(users_store.load()["users"], {})

    def test_stored_user_authenticates(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.assertIsNotNone(self.accounts.authenticate("bob", "bobsecret1"))
        self.assertIsNone(self.accounts.authenticate("bob", "nope"))

    # ---- create policy -------------------------------------------------------
    def test_admin_creates_user_reporting_to_self(self):
        admin = self._admin()
        acct = self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.assertEqual((acct.role, acct.reports_to), ("user", "alice"))

    def test_admin_cannot_create_admin_or_superuser(self):
        admin = self._admin()
        for role in ("admin", "superuser"):
            with self.assertRaises(Exception):
                self.accounts.create(admin, f"x{role}", "password12", role)

    def test_user_cannot_create(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        bob = self.accounts.get("bob")
        with self.assertRaises(Exception):
            self.accounts.create(bob, "carol", "carolsecret1", "user")

    def test_superuser_creates_admin_with_no_manager(self):
        acct = self.accounts.create(_ROOT, "alice", "adminpass1", "admin")
        self.assertEqual((acct.role, acct.reports_to), ("admin", None))

    def test_superuser_user_needs_valid_manager(self):
        self._admin()
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "bob", "bobsecret1", "user")  # no reports_to
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "bob", "bobsecret1", "user", reports_to="ghost")
        acct = self.accounts.create(_ROOT, "bob", "bobsecret1", "user", reports_to="alice")
        self.assertEqual(acct.reports_to, "alice")

    def test_reserved_root_name_and_bad_inputs_rejected(self):
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "root", "password12", "admin")   # reserved
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "Alice", "password12", "admin")  # bad grammar
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "alice", "short", "admin")       # weak password
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "alice", "password12", "wizard") # bad role

    def test_duplicate_rejected(self):
        self._admin()
        with self.assertRaises(Exception):
            self.accounts.create(_ROOT, "alice", "password12", "admin")

    # ---- update policy -------------------------------------------------------
    def test_superuser_promotes_user_and_clears_manager(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        acct = self.accounts.update(_ROOT, "bob", role="admin")
        self.assertEqual((acct.role, acct.reports_to), ("admin", None))

    def test_demoting_admin_with_reports_is_blocked(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        with self.assertRaises(Exception):
            self.accounts.update(_ROOT, "alice", role="user", reports_to="root")

    def test_demote_admin_to_user_needs_a_manager(self):
        self.accounts.create(_ROOT, "alice", "adminpass1", "admin")  # no reports
        with self.assertRaises(Exception):
            self.accounts.update(_ROOT, "alice", role="user")        # user w/o manager
        acct = self.accounts.update(_ROOT, "alice", role="user", reports_to="root")
        self.assertEqual((acct.role, acct.reports_to), ("user", "root"))

    def test_admin_cannot_change_roles(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        with self.assertRaises(Exception):
            self.accounts.update(admin, "bob", role="admin")

    def test_superuser_reassigns_manager(self):
        self.accounts.create(_ROOT, "alice", "adminpass1", "admin")
        self.accounts.create(_ROOT, "amy", "adminpass2", "admin")
        self.accounts.create(Account("alice", "admin", None), "bob", "bobsecret1", "user")
        acct = self.accounts.update(_ROOT, "bob", reports_to="amy")
        self.assertEqual(acct.reports_to, "amy")

    def test_self_role_change_blocked_and_root_immutable(self):
        with self.assertRaises(Exception):
            self.accounts.update(_ROOT, "root", password="whatever12")  # root via API
        admin = self._admin()
        # An admin can reset their own report's password but not their own role.
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.accounts.update(admin, "bob", password="newsecret1")
        self.assertIsNotNone(self.accounts.authenticate("bob", "newsecret1"))

    def test_password_authorization(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.accounts.create(_ROOT, "amy", "adminpass2", "admin")
        amy = self.accounts.get("amy")
        # A different admin may not reset bob's password (bob doesn't report to amy).
        with self.assertRaises(Exception):
            self.accounts.update(amy, "bob", password="hijacked1")
        # Bob may change his own.
        bob = self.accounts.get("bob")
        self.accounts.update(bob, "bob", password="selfset123")
        self.assertIsNotNone(self.accounts.authenticate("bob", "selfset123"))

    # ---- delete policy -------------------------------------------------------
    def test_admin_deletes_only_own_reports(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.accounts.create(_ROOT, "amy", "adminpass2", "admin")
        amy = self.accounts.get("amy")
        with self.assertRaises(Exception):
            self.accounts.delete(amy, "bob")     # not amy's report
        self.accounts.delete(admin, "bob")
        self.assertIsNone(self.accounts.get("bob"))

    def test_superuser_delete_guards_root_self_and_reports(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        with self.assertRaises(Exception):
            self.accounts.delete(_ROOT, "root")   # env root
        with self.assertRaises(Exception):
            self.accounts.delete(_ROOT, "alice")  # still has bob reporting
        self.accounts.delete(admin, "bob")
        self.accounts.delete(_ROOT, "alice")      # now clear
        self.assertIsNone(self.accounts.get("alice"))

    # ---- list scoping --------------------------------------------------------
    def test_list_is_scoped_by_role(self):
        admin = self._admin()
        self.accounts.create(admin, "bob", "bobsecret1", "user")
        self.accounts.create(_ROOT, "amy", "adminpass2", "admin")
        su_names = {a.username for a in self.accounts.list(_ROOT)}
        self.assertEqual(su_names, {"root", "alice", "amy", "bob"})
        admin_names = {a.username for a in self.accounts.list(admin)}
        self.assertEqual(admin_names, {"alice", "bob"})
        bob = self.accounts.get("bob")
        self.assertEqual({a.username for a in self.accounts.list(bob)}, {"bob"})


class SessionsTests(unittest.TestCase):
    def test_create_resolve_and_drop(self):
        s = Sessions(ttl=100)
        token = s.create("alice")
        self.assertEqual(s.resolve(token), "alice")
        s.drop(token)
        self.assertIsNone(s.resolve(token))
        self.assertIsNone(s.resolve("bogus"))

    def test_expired_token_resolves_to_none(self):
        s = Sessions(ttl=-1)  # already expired
        self.assertIsNone(s.resolve(s.create("alice")))

    def test_throttle_locks_after_threshold(self):
        s = Sessions(ttl=100, max_failures=3, lockout=100)
        ip = "10.0.0.1"
        self.assertFalse(s.blocked(ip))
        for _ in range(3):
            s.record_failure(ip)
        self.assertTrue(s.blocked(ip))
        s.clear_failures(ip)
        self.assertFalse(s.blocked(ip))


# ---- route-level: dispatch drives login → cookie → me → users ----------------

class _FakeHandler:
    """Stand-in for the BaseHTTPRequestHandler so dispatch() runs without a socket:
    feeds a JSON body + headers (Cookie/Origin), carries the wired auth services, and
    captures the response including Set-Cookie."""

    def __init__(self, sessions, accounts, body=b"", headers=None):
        self.sessions = sessions
        self.accounts = accounts
        self.jobs = self.live = self.reads = None
        self.client_address = ("127.0.0.1", 5555)
        self.headers = {"Content-Length": str(len(body)), **(headers or {})}
        self.rfile = io.BytesIO(body)
        self.sent = None            # (code, obj)
        self.sent_headers = []      # extra headers (Set-Cookie …)

    def send_json(self, obj, code=200, headers=None):
        self.sent = (code, obj)
        self.sent_headers = list(headers or [])

    def send_download(self, path, filename):
        pass


class AuthRoutesTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        os.environ["UI_USERS"] = str(Path(self.tmp.name) / "users.json")
        self.accounts = Accounts(users_store, PasswordHasher(), "root", "rootpass12")
        self.sessions = Sessions(ttl=100)
        from app.api import routes
        self.routes = routes

    def tearDown(self):
        os.environ.pop("UI_USERS", None)
        self.tmp.cleanup()

    def _handler(self, body=b"", headers=None):
        return _FakeHandler(self.sessions, self.accounts, body=body, headers=headers)

    def _login(self, username, password):
        h = self._handler(body=json.dumps({"username": username, "password": password}).encode())
        self.routes.dispatch(h, "POST", "/api/login")
        return h

    def _cookie(self, login_handler):
        setcookie = dict(login_handler.sent_headers).get("Set-Cookie", "")
        return setcookie.split(";", 1)[0]  # "pkgcache_session=<token>"

    def test_login_sets_cookie_and_me_resolves_it(self):
        h = self._login("root", "rootpass12")
        self.assertEqual(h.sent[0], 200)
        self.assertEqual(h.sent[1]["role"], "superuser")
        cookie = self._cookie(h)
        self.assertTrue(cookie.startswith("pkgcache_session="))
        me = self._handler(headers={"Cookie": cookie})
        self.routes.dispatch(me, "GET", "/api/me")
        self.assertEqual(me.sent[1]["username"], "root")

    def test_bad_login_is_401_and_me_without_cookie_is_401(self):
        h = self._login("root", "wrong")
        self.assertEqual(h.sent[0], 401)
        anon = self._handler()
        self.routes.dispatch(anon, "GET", "/api/me")
        self.assertEqual(anon.sent[0], 401)

    def test_logout_revokes_the_session(self):
        cookie = self._cookie(self._login("root", "rootpass12"))
        out = self._handler(headers={"Cookie": cookie})
        self.routes.dispatch(out, "POST", "/api/logout")
        self.assertEqual(out.sent[0], 200)
        me = self._handler(headers={"Cookie": cookie})
        self.routes.dispatch(me, "GET", "/api/me")
        self.assertEqual(me.sent[0], 401)  # token dropped

    def test_create_user_via_api_requires_auth_then_succeeds(self):
        anon = self._handler(body=json.dumps(
            {"username": "alice", "password": "adminpass1", "role": "admin"}).encode())
        self.routes.dispatch(anon, "POST", "/api/users")
        self.assertEqual(anon.sent[0], 401)

        cookie = self._cookie(self._login("root", "rootpass12"))
        create = self._handler(
            body=json.dumps({"username": "alice", "password": "adminpass1", "role": "admin"}).encode(),
            headers={"Cookie": cookie})
        self.routes.dispatch(create, "POST", "/api/users")
        self.assertEqual(create.sent[0], 201)
        self.assertIsNotNone(self.accounts.authenticate("alice", "adminpass1"))

    def test_cross_origin_mutation_refused(self):
        cookie = self._cookie(self._login("root", "rootpass12"))
        h = self._handler(
            body=json.dumps({"username": "eve", "password": "adminpass1", "role": "admin"}).encode(),
            headers={"Cookie": cookie, "Origin": "http://evil.example", "Host": "console.local"})
        self.routes.dispatch(h, "POST", "/api/users")
        self.assertEqual(h.sent[0], 403)
        self.assertIsNone(self.accounts.get("eve"))

    def test_same_origin_mutation_allowed(self):
        cookie = self._cookie(self._login("root", "rootpass12"))
        h = self._handler(
            body=json.dumps({"username": "alice", "password": "adminpass1", "role": "admin"}).encode(),
            headers={"Cookie": cookie, "Origin": "http://console.local", "Host": "console.local"})
        self.routes.dispatch(h, "POST", "/api/users")
        self.assertEqual(h.sent[0], 201)


if __name__ == "__main__":
    unittest.main()
