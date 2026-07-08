"""Integration tests for the project-scoped control-UI layer (config/reads/ops).

Exercises everything that does NOT need dvc/docker (those run only in the
container and are unchanged plumbing): port-derived endpoints, per-project ledger
reads, per-project git history, per-project shuttle paths, and the build()
dispatcher's project validation.

    cd webui && python3 -m unittest test_multiproject -v

Env is pointed at a throwaway registry + cache root before importing the modules,
so nothing touches the real config/ or caches/.
"""
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable

# Throwaway registry + users store must be set BEFORE importing the modules (they
# read the paths on demand). An empty users store with no root = auth disabled, so
# these config/reads/ops tests exercise the routes with enforcement OFF (its own
# suite in test_ownership covers enforcement ON).
_TMP = tempfile.mkdtemp()
os.environ["PKGCACHE_PROJECTS"] = str(Path(_TMP) / "projects.json")
os.environ["UI_USERS"] = str(Path(_TMP) / "users.json")

from app.services import projects  # noqa: E402

projects.CACHE_REPO = Path(_TMP) / "caches"  # keep created trees in the sandbox

from app import urls as config  # noqa: E402  -- URL/endpoint derivation (was config.py)
from app.api import routes  # noqa: E402  -- the declarative route table + dispatch
from app.services import operations as ops  # noqa: E402  -- keeps the `ops.` references
from app.services import reads  # noqa: E402
from app.services import usage  # noqa: E402


