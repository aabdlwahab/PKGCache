"""Behavior tests for the lock-warm engine — parsing, index mapping, rewriting,
and the warm workflow (with an injected fake proxy, the one external system)."""
import sys
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))  # webui/ → `app` importable

from app.services.lockwarm import (  # noqa: E402
    IndexMap, LockError, LockParser, LockRewriter, WarmError, Warmer,
)

# A minimal but representative uv.lock: two registry packages (one with an sdist +
# wheel, one wheel-only) plus a virtual root that must be left untouched.
SAMPLE = '''version = 1
revision = 3
requires-python = ">=3.11"

[[package]]
name = "idna"
version = "3.10"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/f1/70/idna-3.10.tar.gz", hash = "sha256:aaa", size = 190490 }
wheels = [
    { url = "https://files.pythonhosted.org/packages/76/c6/idna-3.10-py3-none-any.whl", hash = "sha256:bbb", size = 70442 },
]

[[package]]
name = "Torch-Thing"
version = "2.6.0"
source = { registry = "https://download.pytorch.org/whl/cu124" }
wheels = [
    { url = "https://download.pytorch.org/whl/cu124/torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl", hash = "sha256:ccc", size = 10 },
]

[[package]]
name = "spike"
version = "0.1.0"
source = { virtual = "." }
'''

INDEXES = {
    "root/pypi": "https://pypi.org/simple",
    "root/pytorch-cu124": "https://download.pytorch.org/whl/cu124",
}


class LockParserTest(unittest.TestCase):
    def test_keeps_registry_packages_and_drops_virtual(self):
        packages = LockParser().parse(SAMPLE)
        self.assertEqual([p.name for p in packages], ["idna", "Torch-Thing"])

    def test_collects_sdist_and_wheels(self):
        idna = LockParser().parse(SAMPLE)[0]
        self.assertEqual(
            [f.filename for f in idna.files],
            ["idna-3.10.tar.gz", "idna-3.10-py3-none-any.whl"],
        )

    def test_normalizes_project_name(self):
        torch = LockParser().parse(SAMPLE)[1]
        self.assertEqual(torch.project, "torch-thing")

    def test_rejects_unknown_version(self):
        with self.assertRaises(LockError):
            LockParser().parse('version = 99\n')

    def test_rejects_garbage(self):
        with self.assertRaises(LockError):
            LockParser().parse("this is not toml = = =")


class IndexMapTest(unittest.TestCase):
    def test_maps_registry_ignoring_trailing_slash(self):
        m = IndexMap(INDEXES)
        self.assertEqual(m.index("https://pypi.org/simple/"), "root/pypi")
        self.assertEqual(m.index("https://download.pytorch.org/whl/cu124"), "root/pytorch-cu124")

    def test_unknown_registry_is_none(self):
        self.assertIsNone(IndexMap(INDEXES).index("https://example.com/simple"))


class LockRewriterTest(unittest.TestCase):
    def test_rewrites_registries_and_files_preserving_hashes(self):
        packages = LockParser().parse(SAMPLE)
        out = LockRewriter().rewrite(SAMPLE, packages, IndexMap(INDEXES), "https://cache.local:3141")
        # registries now point at the local +simple bases
        self.assertIn('registry = "https://cache.local:3141/root/pypi/+simple"', out)
        self.assertIn('registry = "https://cache.local:3141/root/pytorch-cu124/+simple"', out)
        # file URLs point at +f under the normalized project name
        self.assertIn("https://cache.local:3141/root/pypi/+f/idna/idna-3.10-py3-none-any.whl", out)
        self.assertIn(
            "https://cache.local:3141/root/pytorch-cu124/+f/torch-thing/torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl",
            out,
        )
        # the upstream hosts and hashes are gone / kept respectively
        self.assertNotIn("files.pythonhosted.org", out)
        self.assertIn('hash = "sha256:bbb"', out)
        # the virtual root is untouched
        self.assertIn('source = { virtual = "." }', out)

    def test_rewrites_under_a_project_prefix(self):
        # A named project's public base carries the /<project>/pypi prefix.
        packages = LockParser().parse(SAMPLE)
        out = LockRewriter().rewrite(SAMPLE, packages, IndexMap(INDEXES),
                                     "https://cache.local:3141/proja/pypi")
        self.assertIn('registry = "https://cache.local:3141/proja/pypi/root/pypi/+simple"', out)
        self.assertIn("https://cache.local:3141/proja/pypi/root/pypi/+f/idna/idna-3.10-py3-none-any.whl", out)


class WarmerTest(unittest.TestCase):
    """Warmer fans each locked file out to Proxy.warm concurrently and yields a
    result per file. A fake proxy stands in for the one external system."""

    class _FakeProxy:
        def __init__(self, status_by_file=None, raise_for=None):
            self.calls = []
            self._lock = threading.Lock()
            self._status = status_by_file or {}
            self._raise_for = raise_for or set()

        def warm(self, index, project, filename):
            with self._lock:
                self.calls.append((index, project, filename))
            if filename in self._raise_for:
                raise WarmError("proxy down")
            return self._status.get(filename, 200)

    def _items(self):
        packages = LockParser().parse(SAMPLE)
        index_map = IndexMap(INDEXES)
        return [(index_map.index(p.registry), p.project, f.filename)
                for p in packages for f in p.files]

    def test_warms_every_file_under_its_index(self):
        proxy = self._FakeProxy()
        results = list(Warmer(proxy, workers=4).warm(self._items()))
        self.assertTrue(all(r.ok for r in results))
        # every locked file was requested under the right index (order-independent)
        self.assertEqual(set(proxy.calls), {
            ("root/pypi", "idna", "idna-3.10.tar.gz"),
            ("root/pypi", "idna", "idna-3.10-py3-none-any.whl"),
            ("root/pytorch-cu124", "torch-thing", "torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl"),
        })

    def test_reports_http_and_transport_failures_per_file(self):
        proxy = self._FakeProxy(
            status_by_file={"idna-3.10.tar.gz": 404},
            raise_for={"torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl"},
        )
        by_name = {r.filename: r for r in Warmer(proxy, workers=4).warm(self._items())}
        self.assertFalse(by_name["idna-3.10.tar.gz"].ok)
        self.assertIn("[404]", by_name["idna-3.10.tar.gz"].detail)
        self.assertFalse(by_name["torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl"].ok)
        self.assertIn("unreachable", by_name["torch_thing-2.6.0-cp311-cp311-linux_x86_64.whl"].detail)
        self.assertTrue(by_name["idna-3.10-py3-none-any.whl"].ok)


if __name__ == "__main__":
    unittest.main()
