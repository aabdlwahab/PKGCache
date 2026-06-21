"""Live data polled from the proxies: in-flight downloads and the recent-pulls
feed. A background thread polls every project's role /_progress endpoints
concurrently and caches the merged snapshot; the API reads that cache, filtered to
the requested project.

One central process serves the global project (default ports) plus every registered
project (its own ports); the poller fans out across all of them so the UI can show
live activity for whichever project the operator is viewing. Cached snapshots are
keyed by (project, eco)."""
import json
import os
import ssl
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

import projects
from config import health_sources, progress_sources

_INTERNAL_TLS = ssl._create_unverified_context()  # internal progress polls only
DL_INTERVAL = float(os.environ.get("UI_DL_INTERVAL", "1.5"))
RECENT_MAX = 80
# Bound concurrency so a host with many projects doesn't spawn hundreds of threads
# per poll cycle (4 progress + 4 health endpoints per project).
_POLL_WORKERS = int(os.environ.get("UI_POLL_WORKERS", "16"))


def _fetch_one(url):
    """The proxy's full progress JSON, or None if unreachable."""
    try:
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        ctx = _INTERNAL_TLS if url.startswith("https") else None
        with urllib.request.urlopen(req, timeout=2, context=ctx) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception:  # noqa: BLE001 - unreachable proxy / between requests
        return None


def _for_project(mapping, project):
    """{eco: value} for one project, from a (project, eco)-keyed snapshot."""
    return {eco: val for (proj, eco), val in mapping.items() if proj == project}


class LiveFeed:
    """Owns the background poller and the merged (project, eco)-keyed snapshots of
    in-flight downloads and per-role health. The API methods (`downloads`, `recent`,
    `health`) read the cached snapshot filtered to one project; `start()` launches
    the poll loop."""

    def __init__(self) -> None:
        self._pool = ThreadPoolExecutor(max_workers=_POLL_WORKERS)
        # A None payload for a (project, eco) = that role was unreachable.
        self._downloads = {"sources": {}, "checked": 0.0}
        self._downloads_lock = threading.Lock()
        self._health = {"roles": {}, "checked": 0.0}
        self._health_lock = threading.Lock()

    def start(self) -> None:
        threading.Thread(target=self._refresh, daemon=True).start()

    def downloads(self, project=projects.GLOBAL):
        """In-flight downloads per ecosystem (the 'downloads' field of each payload)
        for ONE project."""
        with self._downloads_lock:
            srcs = _for_project(self._downloads["sources"], project)
            checked = self._downloads["checked"]
        downloads = {
            eco: (p.get("downloads", []) if isinstance(p, dict) else None)
            for eco, p in srcs.items()
        }
        return {
            "project": project,
            "sources": downloads,
            "age": round(time.time() - checked, 1) if checked else None,
        }

    def recent(self, project=projects.GLOBAL):
        """Merge ONE project's per-role rolling 'recent' logs into one time-sorted feed."""
        with self._downloads_lock:
            srcs = _for_project(self._downloads["sources"], project)
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
        return {"project": project, "pulls": merged[:RECENT_MAX]}

    def health(self, project=projects.GLOBAL):
        """Per-role health + an aggregate up-count and the real offline state for ONE
        project. The roles all run in the single container, so offline is reported true
        only when the reachable roles agree on it."""
        with self._health_lock:
            roles = _for_project(self._health["roles"], project)
        offs = [v["offline"] for v in roles.values() if v["up"] and v["offline"] is not None]
        return {
            "project": project,
            "roles": [{"role": eco, "up": v["up"], "offline": v["offline"]} for eco, v in roles.items()],
            "up": sum(1 for v in roles.values() if v["up"]),
            "offline": bool(offs) and all(offs),
        }

    def _project_names(self):
        """Every project to poll, refreshed each cycle so created/deleted projects are
        picked up without restarting the poller."""
        try:
            return [p["name"] for p in projects.list_projects()]
        except Exception:  # noqa: BLE001 - a bad registry write shouldn't kill the loop
            return [projects.GLOBAL]

    def _refresh(self):
        """Poll all projects' progress + health concurrently; cache the merged
        snapshots. Keys are (project, eco); a None value means that role was unreachable
        (down or not in the current profile)."""
        while True:
            names = self._project_names()

            prog_targets, health_targets = [], []  # [((project, eco), url)]
            for name in names:
                for eco, url in progress_sources(name).items():
                    prog_targets.append(((name, eco), url))
                for eco, url in health_sources(name).items():
                    health_targets.append(((name, eco), url))

            prog = dict(zip(
                (k for k, _ in prog_targets),
                self._pool.map(_fetch_one, [u for _, u in prog_targets]),
            ))
            health = {}
            for key, data in zip(
                (k for k, _ in health_targets),
                self._pool.map(_fetch_one, [u for _, u in health_targets]),
            ):
                health[key] = ({"up": True, "offline": bool(data.get("offline"))}
                               if isinstance(data, dict) else {"up": False, "offline": None})

            now = time.time()
            with self._downloads_lock:
                self._downloads["sources"], self._downloads["checked"] = prog, now
            with self._health_lock:
                self._health["roles"], self._health["checked"] = health, now
            time.sleep(DL_INTERVAL)
