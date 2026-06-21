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
        projects.save_registry({"pool": dict(projects.POOL_DEFAULT), "projects": {}})
        self.rec = projects.create("proja", probe=False)
        self.repo = projects.repo_dir("proja")

    def test_endpoints_use_allocated_ports(self):
        ep = config.endpoints("proja")
        npm_port = self.rec["ports"]["npm"]
        self.assertIn(f":{npm_port}/", ep["npm"])
        # Global endpoints stay on the default ports, untouched.
        self.assertIn(":4873/", config.endpoints("global")["npm"])

    def test_progress_and_health_sources_scoped(self):
        ps = config.progress_sources("proja")
        hs = config.health_sources("proja")
        self.assertEqual(ps["npm"], f"https://pkgcache:{self.rec['ports']['npm']}/-/progress")
        self.assertEqual(hs["apt"], f"http://pkgcache:{self.rec['ports']['apt']}/healthz")
        # apt is plain HTTP, the rest HTTPS.
        self.assertTrue(ps["docker"].startswith("https://"))
        self.assertTrue(ps["apt"].startswith("http://"))

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


if __name__ == "__main__":
    unittest.main()
