"""Read-only data for the API: cache contents (live, via pkgcache's /+ledger admin
endpoints — see the pkgcache gateway), the committed manifest, git history (via the
proc gateway), and cache-process status (via its /healthz — no docker involved).

The package/stats views used to open the per-project SQLite ledgers directly,
duplicating pkgcache's own Ledger.query. They now fetch pkgcache's /+ledger/artifacts
and /+ledger/stats per role and combine them here, so the ledger schema has a single
owner (pkgcache) and the two processes talk over HTTP, not a shared database file.
Owns nothing mutable — it reads on each call — so the only injected collaborator is
the disk-usage cache."""
import json
import statistics

from app import settings
from app.gateways import pkgcache, proc
from app.services import projects
from app.urls import health_sources

# eco label → the role whose ledger holds it, for mapping a per-eco row back to the
# role's bandwidth samples when estimating time saved (apt + apk share the apt role).
_ECO_ROLE = {"docker": "oci", "npm": "npm", "pip": "pypi", "apt": "apt",
             "apk": "apt", "git": "git", "files": "files"}


def _repo(project):
    """The cache repo dir for a project (global → caches/)."""
    return projects.repo_dir(project)


def _empty_eco(eco):
    return {"eco": eco, "count": 0, "size": 0, "requests": 0,
            "hit_count": 0, "hit_bytes": 0, "miss_count": 0, "miss_bytes": 0}


