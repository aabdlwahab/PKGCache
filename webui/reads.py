"""Read-only data for the API: cache contents (live, from the SQLite ledgers),
the committed manifest, git history, and proxy container status."""
import json
import os
import sqlite3
import subprocess

import gen_manifest
import projects

from config import ECOS, GIT_ENV, ROOT


def _repo(project):
    """The cache repo dir for a project (global → caches/)."""
    return projects.repo_dir(project)


# The proxies record every cached artifact into caches/<eco>/ledger.db, so the live
# view is a cheap read-only query against those DBs — no walking, no re-hashing.
# This duplicates pkgcache Ledger.query (pkgcache/src/pkgcache/core/ledger.py): the
# UI is deliberately stdlib-only and can't import pkgcache, so it reads the same
# ledger.db files directly. Keep this sort whitelist + the column set in sync there.
_SORT_COLS = {"name": "name", "size": "size", "date": "cached_at", "version": "version"}


def _ledger_rows(eco, root, q=None, sort="name", page=1, page_size=1000, full=False):
    """Read artifacts for one ecosystem from its ledger.db (under `root`, this
    project's cache repo), read-only over WAL.

    cached_at is included even in the compact (non-full) view so the UI can sort
    the live package list by date without a second, heavier query."""
    subdir, ecosystem = gen_manifest.ECOS[eco]
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
    sort_col = _SORT_COLS.get(sort, "name")
    sql = (
        f"SELECT {cols} FROM artifacts WHERE {' AND '.join(clauses)} "
        f"ORDER BY {sort_col}, name, version"
    )
    if full:
        sql += " LIMIT ? OFFSET ?"
        args += [page_size, (max(1, page) - 1) * page_size]
    try:
        conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        conn.row_factory = sqlite3.Row
        try:
            return [dict(r) for r in conn.execute(sql, args).fetchall()]
        finally:
            conn.close()
    except sqlite3.Error:
        return []


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
            "ecosystems": {eco: _ledger_rows(eco, root) for eco in ECOS},
            "checkpointed": {eco: len(committed.get(eco, [])) for eco in ECOS},
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
        ecos = [eco] if eco in ECOS else ECOS
        return {
            "project": project,
            "ecosystems": {e: _ledger_rows(e, root, q=q, sort=sort, page=page, full=True) for e in ecos},
            "page": page,
            "sort": sort,
        }

    def history(self, project=projects.GLOBAL):
        """Recent commits; checkpoints are the ones whose subject starts 'checkpoint:'."""
        # History = this project's cache repo checkpoint log, NOT the code repo. Until
        # the first checkpoint creates its .git, there is no cache repo — return empty
        # instead of letting git walk UP into the parent code repo's history.
        repo = _repo(project)
        if not (repo / ".git").is_dir():
            return {"head": "", "commits": []}
        git_env = {**os.environ, **GIT_ENV}
        try:
            head = subprocess.run(
                ["git", "rev-parse", "--verify", "HEAD"], cwd=str(repo), text=True,
                capture_output=True, timeout=10, env=git_env,
            ).stdout.strip()
            raw = subprocess.run(
                ["git", "log", "-50", "--pretty=format:%H%x1f%h%x1f%ad%x1f%s", "--date=short"],
                cwd=str(repo), text=True, capture_output=True, timeout=10, env=git_env,
            ).stdout
        except (OSError, subprocess.SubprocessError):
            return {"head": "", "commits": []}
        commits = []
        for line in raw.splitlines():
            parts = line.split("\x1f")
            if len(parts) != 4:
                continue
            full, short, date, subject = parts
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
                    cwd=str(ROOT), text=True, capture_output=True, timeout=15,
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
        for eco in ECOS:
            path = manifests / f"{eco}.json"
            try:
                out[eco] = json.loads(path.read_text())
            except (OSError, ValueError):
                out[eco] = []
        return out
