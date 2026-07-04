"""SQLite ledger gateway: read-only access to the per-ecosystem ledger.db files the
proxies write. This is the ONE place the backend opens those DBs directly.

It deliberately duplicates a slice of pkgcache's Ledger.query (the backend is
stdlib-only and can't import pkgcache) — keep the sort whitelist and column set in
sync with pkgcache/src/pkgcache/core/ledger.py. Phase 4 will replace this with an
HTTP call to a pkgcache admin endpoint, at which point the duplication goes away."""
import sqlite3

from app import manifest

# Whitelisted sort columns → the ledger column they map to (guards the ORDER BY).
SORT_COLS = {"name": "name", "size": "size", "date": "cached_at", "version": "version"}


def ro(db):
    """Open a ledger.db read-only over WAL, or None if it doesn't exist / can't open."""
    if not db.exists():
        return None
    try:
        conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
        return conn
    except sqlite3.Error:
        return None


def ledger_rows(eco, root, q=None, sort="name", page=1, page_size=1000, full=False):
    """Artifacts for one ecosystem from its ledger.db (under `root`, a project's cache
    repo), read-only over WAL.

    cached_at is included even in the compact (non-full) view so the UI can sort the
    live package list by date without a second, heavier query."""
    subdir, ecosystem = manifest.ECOS[eco]
    db = root / subdir / "ledger.db"
    if not db.exists():
        return []
    cols = (
        "name, version, digest, size, origin, arch, cached_at"
        if full
        else "name, version, digest, size, cached_at"
    )
    clauses, args = ["ecosystem = ?"], [ecosystem]
    if q:
        clauses.append("name LIKE ?")
        args.append(f"%{q}%")
    sort_col = SORT_COLS.get(sort, "name")
    sql = (
        f"SELECT {cols} FROM artifacts WHERE {' AND '.join(clauses)} "
        f"ORDER BY {sort_col}, name, version"
    )
    if full:
        sql += " LIMIT ? OFFSET ?"
        args += [page_size, (max(1, page) - 1) * page_size]
    conn = ro(db)
    if conn is None:
        return []
    try:
        return [dict(r) for r in conn.execute(sql, args).fetchall()]
    except sqlite3.Error:
        return []
    finally:
        conn.close()
