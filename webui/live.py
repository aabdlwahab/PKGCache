"""Live data polled from the proxies: in-flight downloads and the recent-pulls
feed. A background thread polls all roles' /_progress endpoints concurrently and
caches the merged snapshot; the API reads that cache."""
import json
import os
import ssl
import threading
import time
import urllib.request

from config import HEALTH_SOURCES, PROGRESS_SOURCES

_INTERNAL_TLS = ssl._create_unverified_context()  # internal progress polls only
DL_INTERVAL = float(os.environ.get("UI_DL_INTERVAL", "1.5"))
RECENT_MAX = 80

_downloads = {"sources": {}, "checked": 0.0}
_downloads_lock = threading.Lock()

_health = {"roles": {}, "checked": 0.0}
_health_lock = threading.Lock()


def _fetch_one(eco, url):
    """(eco, payload) where payload is the proxy's full progress JSON, or None if
    the proxy is unreachable."""
    try:
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        ctx = _INTERNAL_TLS if url.startswith("https") else None
        with urllib.request.urlopen(req, timeout=2, context=ctx) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return eco, data
    except Exception:  # noqa: BLE001 - unreachable proxy / between requests
        return eco, None


def _refresher():
    """Poll all proxies concurrently and cache the merged snapshot. A None value
    for an ecosystem means its proxy was unreachable (down or not in this profile)."""
    while True:
        collected = {}
        threads = []

        def worker(eco, url):
            k, v = _fetch_one(eco, url)
            collected[k] = v

        for eco, url in PROGRESS_SOURCES.items():
            t = threading.Thread(target=worker, args=(eco, url), daemon=True)
            t.start()
            threads.append(t)
        for t in threads:
            t.join(timeout=3)
        with _downloads_lock:
            _downloads["sources"] = collected
            _downloads["checked"] = time.time()
        _probe_health()
        time.sleep(DL_INTERVAL)


def _probe_health():
    """Probe each role's /healthz concurrently → {eco: {up, offline}}."""
    collected = {}
    threads = []

    def worker(eco, url):
        _, data = _fetch_one(eco, url)
        if isinstance(data, dict):
            collected[eco] = {"up": True, "offline": bool(data.get("offline"))}
        else:
            collected[eco] = {"up": False, "offline": None}

    for eco, url in HEALTH_SOURCES.items():
        t = threading.Thread(target=worker, args=(eco, url), daemon=True)
        t.start()
        threads.append(t)
    for t in threads:
        t.join(timeout=3)
    with _health_lock:
        _health["roles"] = collected
        _health["checked"] = time.time()


def roles_health():
    """Per-role health + an aggregate up-count and the real offline state. One
    container runs all roles, so offline is reported true only when the reachable
    roles agree on it."""
    with _health_lock:
        roles = dict(_health["roles"])
    offs = [v["offline"] for v in roles.values() if v["up"] and v["offline"] is not None]
    return {
        "roles": [{"role": eco, "up": v["up"], "offline": v["offline"]} for eco, v in roles.items()],
        "up": sum(1 for v in roles.values() if v["up"]),
        "offline": bool(offs) and all(offs),
    }


def start_refresher():
    threading.Thread(target=_refresher, daemon=True).start()


def live_downloads():
    """In-flight downloads per ecosystem (the 'downloads' field of each payload)."""
    with _downloads_lock:
        srcs = _downloads["sources"]
        checked = _downloads["checked"]
        downloads = {
            eco: (p.get("downloads", []) if isinstance(p, dict) else None)
            for eco, p in srcs.items()
        }
    return {
        "sources": downloads,
        "age": round(time.time() - checked, 1) if checked else None,
    }


def recent_pulls():
    """Merge each proxy's rolling 'recent' log into one time-sorted feed."""
    with _downloads_lock:
        srcs = dict(_downloads["sources"])
    merged = []
    for eco, payload in srcs.items():
        if not isinstance(payload, dict):
            continue
        for r in payload.get("recent", []):
            merged.append({
                "eco": eco,
                "name": r.get("name"),
                "id": r.get("id"),
                "size": r.get("size"),
                "hit": r.get("hit"),
                "failed": r.get("failed", False),
                "time": r.get("time"),
            })
    merged.sort(key=lambda x: x.get("time") or 0, reverse=True)
    return {"pulls": merged[:RECENT_MAX]}