class MultiProjectTests(unittest.TestCase):
    def setUp(self):
        # Fresh registry per test.
        projects.save_registry({"projects": {}, "tokens": {}})
        self.rec = projects.create("proja")
        self.repo = projects.repo_dir("proja")

    def test_endpoints_use_project_prefix_on_the_unified_port(self):
        ep = config.endpoints("proja")
        # Endpoints are {url, note, setup} data (the console renders them). Everything
        # HTTPS shares the ONE unified port; the project rides a URL prefix (the image
        # name for docker, the proxy username for apt). Global is the literal project
        # `global`, fully qualified like any other.
        self.assertIn(":8443/proja/npm/", ep["npm"]["url"])
        self.assertIn(":8443/proja/pypi/root/pypi/+simple/", ep["pip"]["url"])
        self.assertIn(":8443/proja/git/", ep["git"]["url"])
        self.assertIn("proja@", ep["apt"]["url"])                 # proxy username
        self.assertIn(":3142", ep["apt"]["url"])                  # apt keeps its own port
        self.assertIn(":8443/proja/dockerhub", ep["docker"]["url"])  # project in image name
        g = config.endpoints("global")
        self.assertIn(":8443/global/npm/", g["npm"]["url"])
        self.assertIn(":8443/dockerhub", g["docker"]["url"])      # no image-name prefix
        self.assertNotIn("@", g["apt"]["url"])                    # no proxy username
        # Every eco ships copy-paste setup instructions.
        for eco, entry in ep.items():
            self.assertTrue(entry.get("setup"), f"{eco} has no setup lines")

    def test_progress_and_health_sources_scoped(self):
        ps = config.progress_sources("proja")
        hs = config.health_sources("proja")
        # Progress is per-project (per-core), via the uniform admin prefix on the
        # unified port (apt on its own plain-HTTP port).
        self.assertEqual(ps["npm"], "https://pkgcache:8443/proja/npm/-/progress")
        self.assertEqual(ps["docker"], "https://pkgcache:8443/proja/oci/v2/_progress")
        self.assertEqual(ps["apt"], "http://pkgcache:3142/proja/apt/acng-progress")
        self.assertTrue(ps["docker"].startswith("https://"))
        self.assertTrue(ps["apt"].startswith("http://"))
        # Health probes the same uniform admin form, per project.
        self.assertEqual(hs["apt"], "http://pkgcache:3142/proja/apt/healthz")
        self.assertEqual(hs["npm"], "https://pkgcache:8443/proja/npm/healthz")
        self.assertEqual(config.health_sources("global")["npm"],
                         "https://pkgcache:8443/global/npm/healthz")

    def test_packages_read_through_the_ledger_gateway_scoped(self):
        # reads.packages now fetches pkgcache's /+ledger/artifacts (per project+eco)
        # instead of opening ledger.db — stub the gateway and assert the project/eco
        # are threaded through and the rows come back untouched.
        from app.gateways import pkgcache
        calls = []

        def fake(project, eco, **kw):
            calls.append((project, eco))
            return [{"name": "left-pad", "version": "1.3.0"}] if (project, eco) == ("proja", "npm") else []

        orig = pkgcache.ledger_artifacts
        pkgcache.ledger_artifacts = fake
        try:
            reader = reads.Reads(usage.Usage())
            out = reader.packages("proja", eco="npm")
            self.assertEqual([r["name"] for r in out["ecosystems"]["npm"]], ["left-pad"])
            self.assertEqual(out["project"], "proja")
            self.assertEqual(reader.packages("global", eco="npm")["ecosystems"]["npm"], [])
        finally:
            pkgcache.ledger_artifacts = orig
        self.assertIn(("proja", "npm"), calls)

    def test_ledger_gateway_builds_prefixed_urls(self):
        # The gateway must hit each role's port + the project's URL prefix, and pass
        # the ecosystem filter (apk rides the apt role's ledger).
        from app.gateways import pkgcache
        seen = {}

        def fake_fetch(url, timeout):
            seen["url"] = url
            return {"artifacts": []}

        orig = pkgcache._fetch_ledger
        pkgcache._fetch_ledger = fake_fetch
        try:
            pkgcache.ledger_artifacts("proja", "npm")
            self.assertEqual(
                seen["url"].split("?")[0], "https://pkgcache:8443/proja/npm/+ledger/artifacts")
            pkgcache.ledger_artifacts("proja", "apk")
            base, query = seen["url"].split("?")
            self.assertEqual(base, "http://pkgcache:3142/proja/apt/+ledger/artifacts")
            self.assertIn("eco=apk", query)
            pkgcache.ledger_artifacts("global", "docker")
            self.assertEqual(
                seen["url"].split("?")[0], "https://pkgcache:8443/global/oci/+ledger/artifacts")
        finally:
            pkgcache._fetch_ledger = orig

    def test_stats_combines_per_role_slices(self):
        # reads.stats fans out to each role's /+ledger/stats and combines. Stub the
        # gateway's per-role fetch and check the cross-role aggregation.
        from app.gateways import pkgcache
        role_stats = {
            "npm": {"by_eco": {"npm": {"count": 2, "size": 30, "requests": 9,
                                       "hit_count": 4, "hit_bytes": 400,
                                       "miss_count": 1, "miss_bytes": 50}},
                    "leaderboard": {"npm": [{"name": "left-pad", "count": 9, "last_access": 1.0}]},
                    "arch": [{"arch": "amd64", "count": 2, "size": 30}],
                    "top_largest": [{"eco": "npm", "name": "left-pad", "version": "1", "size": 20}],
                    "recent_added": [{"eco": "npm", "name": "left-pad", "version": "1",
                                      "size": 20, "cached_at": "2026-01-01"}],
                    "bandwidth": [1000.0, 3000.0], "bandwidth_points": [{"ts": 1, "bps": 1000.0, "source": "passive"}]},
            "pypi": {"by_eco": {"pip": {"count": 1, "size": 5, "requests": 2,
                                        "hit_count": 1, "hit_bytes": 100,
                                        "miss_count": 0, "miss_bytes": 0}},
                     "arch": [{"arch": "amd64", "count": 1, "size": 5}],
                     "top_largest": [{"eco": "pip", "name": "numpy", "version": "2", "size": 999}],
                     "recent_added": [], "bandwidth": [], "bandwidth_points": []},
        }

        def fake_ledger_stats(project):
            return {role: role_stats.get(role) for role in
                    ("oci", "npm", "pypi", "apt", "git", "files")}

        orig = pkgcache.ledger_stats
        pkgcache.ledger_stats = fake_ledger_stats
        try:
            s = reads.Reads(usage.Usage()).stats("proja")
        finally:
            pkgcache.ledger_stats = orig

        self.assertEqual(s["totals"]["packages"], 3)          # 2 npm + 1 pip
        self.assertEqual(s["totals"]["size"], 35)
        self.assertEqual(s["totals"]["hits"], 5)              # 4 + 1
        self.assertEqual(s["bytes_saved"], 500)               # 400 + 100
        self.assertEqual(s["hit_rate"], round(5 / 6 * 100, 1))
        self.assertEqual(s["top_largest"][0]["name"], "numpy")  # sorted across roles
        self.assertEqual({a["arch"]: a["count"] for a in s["by_arch"]}["amd64"], 3)
        self.assertEqual(s["leaderboard"]["npm"][0]["name"], "left-pad")
        self.assertEqual(s["bandwidth"]["current_bps"], 2000.0)  # median of [1000,3000]

    def test_history_is_per_project_repo(self):
        reader = reads.Reads(usage.Usage())
        # No git yet → empty, and it must NOT fall back to the code repo's history.
        self.assertEqual(reader.history("proja"), {"head": "", "commits": []})
        env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
               "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"}
        subprocess.run(["git", "init", "-q"], cwd=self.repo, check=True, env=env)
        (self.repo / "f").write_text("x")
        subprocess.run(["git", "add", "-A"], cwd=self.repo, check=True, env=env)
        subprocess.run(["git", "commit", "-qm", "checkpoint: first"], cwd=self.repo, check=True, env=env)
        hist = reads.Reads(usage.Usage()).history("proja")
        self.assertEqual(len(hist["commits"]), 1)
        self.assertTrue(hist["commits"][0]["is_checkpoint"])

    def test_shuttle_dirs_nest_per_project(self):
        self.assertEqual(ops._export_dir("global"), ops.EXPORT_DIR)
        self.assertEqual(ops._export_dir("proja"), ops.EXPORT_DIR / "projects" / "proja")
        self.assertEqual(ops._import_dir("proja"), ops.IMPORT_DIR / "projects" / "proja")
        self.assertEqual(ops.shuttle_info("proja")["project"], "proja")

    def _seed_obj(self, md5_dir):
        (md5_dir / "28").mkdir(parents=True)
        (md5_dir / "28" / "9ff72645a51917f5f945b101bb2414.dir").write_text("[]")

    def test_find_md5_tree_all_copy_shapes(self):
        base = Path(tempfile.mkdtemp())
        cases = {
            "canon": Path("dvcstore") / "files" / "md5",  # canonical DVC 3.x
            "nowrap": Path("files") / "md5",               # dvcstore/ stripped
            "nofiles": Path("dvcstore") / "md5",           # files/ level stripped
            "raw": Path("md5"),                            # both stripped (gamma-2)
        }
        for name, rel in cases.items():
            d = base / name
            self._seed_obj(d / rel)
            self.assertEqual(ops._find_md5_tree(d), d / rel, name)

    def test_find_md5_tree_missing_or_skeleton(self):
        base = Path(tempfile.mkdtemp())
        empty = base / "empty"
        empty.mkdir()
        self.assertIsNone(ops._find_md5_tree(empty))
        # Only the empty 2-char fan-out dirs (an aborted copy) — not a real store.
        skel = base / "skel"
        (skel / "md5" / "00").mkdir(parents=True)
        (skel / "md5" / "ff").mkdir(parents=True)
        self.assertIsNone(ops._find_md5_tree(skel))

    def test_normalize_dvcstore_relocates_raw_tree(self):
        # The gamma-2 shape: a raw md5/ tree (no dvcstore/, no files/). Normalizing
        # must move it to dvcstore/files/md5 and return the dvcstore/ remote root.
        imp = Path(tempfile.mkdtemp())
        self._seed_obj(imp / "md5")
        tree = ops._find_md5_tree(imp)
        root = ops._normalize_dvcstore(imp, tree)
        self.assertEqual(root, imp / "dvcstore")
        moved = imp / "dvcstore" / "files" / "md5" / "28" / "9ff72645a51917f5f945b101bb2414.dir"
        self.assertTrue(moved.is_file())
        self.assertFalse((imp / "md5").exists())

    def test_normalize_dvcstore_canonical_is_noop(self):
        imp = Path(tempfile.mkdtemp())
        canon = imp / "dvcstore" / "files" / "md5"
        self._seed_obj(canon)
        tree = ops._find_md5_tree(imp)
        self.assertEqual(tree, canon)
        root = ops._normalize_dvcstore(imp, tree)
        self.assertEqual(root, imp / "dvcstore")
        self.assertTrue((canon / "28" / "9ff72645a51917f5f945b101bb2414.dir").is_file())

    def test_build_validates_project_existence(self):
        operations = ops.Operations()
        with self.assertRaises(ops.OpError):
            operations.build("checkpoint", {"project": "ghost", "message": "x"})
        # A known project returns a generator (not executed — needs dvc).
        gen = operations.build("checkpoint", {"project": "proja", "message": "x"})
        self.assertTrue(hasattr(gen, "__next__"))
        # import may register a brand-new project, so non-existence is allowed there.
        self.assertTrue(hasattr(operations.build("import", {"project": "fresh"}), "__next__"))


