"""Integration tests for the project-scoped control-UI layer (config/reads/ops).

Exercises everything that does NOT need dvc/docker (those run only in the
container and are unchanged plumbing): port-derived endpoints, per-project ledger
reads, per-project git history, per-project shuttle paths, and the build()
dispatcher's project validation.

    cd webui && python3 -m unittest test_multiproject -v

Env is pointed at a throwaway registry + cache root before importing the modules,
so nothing touches the real config/ or caches/.
"""
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

# Throwaway registry must be set BEFORE importing the modules (they read it on import).
_TMP = tempfile.mkdtemp()
os.environ["PKGCACHE_PROJECTS"] = str(Path(_TMP) / "projects.json")

import projects  # noqa: E402

projects.CACHE_REPO = Path(_TMP) / "caches"  # keep created trees in the sandbox

import config  # noqa: E402
import ops  # noqa: E402
import reads  # noqa: E402
import usage  # noqa: E402


def _make_ledger(db_path, ecosystem, rows):
    """A minimal artifacts ledger matching what the proxies write / reads.py queries."""
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(db_path)
    conn.execute(
        "CREATE TABLE artifacts (name TEXT, version TEXT, digest TEXT, size INTEGER, "
        "origin TEXT, arch TEXT, cached_at REAL, ecosystem TEXT)"
    )
    conn.executemany(
        "INSERT INTO artifacts VALUES (?,?,?,?,?,?,?,?)",
        [(n, v, "sha256:x", 10, "upstream", "amd64", 1.0, ecosystem) for n, v in rows],
    )
    conn.commit()
    conn.close()


class MultiProjectTests(unittest.TestCase):
    def setUp(self):
        # Fresh registry per test.
        projects.save_registry({"projects": {}, "tokens": {}})
        self.rec = projects.create("proja")
        self.repo = projects.repo_dir("proja")

    def test_endpoints_use_project_prefix_on_shared_ports(self):
        ep = config.endpoints("proja")
        # Projects share the default ports; the project rides a URL prefix (or the
        # proxy username for apt). Global keeps its bare root URLs.
        self.assertIn(":4873/proja/npm/", ep["npm"])
        self.assertIn("/proja/pypi/root/pypi/+simple/", ep["pip"])
        self.assertIn("/proja/git/", ep["git"])
        self.assertIn("proja@", ep["apt"])                      # proxy username
        self.assertIn(":5000/proja/", ep["docker"])             # project in image name
        self.assertIn(":4873/", config.endpoints("global")["npm"])
        self.assertNotIn("/proja/", config.endpoints("global")["npm"])

    def test_progress_and_health_sources_scoped(self):
        ps = config.progress_sources("proja")
        hs = config.health_sources("proja")
        # Progress is per-project (per-core), reached by prefix on the shared port.
        self.assertEqual(ps["npm"], "https://pkgcache:4873/proja/npm/-/progress")
        self.assertEqual(ps["docker"], "https://pkgcache:5000/v2/proja/_progress")
        self.assertEqual(ps["apt"], "http://pkgcache:3142/proja/apt/acng-progress")
        self.assertTrue(ps["docker"].startswith("https://"))
        self.assertTrue(ps["apt"].startswith("http://"))
        # Health is per-SERVER now (projects share one process per role): the global
        # endpoints answer for every project.
        self.assertEqual(hs, config.health_sources("global"))
        self.assertEqual(hs["apt"], "http://pkgcache:3142/healthz")

    def test_packages_read_from_project_ledger(self):
        reader = reads.Reads(usage.Usage())
        _make_ledger(self.repo / "npm" / "ledger.db", "npm", [("left-pad", "1.3.0")])
        out = reader.packages({"project": ["proja"], "eco": ["npm"]})
        names = [r["name"] for r in out["ecosystems"]["npm"]]
        self.assertEqual(names, ["left-pad"])
        self.assertEqual(out["project"], "proja")
        # The global project has its own (here: empty) ledger — isolation holds.
        self.assertEqual(reader.packages({"project": ["global"], "eco": ["npm"]})
                         ["ecosystems"]["npm"], [])

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


if __name__ == "__main__":
    unittest.main()
