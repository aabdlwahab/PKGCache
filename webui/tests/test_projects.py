"""Tests for the project registry (webui/projects.py).

Run from the webui dir so the bare `import projects` resolves like the server does:

    cd webui && python3 -m unittest test_projects -v

Each test points the registry at a throwaway file and the cache repo at a tmp dir
(via env + monkeypatching) so nothing touches the real config/ or caches/. Projects
are port-less now — routed by URL prefix — so there is no allocator to exercise;
these cover the registry CRUD, name rules, and files write tokens.
"""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable


class RegistryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        tmp = Path(self.tmp.name)
        os.environ["PKGCACHE_PROJECTS"] = str(tmp / "projects.json")
        import importlib
        from app.services import projects
        importlib.reload(projects)  # re-read REGISTRY from the patched env
        self.projects = projects
        # Keep created cache dirs inside the tmp sandbox, not the real caches/.
        projects.CACHE_REPO = tmp / "caches"

    def tearDown(self):
        os.environ.pop("PKGCACHE_PROJECTS", None)
        self.tmp.cleanup()

    def test_global_is_implicit_and_reserved(self):
        p = self.projects
        self.assertTrue(p.exists(p.GLOBAL))
        self.assertEqual(p.ports(p.GLOBAL), p.ROLE_PORT)
        self.assertEqual(p.repo_dir(p.GLOBAL), p.CACHE_REPO)
        with self.assertRaises(p.ProjectError):
            p.create(p.GLOBAL)

    def test_all_projects_share_the_two_ports(self):
        p = self.projects
        rec = p.create("proja")
        self.assertEqual(rec["ports"], p.ROLE_PORT)
        self.assertEqual(p.ports("proja"), p.ROLE_PORT)
        # Everything HTTPS is the ONE unified port; apt keeps its plain-HTTP port.
        self.assertEqual({p.ROLE_PORT[r] for r in ("oci", "npm", "pypi", "git", "files")},
                         {p.UNIFIED_PORT})
        self.assertEqual(p.ROLE_PORT["apt"], p.APT_PORT)

    def test_create_persists_name_only_entry(self):
        p = self.projects
        p.create("proja")
        data = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(data["projects"]["proja"], {})   # no ports stored
        self.assertNotIn("pool", data)                    # allocator gone

    def test_role_prefix_scheme(self):
        p = self.projects
        # Uniform for every role and project, global included: /<project>/<role>.
        # (For oci/apt this is the internal admin surface; their client protocols
        # carry the project in the image name / proxy username instead.)
        self.assertEqual(p.role_prefix(p.GLOBAL, "npm"), "/global/npm")
        self.assertEqual(p.role_prefix("proja", "npm"), "/proja/npm")
        self.assertEqual(p.role_prefix("proja", "pypi"), "/proja/pypi")
        self.assertEqual(p.role_prefix("proja", "oci"), "/proja/oci")
        self.assertEqual(p.role_prefix(None, "files"), "/global/files")

    def test_write_tokens_rotate_persist_and_clean_up(self):
        p = self.projects
        # No token until one is generated; status reflects it.
        self.assertFalse(p.has_write_token("global"))
        self.assertIsNone(p.write_token("global"))
        t1 = p.rotate_write_token("global")
        self.assertTrue(t1 and p.has_write_token("global"))
        self.assertEqual(p.write_token("global"), t1)
        # It's persisted to the registry file under "tokens".
        stored = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(stored["tokens"]["global"], t1)
        # Rotation replaces it (old value gone).
        t2 = p.rotate_write_token("global")
        self.assertNotEqual(t1, t2)
        self.assertEqual(p.write_token("global"), t2)
        # A project's token is dropped when the project is deleted.
        p.create("proja")
        p.rotate_write_token("proja")
        self.assertTrue(p.has_write_token("proja"))
        p.delete("proja")
        self.assertFalse(p.has_write_token("proja"))

    def test_offline_flag_set_clear_and_persist(self):
        p = self.projects
        p.create("proja")
        self.assertFalse(p.is_offline("proja"))
        self.assertEqual(p.set_offline("proja", True), {"name": "proja", "offline": True})
        self.assertTrue(p.is_offline("proja"))
        stored = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(stored["offline"], {"proja": True})
        # Stored sparsely: clearing removes the entry rather than writing false.
        p.set_offline("proja", False)
        self.assertFalse(p.is_offline("proja"))
        stored = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(stored["offline"], {})

    def test_offline_flag_scopes_to_one_project(self):
        p = self.projects
        p.create("proja")
        p.create("projb")
        p.set_offline("proja", True)
        by_name = {rec["name"]: rec["offline"] for rec in p.list_projects()}
        self.assertEqual(by_name, {"global": False, "proja": True, "projb": False})

    def test_offline_flag_for_global(self):
        p = self.projects
        p.set_offline(p.GLOBAL, True)
        self.assertTrue(p.is_offline(p.GLOBAL))
        self.assertTrue(p.list_projects()[0]["offline"])

    def test_offline_unknown_project_rejected(self):
        p = self.projects
        with self.assertRaises(p.ProjectError):
            p.set_offline("ghost", True)

    def test_delete_clears_offline_flag(self):
        p = self.projects
        p.create("proja")
        p.set_offline("proja", True)
        p.delete("proja")
        stored = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(stored.get("offline", {}), {})

    def test_create_makes_cache_subdirs(self):
        p = self.projects
        p.create("proja")
        base = p.repo_dir("proja")
        for subdir in p.ROLE_SUBDIR.values():
            self.assertTrue((base / subdir).is_dir())

    def test_delete_leaves_cache_tree(self):
        p = self.projects
        p.create("proja")
        base = p.repo_dir("proja")
        p.delete("proja")
        self.assertFalse(p.exists("proja"))
        self.assertTrue(base.is_dir())  # bytes removed only by an explicit op

    def test_duplicate_rejected(self):
        p = self.projects
        p.create("proja")
        with self.assertRaises(p.ProjectError):
            p.create("proja")

    def test_bad_names_rejected(self):
        p = self.projects
        for bad in ("", "-bad", "bad-", "Bad", "a/b", "a--b", "a..b", ".a", "a_", "x" * 41):
            with self.assertRaises(p.ProjectError, msg=bad):
                p.create(bad)

    def test_valid_names_accepted(self):
        p = self.projects
        for ok in ("proja", "proj-a", "proj.a", "proj_a", "a1", "team-web.2"):
            p.create(ok)
            self.assertTrue(p.exists(ok))

    def test_reserved_names_rejected(self):
        p = self.projects
        # Role names, upstream aliases, global, and the registry roots collide with
        # the routers' meaning of the first segment, so they can't be project names.
        for bad in ("global", "oci", "npm", "pypi", "apt", "git", "files",
                    "dockerhub", "ghcr", "quay", "root", "v2"):
            with self.assertRaises(p.ProjectError, msg=bad):
                p.create(bad)

    def test_legacy_registry_with_pool_and_ports_is_read(self):
        p = self.projects
        # An old registry still carrying a pool + per-project ports must load, with
        # the stale ports simply ignored (every project answers on the shared ports).
        Path(os.environ["PKGCACHE_PROJECTS"]).write_text(json.dumps({
            "pool": {"start": 20000, "end": 20099},
            "projects": {"old": {"oci": 20000, "npm": 20001}},
            "tokens": {},
        }))
        self.assertTrue(p.exists("old"))
        self.assertEqual(p.ports("old"), p.ROLE_PORT)

    def test_corrupt_registry_fails_loudly_not_silently_empty(self):
        # A present-but-corrupt registry must NOT read as empty (that once handed two
        # projects the same ports). The gateway raises ApiError so a mutation aborts.
        from app.errors import ApiError
        from app.gateways import registry
        Path(os.environ["PKGCACHE_PROJECTS"]).write_text("{ not valid json")
        with self.assertRaises(ApiError):
            registry.load()
        with self.assertRaises(ApiError):
            self.projects.load_registry()  # the re-export goes through the gateway too

    def test_registry_path_follows_the_env(self):
        # The gateway resolves the path per call, so no import-time capture leaks the
        # real config/projects.json into a test.
        from app.gateways import registry
        self.assertEqual(str(registry.path()), os.environ["PKGCACHE_PROJECTS"])


if __name__ == "__main__":
    unittest.main()
