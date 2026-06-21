"""Tests for the project registry + port allocator (webui/projects.py).

Run from the webui dir so the bare `import projects` resolves like the server does:

    cd webui && python3 -m unittest test_projects -v

Each test points the registry at a throwaway file and the cache repo at a tmp dir
(via env + monkeypatching) so nothing touches the real config/ or caches/. Port
probing is disabled so allocation is deterministic and doesn't bind real sockets.
"""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))


class AllocatorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        tmp = Path(self.tmp.name)
        os.environ["PKGCACHE_PROJECTS"] = str(tmp / "projects.json")
        import importlib
        import projects
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
            p.create(p.GLOBAL, probe=False)

    def test_next_free_allocation_is_lowest_and_contiguous(self):
        p = self.projects
        a = p.create("proja", probe=False)
        b = p.create("projb", probe=False)
        start = p.POOL_DEFAULT["start"]
        # First project gets the four lowest pool ports, in role order; second the next four.
        self.assertEqual([a["ports"][r] for r in p.ROLES], [start, start + 1, start + 2, start + 3])
        self.assertEqual([b["ports"][r] for r in p.ROLES], [start + 4, start + 5, start + 6, start + 7])

    def test_reserved_default_ports_never_handed_out(self):
        p = self.projects
        rec = p.create("proja", probe=False)
        self.assertFalse(set(rec["ports"].values()) & set(p.ROLE_PORT.values()))

    def test_freed_ports_are_reused(self):
        p = self.projects
        first = p.create("proja", probe=False)["ports"]
        p.delete("proja")
        again = p.create("projc", probe=False)["ports"]
        self.assertEqual(again, first)  # lowest-free reclaims the gap

    def test_ports_persist_across_reload(self):
        p = self.projects
        rec = p.create("proja", probe=False)
        data = json.loads(Path(os.environ["PKGCACHE_PROJECTS"]).read_text())
        self.assertEqual(data["projects"]["proja"], rec["ports"])

    def test_create_makes_cache_subdirs(self):
        p = self.projects
        p.create("proja", probe=False)
        base = p.repo_dir("proja")
        for subdir in p.ROLE_SUBDIR.values():
            self.assertTrue((base / subdir).is_dir())

    def test_duplicate_and_bad_names_rejected(self):
        p = self.projects
        p.create("proja", probe=False)
        with self.assertRaises(p.ProjectError):
            p.create("proja", probe=False)
        for bad in ("", "-bad", "bad-", "Bad", "a/b", "x" * 41):
            with self.assertRaises(p.ProjectError):
                p.create(bad, probe=False)

    def test_pool_exhaustion_raises(self):
        p = self.projects
        reg = p.load_registry()
        reg["pool"] = {"start": 30000, "end": 30005}  # room for one project (4 ports), not two
        p.save_registry(reg)
        p.create("proja", probe=False)
        with self.assertRaises(p.ProjectError):
            p.create("projb", probe=False)


if __name__ == "__main__":
    unittest.main()