class Reads:
    """The read side of the control API: live cache contents (from pkgcache's ledger
    admin endpoints), the last-checkpoint manifest, cache-repo git history, and the
    cache process's health/mode status."""

    def __init__(self, usage) -> None:
        self._usage = usage

    def manifests(self, project=projects.GLOBAL):
        """Snapshot for /api/manifests: live cache contents (full inventory per eco) +
        how many each ecosystem has versioned in the last checkpoint, for THIS project."""
        committed = self._committed(project)
        return {
            "project": project,
            "ecosystems": {eco: pkgcache.ledger_artifacts(project, eco) for eco in settings.ECOS},
            "checkpointed": {eco: len(committed.get(eco, [])) for eco in settings.ECOS},
            "usage": self._usage.read(),  # disk footprint + deduplicated docker bytes (cached)
            "age": 0.0,  # read live on every request
        }

    def packages(self, project=projects.GLOBAL, *, eco=None, q=None, sort="name", page=1):
        """Server-side filter / sort / paginate for /api/packages — richer than the
        manifest view (origin, arch). The controller parses the HTTP query into these
        typed args, so this service never touches a request dict."""
        ecos = [eco] if eco in settings.ECOS else settings.ECOS
        return {
            "project": project,
            "ecosystems": {
                e: pkgcache.ledger_artifacts(project, e, q=q, sort=sort, page=page, page_size=1000)
                for e in ecos
            },
            "page": page,
            "sort": sort,
        }

    def stats(self, project=projects.GLOBAL):
        """Aggregate statistics for the stats tab — inventory, per-package request
        leaderboard, hit/miss traffic, and an estimated 'time saved' from passive
        upstream-bandwidth samples. Combines each role's /+ledger/stats slice."""
        role_stats = pkgcache.ledger_stats(project)  # {role: dict|None}
        by_eco_map = {eco: _empty_eco(eco) for eco in settings.ECOS}
        leaderboard = {eco: [] for eco in settings.ECOS}
        top_largest, recent_added, points = [], [], []
        arch_map, bw_by_role = {}, {}

        for role, data in role_stats.items():
            if not isinstance(data, dict):
                continue
            for eco, agg in data.get("by_eco", {}).items():
                if eco not in by_eco_map:
                    continue
                by_eco_map[eco].update(
                    count=agg.get("count", 0), size=agg.get("size", 0),
                    requests=agg.get("requests", 0),
                    hit_count=agg.get("hit_count", 0), hit_bytes=agg.get("hit_bytes", 0),
                    miss_count=agg.get("miss_count", 0), miss_bytes=agg.get("miss_bytes", 0))
            for eco, lb in data.get("leaderboard", {}).items():
                if eco in leaderboard:
                    leaderboard[eco] = lb
            for a in data.get("arch", []):
                m = arch_map.setdefault(a["arch"], [0, 0])
                m[0] += a.get("count", 0)
                m[1] += a.get("size", 0)
            top_largest.extend(data.get("top_largest", []))
            recent_added.extend(data.get("recent_added", []))
            bw_by_role[role] = data.get("bandwidth", [])
            points.extend(data.get("bandwidth_points", []))

        all_bps = [b for v in bw_by_role.values() for b in v]
        global_bps = statistics.median(all_bps) if all_bps else 0.0
        by_eco = list(by_eco_map.values())
        time_saved = 0.0
        for row in by_eco:
            role_bps = bw_by_role.get(_ECO_ROLE.get(row["eco"]), [])
            bps = statistics.median(role_bps) if role_bps else global_bps
            if bps > 0:
                time_saved += row["hit_bytes"] / bps

        hits = sum(r["hit_count"] for r in by_eco)
        misses = sum(r["miss_count"] for r in by_eco)
        top_largest.sort(key=lambda x: x["size"] or 0, reverse=True)
        recent_added.sort(key=lambda x: x["cached_at"] or "", reverse=True)
        points.sort(key=lambda x: x["ts"])
        arch = sorted(
            ({"arch": k, "count": v[0], "size": v[1]} for k, v in arch_map.items()),
            key=lambda x: x["count"], reverse=True,
        )[:12]

        return {
            "project": project,
            "totals": {
                "packages": sum(r["count"] for r in by_eco),
                "size": sum(r["size"] for r in by_eco),
                "requests": sum(r["requests"] for r in by_eco),
                "hits": hits,
                "misses": misses,
            },
            "hit_rate": round(hits / (hits + misses) * 100, 1) if (hits + misses) else None,
            "bytes_saved": sum(r["hit_bytes"] for r in by_eco),
            "time_saved_seconds": round(time_saved, 1),
            "by_eco": by_eco,
            "by_arch": arch,
            "leaderboard": leaderboard,
            "top_largest": top_largest[:15],
            "recent_added": recent_added[:15],
            "bandwidth": {
                "current_bps": round(global_bps, 1),
                "samples": points[-120:],
            },
            "usage": self._usage.read(),
        }

    def history(self, project=projects.GLOBAL):
        """Recent commits; checkpoints are the ones whose subject starts 'checkpoint:'.

        History = this project's cache repo checkpoint log, NOT the code repo. Until
        the first checkpoint creates its .git, there is no cache repo — return empty
        instead of letting git walk UP into the parent code repo's history."""
        repo = _repo(project)
        if not (repo / ".git").is_dir():
            return {"head": "", "commits": []}
        head = proc.git_head(repo)
        commits = []
        for full, short, date, subject in proc.git_log(repo, limit=50):
            commits.append({
                "hash": full, "short": short, "date": date, "subject": subject,
                "is_checkpoint": subject.startswith("checkpoint:"),
                "is_head": full == head,
            })
        return {"head": head, "commits": commits}

    def status(self):
        """Best-effort: is the cache process serving, and is the instance pinned
        offline? Probed over HTTP (the global project's /healthz on each listener) —
        the webui deliberately has no docker access, so 'up' means 'answering', not
        'container exists'. `profile` keeps its historical meaning for the console's
        hard-mode lock: "offline" when the INSTANCE is offline by something the
        per-project toggle can't override (the OFFLINE env or the mode op's "*"
        flag), "online" otherwise, None when the cache process is unreachable."""
        checks = health_sources(projects.GLOBAL)
        services, offline_global = [], None
        # One probe per listener: pip rides the unified HTTPS port, apt is the
        # separate plain-HTTP proxy port. All other roles share the unified listener.
        for eco, label in (("pip", "pkgcache (unified port)"), ("apt", "pkgcache (apt proxy)")):
            data = pkgcache.fetch_json(checks[eco], timeout=2)
            up = isinstance(data, dict)
            if up and offline_global is None:
                offline_global = bool(data.get("offline"))
            services.append({
                "name": label,
                "state": "running" if up else "unreachable",
                "status": ("offline" if data.get("offline") else "online") if up else "",
            })
        if offline_global is None:
            return {"available": False, "profile": None, "services": services}
        # Offline without the global project's OWN soft flag explaining it means an
        # instance-wide pin (env hard mode or the "*" switch) — what the console
        # locks its per-project toggles on.
        pinned = offline_global and not projects.is_offline(projects.GLOBAL)
        return {
            "available": True,
            "profile": "offline" if pinned else "online",
            "services": services,
        }

    def _committed(self, project=projects.GLOBAL):
        """The committed ledger — what the LAST checkpoint versioned (manifests/*.json),
        for THIS project's repo."""
        manifests = _repo(project) / "manifests"
        out = {}
        for eco in settings.ECOS:
            path = manifests / f"{eco}.json"
            try:
                out[eco] = json.loads(path.read_text())
            except (OSError, ValueError):
                out[eco] = []
        return out
