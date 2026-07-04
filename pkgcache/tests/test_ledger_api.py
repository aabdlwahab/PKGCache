"""Tests for the ledger admin API the control UI reads instead of opening ledger.db
directly: Ledger.query (all-rows mode + filter/sort/paginate) and Ledger.stats, plus
the /+ledger/* routes wired into the app (and NOT shadowed by the handler routes).

    cd pkgcache && .venv-test/bin/python -m pytest tests/test_ledger_api.py -q
"""
from __future__ import annotations

import time

from starlette.testclient import TestClient

from pkgcache.app import build_app
from pkgcache.core.config import Config
from pkgcache.core.ledger import ArtifactRecord, Ledger


def _seed(led: Ledger) -> None:
    led.record(ArtifactRecord(ecosystem="pip", name="numpy", version="2.1", digest="sha256:a", size=100, arch="amd64"))
    led.record(ArtifactRecord(ecosystem="pip", name="torch", version="2.3", digest="sha256:b", size=999, arch="amd64"))
    led.record(ArtifactRecord(ecosystem="pip", name="click", version="8.0", digest="sha256:c", size=5, arch=None))
    led.apply_stats(
        access={("pip", "numpy"): (7, time.time()), ("pip", "torch"): (3, time.time())},
        traffic={"pip": (5, 500, 2, 200)},
        bandwidth=[(1.0, 1000.0, "passive"), (2.0, 2000.0, "active")],
    )


def test_query_all_rows_when_page_size_zero(tmp_path):
    led = Ledger(tmp_path / "ledger.db")
    _seed(led)
    try:
        self_all = led.query(page_size=0)
        assert len(self_all) == 3                              # all rows, no page cap
        assert len(led.query(page_size=2)) == 2                # a positive page_size still paginates
        assert [r["name"] for r in led.query(ecosystem="pip", q="tor")] == ["torch"]
        sizes = [r["size"] for r in led.query(sort="size", page_size=0)]
        assert sizes == sorted(sizes)                          # sort whitelist honored
    finally:
        led.close()


def test_stats_shape_and_values(tmp_path):
    led = Ledger(tmp_path / "ledger.db")
    _seed(led)
    try:
        s = led.stats()
        pip = s["by_eco"]["pip"]
        assert pip["count"] == 3 and pip["size"] == 1104 and pip["requests"] == 10
        assert pip["hit_count"] == 5 and pip["hit_bytes"] == 500 and pip["miss_count"] == 2
        assert [x["name"] for x in s["leaderboard"]["pip"]][:2] == ["numpy", "torch"]
        assert s["top_largest"][0]["name"] == "torch"          # largest first
        assert s["recent_added"][0]["eco"] == "pip"
        assert sorted(s["bandwidth"]) == [1000.0, 2000.0]
        assert len(s["bandwidth_points"]) == 2
        arches = {a["arch"]: a["count"] for a in s["arch"]}
        assert arches["amd64"] == 2 and arches["(none)"] == 1  # the NULL-arch bucket
    finally:
        led.close()


def _app(tmp_path):
    cfg = Config(
        role="pypi", offline=True, project="global", cache_root=tmp_path / "pip",
        cas_root=None, host="127.0.0.1", port=0, request_timeout=1,
    )
    app = build_app(cfg, manage_lifecycle=False)  # no lifespan → no background tasks
    _seed(app.state.core.ledger)
    return app


def test_ledger_routes_are_served_and_not_shadowed(tmp_path):
    app = _app(tmp_path)
    with TestClient(app) as client:
        r = client.get("/+ledger/artifacts")
        assert r.status_code == 200
        assert len(r.json()["artifacts"]) == 3                 # default = all rows
        assert [a["name"] for a in client.get("/+ledger/artifacts?eco=pip&q=num").json()["artifacts"]] == ["numpy"]
        s = client.get("/+ledger/stats").json()
        assert s["by_eco"]["pip"]["count"] == 3
        assert s["by_eco"]["pip"]["hit_bytes"] == 500