class SharedDvcCacheTests(unittest.TestCase):
    """Phase 1 offline import dedup: the shared DVC object store wiring. dvc itself
    runs only in the container, so we test the pure-Python pieces — the command
    construction (stubbed run) and the hardlink-breaking ledger detach (real fs)."""

    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})
        projects.create("proja")
        self.repo = projects.repo_dir("proja")
        # Keep the shared store inside the sandbox, not the real caches/ tree
        # (ops._SHARED_DVC_STORE derives from the real repo root at import time).
        self._orig_store = ops._SHARED_DVC_STORE
        ops._SHARED_DVC_STORE = Path(tempfile.mkdtemp()) / ".dvc-shared"

    def tearDown(self):
        ops._SHARED_DVC_STORE = self._orig_store

    def test_use_shared_dvc_cache_writes_local_config(self):
        # Stub run() so no real dvc is needed; capture the argv of each call.
        calls = []

        def fake_run(cmd, env=None, cwd=None):
            calls.append((cmd, str(cwd)))
            yield "$ " + " ".join(cmd) + "\n"

        orig = ops.run
        ops.run = fake_run
        try:
            list(ops._use_shared_dvc_cache(self.repo))
        finally:
            ops.run = orig

        # The store dir is created, and every dvc write targets .dvc/config.local
        # (--local) in THIS repo — never the tracked config the bundle carries.
        self.assertTrue(ops._SHARED_DVC_STORE.is_dir())
        self.assertEqual(
            calls[0][0],
            ["dvc", "cache", "dir", "--local", str(ops._SHARED_DVC_STORE)],
        )
        self.assertEqual(calls[1][0], ["dvc", "config", "--local", "cache.type", "reflink,hardlink,copy"])
        self.assertTrue(all(c[1] == str(self.repo) for c in calls))
        self.assertTrue(all("--local" in c[0] for c in calls))

    def test_unshare_ledgers_breaks_the_hardlink(self):
        # Simulate a hardlink-mode checkout: the role's ledger.db is a link into the
        # shared store (nlink == 2), sharing an inode with the store object.
        db = self.repo / "docker" / "ledger.db"
        db.parent.mkdir(parents=True, exist_ok=True)
        db.write_bytes(b"LEDGER-V1")
        store_obj = Path(tempfile.mkdtemp()) / "store-object"
        os.link(db, store_obj)
        self.assertEqual(os.stat(db).st_nlink, 2)

        list(ops._unshare_ledgers(self.repo))

        # The repo copy is now a private inode; the store object is untouched.
        self.assertEqual(os.stat(db).st_nlink, 1)
        self.assertNotEqual(os.stat(db).st_ino, os.stat(store_obj).st_ino)
        self.assertEqual(db.read_bytes(), b"LEDGER-V1")
        # A subsequent in-place write to the ledger must NOT reach the shared store.
        db.write_bytes(b"LEDGER-V2-written-by-proxy")
        self.assertEqual(store_obj.read_bytes(), b"LEDGER-V1")

    def test_unshare_ledgers_skips_absent_dbs(self):
        # No ledger.db in any role dir → clean no-op, no exception, no stray files.
        list(ops._unshare_ledgers(self.repo))
        self.assertFalse((self.repo / "docker" / "ledger.db.unshare").exists())

    def test_ignore_shared_store_when_inside_repo(self):
        # Store inside the repo (the global-repo case): must be git-ignored so the
        # checkpoint's `git add -A` never stages the raw object bytes. Idempotent.
        repo = Path(tempfile.mkdtemp())
        ops._SHARED_DVC_STORE = repo / ".dvc-shared"
        ops._ignore_shared_store(repo)
        ops._ignore_shared_store(repo)  # second call must not duplicate the entry
        body = (repo / ".gitignore").read_text()
        self.assertEqual(body.count("/.dvc-shared/"), 1)

    def test_ignore_shared_store_noop_when_outside_repo(self):
        # Store outside the repo (the named-project case): nothing to ignore.
        repo = Path(tempfile.mkdtemp())
        ops._SHARED_DVC_STORE = Path(tempfile.mkdtemp()) / ".dvc-shared"
        ops._ignore_shared_store(repo)
        self.assertFalse((repo / ".gitignore").exists())


