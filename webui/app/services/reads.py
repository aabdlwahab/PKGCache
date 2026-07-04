"""Read-only data for the API: cache contents (live, from the SQLite ledgers via the
ledgers gateway), the committed manifest, git history (via the proc gateway), and
proxy container status. Owns nothing mutable — it reads on each call — so the only
injected collaborator is the disk-usage cache."""
import json
import statistics
import subprocess

from app import manifest, settings
from app.gateways import ledgers, proc
from app.services import projects

# Cache subdirs holding a ledger.db (apt + apk share "apt"). Used by the stats
# aggregation, which opens each DB once.
_SUBDIRS = ("docker", "npm", "pip", "apt", "git", "files")


def _repo(project):
    """The cache repo dir for a project (global → caches/)."""
    return projects.repo_dir(project)


def _empty_eco(eco):
    return {"eco": eco, "count": 0, "size": 0, "requests": 0,
            "hit_count": 0, "hit_bytes": 0, "miss_count": 0, "miss_bytes": 0}


class Reads:
    """The read side of the control API: live cache contents (from the per-project
    SQLite ledgers), the last-checkpoint manifest, cache-repo git history, and proxy
    container status. Owns nothing mutable — it reads the filesystem on each call —
    so the only injected collaborator is the disk-usage cache."""

    def __init__(self, usage) -> None:
        self._usage = usage

    def manifests(self, project=projects.GLOBAL):
        """Snapshot for /api/manifests: live cache contents (from the ledgers) + how
        many each ecosystem has versioned in the last checkpoint, for THIS project."""
        root = _repo(project)
        committed = self._committed(project)
        return {
            "project": project,
            "ecosystems": {eco: ledgers.ledger_rows(eco, root) for eco in settings.ECOS},
            "checkpointed": {eco: len(committed.get(eco, [])) for eco in settings.ECOS},
            "usage": self._usage.read(),  # disk footprint + deduplicated docker bytes (cached)
            "age": 0.0,  # read live on every request
        }

    def packages(self, params):
        """Server-side filter / sort / paginate for /api/packages — richer than the
        manifest view (origin, arch). params is a parse_qs dict (incl. optional `project`)."""

        def one(key, default=None):
            v = params.get(key)
            return v[0] if v else default

        project = one("project", projects.GLOBAL) or projects.GLOBAL
        root = _repo(project)
        eco = one("eco")
        q = one("q")
        sort = one("sort", "name")
        try:
            page = int(one("page", "1"))
        except ValueError:
            page = 1
        ecos = [eco] if eco in settings.ECOS else settings.ECOS
        return {
            "project": project,
            "ecosystems": {e: ledgers.ledger_rows(e, root, q=q, sort=sort, page=page, full=True) for e in ecos},
            "page": page,
            "sort": sort,
        }

    def stats(self, project=projects.GLOBAL):
        """Aggregate statistics for the stats tab — inventory, per-package request
        leaderboard, hit/miss traffic, and an estimated 'time saved' from passive
        upstream-bandwidth samples. All read-only over the per-eco ledgers."""
        import sqlite3

        root = _repo(project)
        by_eco_map = {eco: _empty_eco(eco) for eco in settings.ECOS}
        leaderboard = {eco: [] for eco in settings.ECOS}
        top_largest, recent_added, samples = [], [], []
        arch_map, bw_by_subdir = {}, {}
        eco_subdir = {eco: sd for eco, (sd, _) in manifest.ECOS.items()}

        for subdir in _SUBDIRS:
            db = root / subdir / "ledger.db"
            conn = ledgers.ro(db)
            if conn is None:
                continue
            try:
                for eco, (sd, ecosystem) in manifest.ECOS.items():
                    if sd != subdir:
                        continue
                    cnt, size = conn.execute(
                        "SELECT COUNT(*), COALESCE(SUM(size),0) FROM artifacts WHERE ecosystem=?",
                        (ecosystem,),
                    ).fetchone()
                    tr = conn.execute(
                        "SELECT hit_count,hit_bytes,miss_count,miss_bytes FROM traffic_stats WHERE ecosystem=?",
                        (ecosystem,),
                    ).fetchone()
                    req = conn.execute(
                        "SELECT COALESCE(SUM(access_count),0) FROM package_stats WHERE ecosystem=?",
                        (ecosystem,),
                    ).fetchone()[0]
                    row = by_eco_map[eco]
                    row.update(count=cnt, size=size, requests=req)
                    if tr:
                        row.update(hit_count=tr["hit_count"], hit_bytes=tr["hit_bytes"],
                                   miss_count=tr["miss_count"], miss_bytes=tr["miss_bytes"])
                    leaderboard[eco] = [
                        {"name": r["name"], "count": r["access_count"], "last_access": r["last_access"]}
                        for r in conn.execute(
                            "SELECT name,access_count,last_access FROM package_stats "
                            "WHERE ecosystem=? ORDER BY access_count DESC, name LIMIT 10", (ecosystem,))
                    ]
                    for r in conn.execute(
                        "SELECT COALESCE(NULLIF(arch,''),'(none)') a, COUNT(*) c, COALESCE(SUM(size),0) s "
                        "FROM artifacts WHERE ecosystem=? GROUP BY a", (ecosystem,)):
                        m = arch_map.setdefault(r["a"], [0, 0])
                        m[0] += r["c"]
                        m[1] += r["s"]
                    for r in conn.execute(
                        "SELECT name,version,size FROM artifacts WHERE ecosystem=? AND size IS NOT NULL "
                        "ORDER BY size DESC LIMIT 15", (ecosystem,)):
                        top_largest.append({"eco": eco, "name": r["name"], "version": r["version"], "size": r["size"]})
                    for r in conn.execute(
                        "SELECT name,version,size,cached_at FROM artifacts WHERE ecosystem=? "
                        "ORDER BY cached_at DESC LIMIT 15", (ecosystem,)):
                        recent_added.append({"eco": eco, "name": r["name"], "version": r["version"],
                                             "size": r["size"], "cached_at": r["cached_at"]})
                bps = [r["bps"] for r in conn.execute("SELECT bps FROM bandwidth_samples ORDER BY ts DESC LIMIT 500")]
                bw_by_subdir[subdir] = bps
                for r in conn.execute("SELECT ts,bps,source FROM bandwidth_samples ORDER BY ts DESC LIMIT 120"):
                    samples.append({"ts": r["ts"], "bps": r["bps"], "source": r["source"]})
            except sqlite3.Error:
                pass
            finally:
                conn.close()

        all_bps = [b for v in bw_by_subdir.values() for b in v]
        global_bps = statistics.median(all_bps) if all_bps else 0.0
        by_eco = list(by_eco_map.values())
        time_saved = 0.0
        for row in by_eco:
            sd_bps = bw_by_subdir.get(eco_subdir.get(row["eco"]), [])
            bps = statistics.median(sd_bps) if sd_bps else global_bps
            if bps > 0:
                time_saved += row["hit_bytes"] / bps

        hits = sum(r["hit_count"] for r in by_eco)
        misses = sum(r["miss_count"] for r in by_eco)
        top_largest.sort(key=lambda x: x["size"] or 0, reverse=True)
        recent_added.sort(key=lambda x: x["cached_at"] or "", reverse=True)
        samples.sort(key=lambda x: x["ts"])
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
                "samples": samples[-120:],
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
        """Best-effort: which proxy containers are up. Empty if docker is unreachable."""
        for profile in ("online", "offline"):
            try:
                res = subprocess.run(
                    ["docker", "compose", "--profile", profile, "ps", "--format", "json"],
                    cwd=str(settings.ROOT), text=True, capture_output=True, timeout=15,
                )
            except (OSError, subprocess.SubprocessError):
                return {"available": False, "services": []}
            services = []
            for chunk in res.stdout.splitlines():
                chunk = chunk.strip()
                if not chunk:
                    continue
                try:
                    obj = json.loads(chunk)
                except ValueError:
                    continue
                items = obj if isinstance(obj, list) else [obj]
                for it in items:
                    services.append({
                        "name": it.get("Service") or it.get("Name", ""),
                        "state": it.get("State", ""),
                        "status": it.get("Status", ""),
                    })
            if services:
                return {"available": True, "profile": profile, "services": services}
        return {"available": True, "profile": None, "services": []}

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
