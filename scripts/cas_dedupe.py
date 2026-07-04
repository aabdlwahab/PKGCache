#!/usr/bin/env python3
"""Backfill the cross-project content store (CAS) over an EXISTING cache tree.

New downloads populate the CAS automatically (see pkgcache/core/storage.py), but
files cached before the CAS existed — or downloaded by different projects — still sit
as independent copies. This walks every project's cache tree, hardlinks each artifact
into `<root>/.cas/sha256/<aa>/<hex>`, and collapses duplicate copies (same content in
another project, or a pre-CAS copy) into that one shared inode. The proxy then serves
and dedups them exactly as if they'd been fetched after the CAS was enabled.

Safe to run against the LIVE cache: every replacement is an atomic temp→rename, so a
concurrent reader keeps serving its open file and never sees a partial one. Only
immutable artifacts are touched — the mutable per-role `ledger.db` (and `.part`
in-flight temps, and the git/DVC internals) are skipped, so nothing that gets
rewritten in place is ever shared.

    scripts/cas_dedupe.py [CACHE_ROOT]   # default: ./caches
    scripts/cas_dedupe.py --dry-run      # report what would change, touch nothing

Matches the layout in pkgcache/core/config.py (_CAS_SUBDIR) and storage.py.
"""
from __future__ import annotations

import hashlib
import os
import sys
from pathlib import Path

CAS_SUBDIR = ".cas"
ROLE_SUBDIRS = ("docker", "npm", "pip", "apt", "files")  # git = bare repos; left alone
# Never share a file that is rewritten in place, or a transient/VC-internal one.
SKIP_NAMES = {"ledger.db", "ledger.db-wal", "ledger.db-shm"}
SKIP_SUFFIXES = (".part",)
_CHUNK = 1 << 20


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def _cache_trees(root: Path):
    """Yield every project's role dirs: the global tree directly under root, plus each
    caches/projects/<name>/ tree."""
    bases = [root]
    projects = root / "projects"
    if projects.is_dir():
        bases += [p for p in sorted(projects.iterdir()) if p.is_dir()]
    for base in bases:
        for sub in ROLE_SUBDIRS:
            d = base / sub
            if d.is_dir():
                yield d


def _artifacts(role_dir: Path):
    for p in role_dir.rglob("*"):
        if p.name in SKIP_NAMES or p.name.endswith(SKIP_SUFFIXES):
            continue
        if p.is_symlink() or not p.is_file():
            continue
        yield p


class Deduper:
    def __init__(self, root: Path, dry_run: bool = False):
        self.root = root
        self.cas = root / CAS_SUBDIR / "sha256"
        self.dry_run = dry_run
        self.scanned = self.published = self.collapsed = self.already = 0
        self.reclaimed = 0
        # Hashes published so far THIS run. In dry-run no CAS entry is created, so this
        # is how a second copy is recognized; in a real run it also spares a stat().
        self._seen: set[str] = set()

    def _cas_path(self, hexd: str) -> Path:
        return self.cas / hexd[:2] / hexd

    def _link_over(self, src: Path, dst: Path) -> None:
        """Atomically make `dst` a hardlink to `src` (replacing dst if present)."""
        tmp = dst.parent / (dst.name + f".casdedupe.{os.getpid()}.tmp")
        if tmp.exists():
            tmp.unlink()
        os.link(src, tmp)
        os.replace(tmp, dst)

    def process(self, f: Path) -> None:
        self.scanned += 1
        try:
            st = f.stat()
        except OSError:
            return
        hexd = _sha256(f)
        cp = self._cas_path(hexd)
        known = cp.exists() or hexd in self._seen

        if not known:
            # First time we see this content: publish it (hardlink f into the CAS).
            if not self.dry_run:
                cp.parent.mkdir(parents=True, exist_ok=True)
                self._link_over(f, cp)
                try:
                    os.chmod(cp, 0o644)
                except OSError:
                    pass
            self._seen.add(hexd)
            self.published += 1
            return

        if cp.exists() and cp.stat().st_ino == st.st_ino:
            self.already += 1          # already the shared inode — nothing to do
            return

        # A separate copy of content the CAS already holds: collapse it to the shared
        # inode. Its blocks are freed (this copy had no other links to keep them).
        if not self.dry_run:
            self._link_over(cp, f)
        self.collapsed += 1
        self.reclaimed += st.st_size

    def run(self) -> None:
        for role_dir in _cache_trees(self.root):
            for f in _artifacts(role_dir):
                self.process(f)


def _fmt(n: int) -> str:
    v = float(n)
    for u in ("B", "KiB", "MiB", "GiB", "TiB"):
        if v < 1024 or u == "TiB":
            return f"{v:.1f} {u}"
        v /= 1024


def main(argv: list[str]) -> int:
    args = [a for a in argv if a != "--dry-run"]
    dry = "--dry-run" in argv
    root = Path(args[0] if args else "caches").resolve()
    if not root.is_dir():
        print(f"no such cache root: {root}", file=sys.stderr)
        return 2

    d = Deduper(root, dry_run=dry)
    print(f"{'[dry-run] ' if dry else ''}deduping artifacts under {root} into {root / CAS_SUBDIR}")
    d.run()
    print(
        f"  scanned {d.scanned} artifacts: {d.published} newly published, "
        f"{d.collapsed} duplicates collapsed, {d.already} already shared\n"
        f"  reclaimed {_fmt(d.reclaimed)}{' (would reclaim)' if dry else ''}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
