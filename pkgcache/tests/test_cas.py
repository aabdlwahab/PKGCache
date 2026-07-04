"""Tests for the cross-project content-addressed store (CAS).

Two layers:
  * storage primitives — cas_link_from / cas_materialize / cas_path, over a real
    filesystem (hardlink sharing, digest validation, disabled store); and
  * Cache.fetch — a miss whose sha256 is known up front is served from the CAS by
    hardlink, WITHOUT touching the upstream stream_opener.

    cd pkgcache && .venv-test/bin/python -m pytest tests/test_cas.py -q
"""
from __future__ import annotations

import hashlib
import os

from pkgcache.core.cache import Cache
from pkgcache.core.inflight import InflightRegistry
from pkgcache.core.ledger import ArtifactRecord
from pkgcache.core.storage import Storage

_BODY = b"the exact same bytes two projects both want" * 1000


def _sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def _storage(tmp_path, cas=True):
    root = tmp_path / "proj" / "pip"
    cas_root = (tmp_path / ".cas") if cas else None
    return Storage(root, cas_root=cas_root)


# ---- storage primitives --------------------------------------------------

def test_link_from_then_materialize_shares_one_inode(tmp_path):
    st = _storage(tmp_path)
    hexd = _sha(_BODY)

    # A committed file in project A's tree, published to the CAS.
    a = st.root / "a" / "pkg.whl"
    a.parent.mkdir(parents=True)
    a.write_bytes(_BODY)
    st.cas_link_from(a, hexd)
    cp = st.cas_path(hexd)
    assert cp.exists()
    assert cp.stat().st_ino == a.stat().st_ino  # published by hardlink, not copy

    # Project B fetches the same content: materialized by hardlink, no download.
    b = st.root / "b" / "pkg.whl"
    assert st.cas_materialize(hexd, b) is True
    assert b.read_bytes() == _BODY
    assert b.stat().st_ino == cp.stat().st_ino
    assert cp.stat().st_nlink == 3  # CAS entry + A + B, one physical copy


def test_materialize_miss_returns_false(tmp_path):
    st = _storage(tmp_path)
    assert st.cas_materialize(_sha(b"never stored"), st.root / "x") is False
    assert not (st.root / "x").exists()


def test_cas_path_rejects_bad_digest_and_disabled_store(tmp_path):
    st = _storage(tmp_path)
    assert st.cas_path("nothex!!") is None      # non-hex
    assert st.cas_path("ab") is None            # too short
    assert st.cas_path(_sha(_BODY)) is not None

    off = _storage(tmp_path, cas=False)
    assert off.cas_root is None
    assert off.cas_path(_sha(_BODY)) is None
    assert off.cas_materialize(_sha(_BODY), off.root / "y") is False
    off.cas_link_from(off.root / "nope", _sha(_BODY))  # no-op, no raise


def test_link_from_is_idempotent(tmp_path):
    st = _storage(tmp_path)
    hexd = _sha(_BODY)
    a = st.root / "a.whl"
    a.write_bytes(_BODY)
    st.cas_link_from(a, hexd)
    first_ino = st.cas_path(hexd).stat().st_ino
    st.cas_link_from(a, hexd)  # second publish leaves the existing entry untouched
    assert st.cas_path(hexd).stat().st_ino == first_ino


# ---- Cache.fetch download-avoidance --------------------------------------

class _Progress:
    def record_recent(self, *a, **k): pass
    def start(self, *a, **k): pass
    def update(self, *a, **k): pass
    def complete(self, *a, **k): pass
    def error(self, *a, **k): pass


class _Stats:
    def __init__(self): self.traffic_calls = []
    def traffic(self, eco, *, hit, nbytes): self.traffic_calls.append((eco, hit, nbytes))


class _Ledger:
    def __init__(self): self.records = []
    async def arecord(self, rec): self.records.append(rec)


async def test_fetch_serves_from_cas_without_downloading(tmp_path):
    st = _storage(tmp_path)
    hexd = _sha(_BODY)
    # Seed the CAS as if project A had already fetched this content.
    seed = st.root / "seed.whl"
    seed.write_bytes(_BODY)
    st.cas_link_from(seed, hexd)

    ledger, stats = _Ledger(), _Stats()
    cache = Cache(st, InflightRegistry(), _Progress(), ledger, stats)

    def opener():
        raise AssertionError("upstream must not be contacted when the CAS has the content")

    def on_commit(size, h):
        return ArtifactRecord(ecosystem="pip", name="pkg", version="1.0",
                              digest=f"sha256:{h}", size=size)

    final = st.root / "b" / "pkg.whl"
    resp = await cache.fetch(
        key="pip/+f/pkg/pkg.whl", final_path=final, stream_opener=opener,
        name="pkg.whl", expected_sha256=hexd, on_commit=on_commit, eco="pip",
    )

    assert resp is not None
    assert final.exists()
    assert final.stat().st_ino == st.cas_path(hexd).stat().st_ino  # one physical copy
    # Recorded for THIS project's ledger, and counted as a saved-bytes hit.
    assert len(ledger.records) == 1
    assert ledger.records[0].digest == f"sha256:{hexd}"
    assert stats.traffic_calls == [("pip", True, len(_BODY))]


async def test_fetch_without_known_sha_falls_through_to_miss(tmp_path):
    # No expected_sha256 (npm/apt shape) → the CAS is skipped and the normal miss
    # path runs; here the opener is invoked (and we let it fail fast to prove it ran).
    st = _storage(tmp_path)
    cache = Cache(st, InflightRegistry(), _Progress(), _Ledger(), _Stats())
    called = {"n": 0}

    class _Boom(Exception):
        pass

    def opener():
        called["n"] += 1
        raise _Boom()

    final = st.root / "c" / "thing.tgz"
    try:
        await cache.fetch(key="npm/thing", final_path=final, stream_opener=opener,
                          name="thing.tgz", eco="npm")
    except Exception:
        pass
    assert called["n"] == 1  # the miss path (not the CAS) handled it
