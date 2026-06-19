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

SCHEMA_VERSION = 1

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
        self._conn.execute("PRAGMA busy_timeout=5000")
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

    def query(self, ecosystem: str | None = None, q: str | None = None,
              sort: str = "name", page: int = 1, page_size: int = 200) -> list[dict]:
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
        args += [page_size, (page - 1) * page_size]
        with self._lock:
            cur = self._conn.execute(
                f"SELECT ecosystem, name, version, digest, size, origin, arch, cached_at "
                f"FROM artifacts{where} ORDER BY {sort_col}, name, version LIMIT ? OFFSET ?",
                args,
            )
            return [dict(r) for r in cur.fetchall()]

    def export(self, ecosystem: str) -> list[dict]:
        """Deterministic git-snapshot subset (volatile fields dropped)."""
        with self._lock:
            cur = self._conn.execute(
                "SELECT ecosystem, name, version, digest, size FROM artifacts "
                "WHERE ecosystem=? ORDER BY name, version, digest",
                (ecosystem,),
            )
            return [dict(r) for r in cur.fetchall()]

    def clear(self) -> None:
        with self._lock:
            self._conn.execute("DELETE FROM artifacts")

    def checkpoint(self) -> None:
        """Fold the WAL into the main db so a quiesced snapshot is self-contained."""
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


def _now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