class PrefixRoutingFixTests(unittest.TestCase):
    """Regressions from the port-pool → URL-prefix migration: every internal call
    to a named project's role must carry its prefix (the shared ports alone reach
    only the GLOBAL sub-app), and the HTTP layer's name patterns must accept the
    same grammar validate_name does."""

    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})
        projects.create("proja")

    def test_files_proxy_target_carries_project_prefix(self):
        from app.gateways import pkgcache
        # Every project INCLUDING global is fully qualified on the unified port —
        # without the prefix, a console upload for proja lands in the GLOBAL tree.
        self.assertEqual(pkgcache.files_target("global", "a/b.txt"), "/global/files/a/b.txt")
        self.assertEqual(pkgcache.files_target("proja", "a/b.txt"), "/proja/files/a/b.txt")
        self.assertEqual(
            pkgcache.files_target("proja", "a/b.txt", overwrite=True),
            "/proja/files/a/b.txt?overwrite=1",
        )
        # Path segments are quoted; the separator survives.
        self.assertEqual(pkgcache.files_target("global", "dir/a b.txt"),
                         "/global/files/dir/a%20b.txt")
        with self.assertRaises(projects.ProjectError):
            pkgcache.files_target("ghost", "a.txt")

    def test_git_maintain_url_carries_project_prefix(self):
        # Checkpoint's mirror repack must hit THIS project's git sub-app, not global's.
        self.assertEqual(ops._git_maintain_url("global"),
                         "https://pkgcache:8443/global/git/+maintain")
        self.assertEqual(ops._git_maintain_url("proja"),
                         "https://pkgcache:8443/proja/git/+maintain")
        with self.assertRaises(projects.ProjectError):
            ops._git_maintain_url("ghost")

    def test_project_delete_route_accepts_full_name_grammar(self):
        import re
        from app.api import routes
        # Names with '.' and '_' are creatable, so the DELETE route must match them
        # (validate_name is the gatekeeper, not the route pattern).
        pat = re.compile(routes._PROJECT_PATH)
        for name in ("proja", "my_app", "team.web-2"):
            m = pat.fullmatch(f"/api/projects/{name}")
            self.assertIsNotNone(m, name)
            self.assertEqual(m.group("name"), name)
        self.assertIsNone(pat.fullmatch("/api/projects/a/b"))


