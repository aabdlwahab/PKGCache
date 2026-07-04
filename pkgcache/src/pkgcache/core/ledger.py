"""The native manifest: a per-ecosystem SQLite ledger written at cache-commit time.

Each proxy is the single writer of its own caches/<eco>/ledger.db. The webui reads
these files read-only (over WAL) for the live /api/packages view, and
gen_manifest.py exports a deterministic subset for the git-committed manifest.

sqlite3 is blocking, so async callers must go through the async wrappers
(arecord/aquery/...), which hop to a thread. Writes are additionally serialized by
an internal lock so the single writer never trips SQLITE_BUSY against itself.
"""
from __future__ import annotations

import asyncio
import json
import sqlite3
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path

SCHEMA_VERSION = 3
# Wait this long for a competing writer before raising SQLITE_BUSY. Generous: the
# single writer only contends with its own thread-hopped writes and the WAL readers.
_BUSY_TIMEOUT_MS = 5000
# Cap the rolling bandwidth-sample log per ledger (passive miss throughput + active
# speed-test points). Plenty for an over-time chart; pruned on every flush.
_BANDWIDTH_KEEP = 2000

_SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);

CREATE TABLE IF NOT EXISTS artifacts (
  id         INTEGER PRIMARY KEY,
  ecosystem  TEXT NOT NULL,
  name       TEXT NOT NULL,
  version    TEXT NOT NULL,
  digest     TEXT,
  size       INTEGER,
  origin     TEXT,
  path       TEXT,
  arch       TEXT,
  cached_at  TEXT NOT NULL,
  extra      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_artifact ON artifacts(ecosystem, name, version, digest);
CREATE INDEX IF NOT EXISTS ix_name ON artifacts(name);

CREATE TABLE IF NOT EXISTS oci_tags (
  upstream   TEXT NOT NULL,
  repo       TEXT NOT NULL,
  tag        TEXT NOT NULL,
  digest     TEXT NOT NULL,
  media_type TEXT,
  fetched_at TEXT,
  PRIMARY KEY (upstream, repo, tag)
);

-- Per-package request tally (leaderboard + future LRU eviction). One row per
-- (ecosystem, package); access_count is cumulative, last_access is epoch seconds.
CREATE TABLE IF NOT EXISTS package_stats (
  ecosystem    TEXT NOT NULL,
  name         TEXT NOT NULL,
  access_count INTEGER NOT NULL DEFAULT 0,
  last_access  REAL,
  PRIMARY KEY (ecosystem, name)
);

-- Cumulative hit/miss tallies per ecosystem → hit rate + bytes-saved.
CREATE TABLE IF NOT EXISTS traffic_stats (
  ecosystem  TEXT PRIMARY KEY,
  hit_count  INTEGER NOT NULL DEFAULT 0,
  hit_bytes  INTEGER NOT NULL DEFAULT 0,
  miss_count INTEGER NOT NULL DEFAULT 0,
  miss_bytes INTEGER NOT NULL DEFAULT 0
);

-- Rolling upstream-throughput samples: passive (measured from real cache-miss
-- downloads) + active (scheduled speed tests). Feeds the "time saved" estimate.
CREATE TABLE IF NOT EXISTS bandwidth_samples (
  ts     REAL NOT NULL,
  bps    REAL NOT NULL,
  source TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_bw_ts ON bandwidth_samples(ts);

-- git ref → commit map (the oci_tags analog): lets the offline side report which
-- branches/tags a mirror holds. repo = "<host>/<owner>/<name>".
CREATE TABLE IF NOT EXISTS git_refs (
  repo       TEXT NOT NULL,
  ref        TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  fetched_at TEXT,
  PRIMARY KEY (repo, ref)
);
"""


@dataclass
class ArtifactRecord:
    ecosystem: str
    name: str
    version: str
    digest: str | None = None
    size: int | None = None
    origin: str | None = None
    path: str | None = None
    arch: str | None = None
    extra: dict | None = field(default=None)


class Ledger:
    def __init__(self, db_path: Path) -> None:
        self.db_path = Path(db_path)
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._conn = sqlite3.connect(
            self.db_path, check_same_thread=False, isolation_level=None
        )
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=NORMAL")
        self._conn.execute(f"PRAGMA busy_timeout={_BUSY_TIMEOUT_MS}")
        with self._lock:
            self._conn.executescript(_SCHEMA)
            self._conn.execute(
                "INSERT OR IGNORE INTO meta(key, value) VALUES('schema_version', ?)",
                (str(SCHEMA_VERSION),),
            )

    # ---- sync core (run under the lock; callers hop a thread via the a* wrappers)
    def record(self, rec: ArtifactRecord) -> None:
        with self._lock:
            self._conn.execute(
                """INSERT INTO artifacts
                     (ecosystem, name, version, digest, size, origin, path, arch, cached_at, extra)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(ecosystem, name, version, digest) DO UPDATE SET
                     size=excluded.size, origin=excluded.origin, path=excluded.path,
                     arch=excluded.arch""",
                (
                    rec.ecosystem, rec.name, rec.version, rec.digest, rec.size,
                    rec.origin, rec.path, rec.arch, _now_iso(),
                    json.dumps(rec.extra) if rec.extra else None,
                ),
            )

    def delete_artifact(self, ecosystem: str, name: str) -> None:
        """Remove every row for one (ecosystem, name) — used by the files role to make
        an overwrite/delete replace rather than accumulate rows (record() upserts on
        the full identity incl. digest, so a changed-content re-upload would otherwise
        leave a stale row)."""
        with self._lock:
            self._conn.execute(
                "DELETE FROM artifacts WHERE ecosystem=? AND name=?", (ecosystem, name))

    async def adelete_artifact(self, ecosystem: str, name: str) -> None:
        await asyncio.to_thread(self.delete_artifact, ecosystem, name)

    def set_tag(self, upstream: str, repo: str, tag: str, digest: str, media_type: str | None) -> None:
        with self._lock:
            self._conn.execute(
                """INSERT INTO oci_tags(upstream, repo, tag, digest, media_type, fetched_at)
                   VALUES (?, ?, ?, ?, ?, ?)
                   ON CONFLICT(upstream, repo, tag) DO UPDATE SET
                     digest=excluded.digest, media_type=excluded.media_type,
                     fetched_at=excluded.fetched_at""",
                (upstream, repo, tag, digest, media_type, _now_iso()),
            )

    def get_tag(self, upstream: str, repo: str, tag: str) -> sqlite3.Row | None:
        with self._lock:
            cur = self._conn.execute(
                "SELECT digest, media_type FROM oci_tags WHERE upstream=? AND repo=? AND tag=?",
                (upstream, repo, tag),
            )
            return cur.fetchone()

    def list_tags(self, upstream: str, repo: str) -> list[str]:
        with self._lock:
            cur = self._conn.execute(
                "SELECT tag FROM oci_tags WHERE upstream=? AND repo=? ORDER BY tag",
                (upstream, repo),
            )
            return [r["tag"] for r in cur.fetchall()]

    def sync_git_refs(self, repo: str, entries: list[tuple[str, str]],
                      head_ref: str | None, mirror_size: int | None) -> None:
        """Replace the ledger view of one git mirror after a clone/fetch.

        entries: [(ref_shortname, commit_sha), …] for heads + tags. head_ref is the
        default-branch shortname (carries the mirror's on-disk size on its artifacts
        row so the git eco's total size is the sum of mirror sizes, counted once).
        Wholesale replace (delete-then-insert) handles pruned refs for free."""
        now = _now_iso()
        with self._lock:
            self._conn.execute("BEGIN")
            try:
                self._conn.execute("DELETE FROM git_refs WHERE repo=?", (repo,))
                self._conn.execute(
                    "DELETE FROM artifacts WHERE ecosystem='git' AND name=?", (repo,))
                self._conn.executemany(
                    "INSERT INTO git_refs(repo, ref, commit_sha, fetched_at) VALUES (?,?,?,?)",
                    [(repo, ref, sha, now) for ref, sha in entries],
                )
                self._conn.executemany(
                    """INSERT INTO artifacts
                         (ecosystem, name, version, digest, size, origin, path, arch, cached_at, extra)
                       VALUES ('git', ?, ?, ?, ?, NULL, NULL, NULL, ?, NULL)
                       ON CONFLICT(ecosystem, name, version, digest) DO UPDATE SET size=excluded.size""",
                    [
                        (repo, ref, sha, (mirror_size if ref == head_ref else None), now)
                        for ref, sha in entries
                    ],
                )
                self._conn.execute("COMMIT")
            except Exception:
                self._conn.execute("ROLLBACK")
                raise

    async def async_sync_git_refs(self, *a) -> None:
        await asyncio.to_thread(self.sync_git_refs, *a)

    def query(self, ecosystem: str | None = None, q: str | None = None,
              sort: str = "name", page: int = 1, page_size: int = 200) -> list[dict]:
        # The control UI reaches this over HTTP (GET /+ledger/artifacts, served by
        # app.py) via the webui pkgcache gateway — it no longer opens ledger.db
        # itself, so this is the single implementation. page_size<=0 returns ALL rows
        # (the manifest view wants the full inventory, not a page).
        sort_col = {"name": "name", "size": "size", "date": "cached_at",
                    "version": "version"}.get(sort, "name")
        clauses, args = [], []
        if ecosystem:
            clauses.append("ecosystem = ?")
            args.append(ecosystem)
        if q:
            clauses.append("name LIKE ?")
            args.append(f"%{q}%")
        where = (" WHERE " + " AND ".join(clauses)) if clauses else ""
        page = max(1, page)
        limit = page_size if page_size > 0 else -1          # -1 → no limit (all rows)
        offset = (page - 1) * page_size if page_size > 0 else 0
        args += [limit, offset]
        with self._lock:
            cur = self._conn.execute(
                f"SELECT ecosystem, name, version, digest, size, origin, arch, cached_at "
                f"FROM artifacts{where} ORDER BY {sort_col}, name, version LIMIT ? OFFSET ?",
                args,
            )
            return [dict(r) for r in cur.fetchall()]

    def stats(self) -> dict:
        """Per-ecosystem usage aggregates for THIS ledger, plus this ledger's
        bandwidth samples. The control UI (webui reads service) fetches one of these
        per role over HTTP and combines them across roles into the /api/stats view —
        so the aggregation SQL lives here, next to the schema it reads, instead of
        being reimplemented over the raw file. apt's ledger holds two ecosystems
        (apt + apk); they come back as separate by_eco/leaderboard entries."""
        with self._lock:
            c = self._conn
            by_eco: dict[str, dict] = {}
            for r in c.execute(
                "SELECT ecosystem, COUNT(*) n, COALESCE(SUM(size),0) sz "
                "FROM artifacts GROUP BY ecosystem"
            ):
                by_eco.setdefault(r["ecosystem"], {}).update(count=r["n"], size=r["sz"])
            for r in c.execute(
                "SELECT ecosystem, hit_count, hit_bytes, miss_count, miss_bytes FROM traffic_stats"
            ):
                by_eco.setdefault(r["ecosystem"], {}).update(
                    hit_count=r["hit_count"], hit_bytes=r["hit_bytes"],
                    miss_count=r["miss_count"], miss_bytes=r["miss_bytes"])
            for r in c.execute(
                "SELECT ecosystem, COALESCE(SUM(access_count),0) req "
                "FROM package_stats GROUP BY ecosystem"
            ):
                by_eco.setdefault(r["ecosystem"], {}).update(requests=r["req"])
            leaderboard = {
                eco: [
                    {"name": r["name"], "count": r["access_count"], "last_access": r["last_access"]}
                    for r in c.execute(
                        "SELECT name, access_count, last_access FROM package_stats "
                        "WHERE ecosystem=? ORDER BY access_count DESC, name LIMIT 10", (eco,))
                ]
                for eco in by_eco
            }
            arch = [
                {"arch": r["a"], "count": r["c"], "size": r["s"]}
                for r in c.execute(
                    "SELECT COALESCE(NULLIF(arch,''),'(none)') a, COUNT(*) c, "
                    "COALESCE(SUM(size),0) s FROM artifacts GROUP BY a")
            ]
            top_largest = [
                {"eco": r["ecosystem"], "name": r["name"], "version": r["version"], "size": r["size"]}
                for r in c.execute(
                    "SELECT ecosystem, name, version, size FROM artifacts "
                    "WHERE size IS NOT NULL ORDER BY size DESC LIMIT 15")
            ]
            recent_added = [
                {"eco": r["ecosystem"], "name": r["name"], "version": r["version"],
                 "size": r["size"], "cached_at": r["cached_at"]}
                for r in c.execute(
                    "SELECT ecosystem, name, version, size, cached_at FROM artifacts "
                    "ORDER BY cached_at DESC LIMIT 15")
            ]
            bandwidth = [r["bps"] for r in c.execute(
                "SELECT bps FROM bandwidth_samples ORDER BY ts DESC LIMIT 500")]
            points = [
                {"ts": r["ts"], "bps": r["bps"], "source": r["source"]}
                for r in c.execute(
                    "SELECT ts, bps, source FROM bandwidth_samples ORDER BY ts DESC LIMIT 120")
            ]
        return {
            "by_eco": by_eco, "leaderboard": leaderboard, "arch": arch,
            "top_largest": top_largest, "recent_added": recent_added,
            "bandwidth": bandwidth, "bandwidth_points": points,
        }

    def export(self, ecosystem: str) -> list[dict]:
        """Deterministic git-snapshot subset (volatile fields dropped)."""
        with self._lock:
            cur = self._conn.execute(
                "SELECT ecosystem, name, version, digest, size FROM artifacts "
                "WHERE ecosystem=? ORDER BY name, version, digest",
                (ecosystem,),
            )
            return [dict(r) for r in cur.fetchall()]

    def apply_stats(self, access: dict, traffic: dict, bandwidth: list) -> None:
        """Fold one flush window's in-memory deltas into the persistent tallies.

        access:    {(ecosystem, name): (count_delta, last_access_epoch)}
        traffic:   {ecosystem: (hit_count, hit_bytes, miss_count, miss_bytes)} deltas
        bandwidth: [(ts, bps, source), …] samples to append
        All applied in one transaction; bandwidth log is pruned to the last N rows."""
        if not (access or traffic or bandwidth):
            return
        with self._lock:
            self._conn.execute("BEGIN")
            try:
                for (eco, name), (cnt, last_ts) in access.items():
                    self._conn.execute(
                        """INSERT INTO package_stats(ecosystem, name, access_count, last_access)
                           VALUES (?, ?, ?, ?)
                           ON CONFLICT(ecosystem, name) DO UPDATE SET
                             access_count = access_count + excluded.access_count,
                             last_access  = excluded.last_access""",
                        (eco, name, cnt, last_ts),
                    )
                for eco, (hc, hb, mc, mb) in traffic.items():
                    self._conn.execute(
                        """INSERT INTO traffic_stats(ecosystem, hit_count, hit_bytes, miss_count, miss_bytes)
                           VALUES (?, ?, ?, ?, ?)
                           ON CONFLICT(ecosystem) DO UPDATE SET
                             hit_count  = hit_count  + excluded.hit_count,
                             hit_bytes  = hit_bytes  + excluded.hit_bytes,
                             miss_count = miss_count + excluded.miss_count,
                             miss_bytes = miss_bytes + excluded.miss_bytes""",
                        (eco, hc, hb, mc, mb),
                    )
                if bandwidth:
                    self._conn.executemany(
                        "INSERT INTO bandwidth_samples(ts, bps, source) VALUES (?, ?, ?)", bandwidth
                    )
                    self._conn.execute(
                        "DELETE FROM bandwidth_samples WHERE rowid NOT IN "
                        "(SELECT rowid FROM bandwidth_samples ORDER BY ts DESC LIMIT ?)",
                        (_BANDWIDTH_KEEP,),
                    )
                self._conn.execute("COMMIT")
            except Exception:
                self._conn.execute("ROLLBACK")
                raise

    def clear(self) -> None:
        with self._lock:
            self._conn.execute("DELETE FROM artifacts")

    def checkpoint(self) -> None:
        """Fold the WAL into the main db so the on-disk ledger.db is self-contained.

        Not required for checkpoints (those run live and DVC captures ledger.db
        together with its -wal, which SQLite recovers on open); used on close()."""
        with self._lock:
            self._conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")

    def close(self) -> None:
        with self._lock:
            self._conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            self._conn.close()

    # ---- async wrappers (keep sqlite off the event loop) --------------------
    async def arecord(self, rec: ArtifactRecord) -> None:
        await asyncio.to_thread(self.record, rec)

    async def aset_tag(self, *a) -> None:
        await asyncio.to_thread(self.set_tag, *a)

    async def aget_tag(self, *a):
        return await asyncio.to_thread(self.get_tag, *a)

    async def alist_tags(self, *a):
        return await asyncio.to_thread(self.list_tags, *a)

    async def aquery(self, **kw):
        return await asyncio.to_thread(lambda: self.query(**kw))

    async def astats(self):
        return await asyncio.to_thread(self.stats)


def _now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
