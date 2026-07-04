#!/usr/bin/env python3
"""Export the cross-ecosystem manifest from the proxies' native SQLite ledgers.

The proxies (pkgcache) record every cached artifact into caches/<eco>/ledger.db at
commit time, so there's no filesystem walking here anymore — this just reads each
ledger and emits a deterministic, git-diffable subset into caches/manifests/<eco>.json.

Stdlib-only (sqlite3), so it runs even while a checkpoint has the proxies stopped.

    gen_manifest.py            # export manifests/*.json from the ledgers
    gen_manifest.py --rebuild  # first repopulate each ledger from disk (repair),
                               #   then export. Needs the pkgcache package importable.

The five logical ecosystems map onto four DB files (apt + apk share one):
"""
import json
import os
import pathlib
import sqlite3
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
# The cache repo whose ledgers we export. Defaults to the global cache repo
# (caches/); a per-project checkpoint sets PKGCACHE_MANIFEST_ROOT to that project's
# repo (caches/projects/<name>/) so its manifest is generated from ITS ledgers.
CACHES = pathlib.Path(os.environ.get("PKGCACHE_MANIFEST_ROOT") or (ROOT / "caches"))
# Manifests live inside the cache repo (caches/manifests/), not the code repo, so
# the committed inventory ships and rolls back atomically with the .dvc pointers.
MANIFESTS = CACHES / "manifests"

# eco -> (cache subdir holding ledger.db, ecosystem value to filter on)
ECOS = {
    "docker": ("docker", "docker"),
    "npm": ("npm", "npm"),
    "pip": ("pip", "pip"),
    "apt": ("apt", "apt"),
    "apk": ("apt", "apk"),
    "git": ("git", "git"),
    "files": ("files", "files"),
}


def export_one(subdir: str, ecosystem: str) -> list[dict]:
    db = CACHES / subdir / "ledger.db"
    if not db.exists():
        return []
    conn = sqlite3.connect(f"file:{db}?mode=ro&immutable=0", uri=True)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute(
            "SELECT name, version, digest, size FROM artifacts "
            "WHERE ecosystem=? ORDER BY name, version, digest",
            (ecosystem,),
        ).fetchall()
    finally:
        conn.close()
    return [dict(r) for r in rows]


def rebuild() -> None:
    """Repopulate each ledger from the on-disk cache (the retired walkers' last
    home). Requires the pkgcache package on PYTHONPATH."""
    try:
        from pkgcache.core.config import Config
        from pkgcache.core import build_core
        from pkgcache.repositories import REPOSITORIES
    except ImportError:
        sys.exit("--rebuild needs the pkgcache package importable (pip install ./pkgcache)")

    for role, repo_cls in REPOSITORIES.items():
        subdir = {"oci": "docker", "npm": "npm", "pypi": "pip", "apt": "apt", "git": "git", "files": "files"}[role]
        cache_dir = CACHES / subdir
        if not cache_dir.exists():
            continue
        cfg = Config(role=role, offline=True, project="global", cache_root=cache_dir,
                     host="127.0.0.1", port=0, request_timeout=1)
        core = build_core(cfg)
        core.ledger.clear()
        repo = repo_cls()
        n = 0
        for rec in repo.rebuild_ledger(cache_dir):
            core.ledger.record(rec)
            n += 1
        core.ledger.close()
        print(f"rebuilt {role:6} {n} rows from {cache_dir}")


def main(argv: list[str]) -> int:
    if "--rebuild" in argv:
        rebuild()
    MANIFESTS.mkdir(exist_ok=True)
    for eco, (subdir, ecosystem) in ECOS.items():
        items = export_one(subdir, ecosystem)
        (MANIFESTS / f"{eco}.json").write_text(
            json.dumps(items, indent=2, sort_keys=True) + "\n"
        )
        print(f"{eco:8} {len(items)} items")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