class _FakeHandler:
    """Stand-in for the BaseHTTPRequestHandler so dispatch() can be driven without a
    socket: captures the response, feeds a JSON body, and carries the wired services.
    Auth is disabled here (no root, empty store), so the authorization guards are no-
    ops and these tests see the pre-auth behaviour."""

    def __init__(self, body=b"", jobs=None, live=None, reads=None):
        from app.gateways import users
        from app.services.accounts import Accounts
        from app.services.passwords import PasswordHasher
        from app.services.sessions import Sessions
        self.jobs, self.live, self.reads = jobs, live, reads
        self.accounts = Accounts(users, PasswordHasher(), None, None)  # disabled
        self.sessions = Sessions(ttl=100)
        self.client_address = ("127.0.0.1", 0)
        self.headers = {"Content-Length": str(len(body))}
        self.rfile = io.BytesIO(body)
        self.sent = None       # (code, obj) from send_json
        self.downloaded = None  # (path, filename) from send_download

    def send_json(self, obj, code=200, headers=None):
        self.sent = (code, obj)

    def send_download(self, path, filename):
        self.downloaded = (str(path), filename)


class DispatchContractTests(unittest.TestCase):
    """The route table + single error contract (Phase 2): a matched route runs its
    controller; a service ApiError becomes {"error": …} with its status; an unknown
    path is a miss (→ 404 at the handler); a bad JSON body is a 400, not a 500."""

    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})

    def test_exact_route_runs_and_serializes(self):
        h = _FakeHandler()
        self.assertTrue(routes.dispatch(h, "GET", "/healthz"))
        self.assertEqual(h.sent, (200, {"status": "ok"}))

    def test_unknown_path_is_a_miss(self):
        h = _FakeHandler()
        self.assertFalse(routes.dispatch(h, "GET", "/api/nope"))
        self.assertIsNone(h.sent)

    def test_method_is_part_of_the_key(self):
        # /api/projects is GET (list) and POST (create); a DELETE to it must miss.
        self.assertFalse(routes.dispatch(_FakeHandler(), "DELETE", "/api/projects"))

    def test_service_error_maps_to_its_status(self):
        # Deleting an unregistered project raises ProjectError (ApiError, 400).
        h = _FakeHandler()
        self.assertTrue(routes.dispatch(h, "DELETE", "/api/projects/ghost"))
        code, obj = h.sent
        self.assertEqual(code, 400)
        self.assertIn("error", obj)

    def test_bad_json_body_is_a_400_not_a_500(self):
        h = _FakeHandler(body=b"{not json")
        self.assertTrue(routes.dispatch(h, "POST", "/api/projects"))
        self.assertEqual(h.sent[0], 400)

    def test_capture_group_reaches_the_controller(self):
        projects.create("keeper")
        h = _FakeHandler()
        self.assertTrue(routes.dispatch(h, "DELETE", "/api/projects/keeper"))
        self.assertEqual(h.sent[0], 200)
        self.assertFalse(projects.exists("keeper"))


