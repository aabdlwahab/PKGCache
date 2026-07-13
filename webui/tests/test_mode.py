"""The docker-free mode switch and status probe.

mode() is a registry write (the instance-wide "*" soft flag) confirmed against the
cache process's /healthz; Reads.status() is an HTTP health probe. Neither touches
docker, so both are fully exercisable here with fetch_json stubbed.

    cd webui && python3 -m unittest tests.test_mode -v
"""
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable

# Throwaway registry BEFORE importing the modules (they resolve the path on demand).
_TMP = tempfile.mkdtemp()
os.environ["PKGCACHE_PROJECTS"] = str(Path(_TMP) / "projects.json")
os.environ["UI_USERS"] = str(Path(_TMP) / "users.json")

from app.services import operations as ops  # noqa: E402
from app.services import projects, reads  # noqa: E402


class InstanceOfflineFlagTests(unittest.TestCase):
    def setUp(self):
        projects.save_registry({"projects": {"proja": {}}, "tokens": {},
                                "offline": {"proja": True}})

    def test_set_clears_star_and_preserves_project_flags(self):
        projects.set_instance_offline(True)
        flags = projects.load_registry()["offline"]
        self.assertEqual(flags, {"*": True, "proja": True})
        self.assertTrue(projects.is_instance_offline())

        projects.set_instance_offline(False)
        flags = projects.load_registry()["offline"]
        self.assertEqual(flags, {"proja": True})  # per-project flag untouched
        self.assertFalse(projects.is_instance_offline())


class ModeOpTests(unittest.TestCase):
    """mode() = registry write + healthz confirm. fetch_json is stubbed per test;
    the confirm timeout is zeroed so the loop probes exactly once."""

    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})
        self._timeout = ops._MODE_CONFIRM_TIMEOUT
        ops._MODE_CONFIRM_TIMEOUT = 0
        self._fetch = ops.fetch_json

    def tearDown(self):
        ops._MODE_CONFIRM_TIMEOUT = self._timeout
        ops.fetch_json = self._fetch

    def _run(self, target):
        return "".join(ops.Operations().build("mode", {"target": target}))

    def test_offline_writes_star_and_confirms_via_healthz(self):
        ops.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": True}
        log = self._run("offline")
        self.assertTrue(projects.is_instance_offline())
        self.assertIn("confirmed offline", log)

    def test_online_clears_star(self):
        projects.set_instance_offline(True)
        ops.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": False}
        log = self._run("online")
        self.assertFalse(projects.is_instance_offline())
        self.assertIn("confirmed online", log)

    def test_online_reports_the_env_hard_pin(self):
        # The cache answers but stays offline → the OFFLINE env pins it; the op
        # must say so instead of pretending the switch took effect.
        ops.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": True}
        log = self._run("online")
        self.assertFalse(projects.is_instance_offline())  # flag still cleared
        self.assertIn("OFFLINE env", log)

    def test_unreachable_cache_still_saves_the_flag(self):
        ops.fetch_json = lambda url, timeout=2: None
        log = self._run("offline")
        self.assertTrue(projects.is_instance_offline())
        self.assertIn("unreachable", log)

    def test_no_docker_in_the_op(self):
        # The whole point of the rewrite: a mode switch must not shell out at all.
        calls = []
        ops.run = lambda *a, **k: calls.append(a)  # any subprocess use would land here
        try:
            ops.fetch_json = lambda url, timeout=2: {"offline": True}
            self._run("offline")
        finally:
            ops.run = ops.proc.run
        self.assertEqual(calls, [])


class StatusProbeTests(unittest.TestCase):
    def setUp(self):
        projects.save_registry({"projects": {}, "tokens": {}})
        self._fetch = reads.pkgcache.fetch_json
        self.reads = reads.Reads(usage=None)

    def tearDown(self):
        reads.pkgcache.fetch_json = self._fetch

    def test_reachable_online(self):
        reads.pkgcache.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": False}
        out = self.reads.status()
        self.assertTrue(out["available"])
        self.assertEqual(out["profile"], "online")
        self.assertEqual([s["state"] for s in out["services"]], ["running", "running"])

    def test_unreachable(self):
        reads.pkgcache.fetch_json = lambda url, timeout=2: None
        out = self.reads.status()
        self.assertFalse(out["available"])
        self.assertIsNone(out["profile"])
        self.assertEqual([s["state"] for s in out["services"]],
                         ["unreachable", "unreachable"])

    def test_instance_pin_reports_offline_profile(self):
        # Offline with NO global soft flag = an instance-wide pin (env or "*") —
        # what the console locks its per-project toggles on.
        reads.pkgcache.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": True}
        out = self.reads.status()
        self.assertEqual(out["profile"], "offline")

    def test_global_soft_flag_is_not_a_pin(self):
        # global's own toggle explains the offline healthz → other projects are
        # still switchable, so the profile must stay "online".
        projects.set_offline(projects.GLOBAL, True)
        reads.pkgcache.fetch_json = lambda url, timeout=2: {"status": "ok", "offline": True}
        out = self.reads.status()
        self.assertEqual(out["profile"], "online")


if __name__ == "__main__":
    unittest.main()