class ProjectModeTests(unittest.TestCase):
    """POST /api/projects/<name>/mode flips ONE project's soft offline flag in the
    registry — the pkgcache supervisor picks it up on its next poll. Distinct from
    the instance-wide `mode` job (container recreate)."""

    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})

    def _post(self, name, target):
        h = _FakeHandler(body=json.dumps({"target": target}).encode())
        self.assertTrue(routes.dispatch(h, "POST", f"/api/projects/{name}/mode"))
        return h.sent

    def test_offline_then_online_round_trip(self):
        projects.create("proja")
        code, obj = self._post("proja", "offline")
        self.assertEqual((code, obj), (200, {"name": "proja", "offline": True}))
        self.assertTrue(projects.is_offline("proja"))
        code, obj = self._post("proja", "online")
        self.assertEqual((code, obj), (200, {"name": "proja", "offline": False}))
        self.assertFalse(projects.is_offline("proja"))

    def test_global_is_a_valid_target(self):
        code, obj = self._post("global", "offline")
        self.assertEqual((code, obj), (200, {"name": "global", "offline": True}))

    def test_bad_target_is_a_400(self):
        projects.create("proja")
        code, obj = self._post("proja", "sideways")
        self.assertEqual(code, 400)
        self.assertIn("error", obj)
        self.assertFalse(projects.is_offline("proja"))

    def test_unknown_project_is_a_400(self):
        code, obj = self._post("ghost", "offline")
        self.assertEqual(code, 400)
        self.assertIn("error", obj)


class OriginGuardTests(unittest.TestCase):
    """The CSRF Origin check on mutating requests compares HOSTNAMES, so a reverse
    proxy that rewrites the Host (nginx `$host` drops the port while the browser's
    Origin keeps it) doesn't wrongly reject same-site requests — but a genuinely
    cross-site Origin is still refused."""

    def _post(self, origin=None, host=None):
        h = _FakeHandler(body=b'{"username":"x","password":"y"}')
        if origin is not None:
            h.headers["Origin"] = origin
        if host is not None:
            h.headers["Host"] = host
        routes.dispatch(h, "POST", "/api/login")
        return h.sent

    def test_absent_origin_allowed(self):
        # A non-browser client (curl) sends no Origin — no CSRF vector, so allow it.
        code, _ = self._post(host="cache:8088")
        self.assertNotEqual(code, 403)

    def test_same_hostname_different_port_allowed(self):
        # The real bug: browser Origin carries :8088, nginx forwarded Host without it.
        code, obj = self._post(origin="http://cache:8088", host="cache")
        self.assertNotEqual(code, 403)
        self.assertNotIn("cross-origin", obj.get("error", ""))

    def test_cross_host_refused(self):
        code, obj = self._post(origin="http://evil.example", host="cache:8088")
        self.assertEqual(code, 403)
        self.assertIn("cross-origin", obj["error"])


if __name__ == "__main__":
    unittest.main()
