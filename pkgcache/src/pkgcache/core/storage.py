"""On-disk cache: path-safe layout, content-addressed blob store, atomic commits,
and Range-capable serving. World-readable so a checkpoint needs no chmod dance.

Two addressing modes:
  * by relative path  — npm tarballs, pip wheels, apt files (the URL path is the key)
  * content-addressed — OCI blobs/manifests under blobs/sha256/<aa>/<hex>

Every write lands via an atomic temp-in-same-dir → fsync → rename, so a checkpoint
can hash the cache live (no proxy quiesce) and DVC never observes a partial file.
"""
from __future__ import annotations

import contextlib
import os
from pathlib import Path

from starlette.responses import FileResponse, Response

PART_SUFFIX = ".part"


class UnsafePath(ValueError):
    """A derived cache path tried to escape the cache root."""


class Storage:
    def __init__(self, root: Path, cas_root: Path | None = None) -> None:
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        # Optional cross-project content-addressed store (keyed by sha256). One CAS is
        # shared by every project+role Core, so an artifact one project downloads is
        # neither re-downloaded nor re-stored for the next: a committed file is
        # hardlinked into the CAS, and a miss whose sha256 is known up front (pypi
        # index hashes, OCI digests) is served by hardlinking the CAS entry into this
        # project's tree. Hardlinks are safe because cached artifacts are immutable —
        # nothing rewrites a committed file in place (overwrites rename a fresh inode
        # over the path, leaving the shared inode untouched).
        self.cas_root: Path | None = None
        if cas_root is not None:
            cr = Path(cas_root).resolve()
            cr.mkdir(parents=True, exist_ok=True)
            # Hardlinks can't cross filesystems, and dedup is the whole point (a copy
            # fallback would defeat it), so require the CAS to share the cache's fs;
            # otherwise disable it rather than silently copy.
            if cr.stat().st_dev == self.root.stat().st_dev:
                self.cas_root = cr

    # ---- path safety ---------------------------------------------------------
    def safe_path(self, *parts: str) -> Path:
        """Join under the cache root, rejecting traversal/absolute escapes.

        Names come from untrusted URLs (repo names, filenames, apt targets), so a
        crafted '..' must never write outside the root.
        """
        rel = Path(*[p.strip("/") for p in parts if p not in ("", None)])
        if rel.is_absolute():
            raise UnsafePath(str(rel))
        full = (self.root / rel).resolve()
        if full != self.root and self.root not in full.parents:
            raise UnsafePath(str(rel))
        return full

    def blob_path(self, digest: str) -> Path:
        """Content-addressed location for an OCI blob/manifest digest.

        digest is 'sha256:<hex>' or a bare hex string.
        """
        algo, _, hexd = digest.partition(":")
        if not hexd:
            algo, hexd = "sha256", algo
        if not (len(hexd) >= 4 and all(c in "0123456789abcdef" for c in hexd.lower())):
            raise UnsafePath(digest)
        hexd = hexd.lower()
        return self.safe_path("blobs", algo, hexd[:2], hexd)

    # ---- atomic write helpers ------------------------------------------------
    @staticmethod
    def open_part(final: Path) -> tuple[Path, "os.PathLike"]:
        """Return (tmp_path, open binary file) for a sibling .part of `final`."""
        final.parent.mkdir(parents=True, exist_ok=True)
        tmp = final.parent / (final.name + f".{os.getpid()}.{id(final):x}{PART_SUFFIX}")
        return tmp, open(tmp, "wb")

    @staticmethod
    def commit_part(tmp: Path, final: Path) -> None:
        """fsync the temp file and its dir, then atomically rename into place."""
        fd = os.open(tmp, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(tmp, final)
        try:
            os.chmod(final, 0o644)
        except OSError:
            pass
        dfd = os.open(final.parent, os.O_RDONLY)
        try:
            os.fsync(dfd)
        finally:
            os.close(dfd)

    def gc_parts(self) -> int:
        """Delete orphaned .part files left by an interrupted download. Run at startup."""
        n = 0
        for p in self.root.rglob(f"*{PART_SUFFIX}"):
            try:
                p.unlink()
                n += 1
            except OSError:
                pass
        return n

    # ---- content-addressed store (cross-project dedup) -----------------------
    def cas_path(self, sha256_hex: str) -> Path | None:
        """The CAS location for a sha256 digest, or None if the CAS is disabled or the
        digest is malformed (too short / non-hex → never trust it as a path)."""
        if self.cas_root is None:
            return None
        hexd = (sha256_hex or "").lower()
        if not (len(hexd) >= 4 and all(c in "0123456789abcdef" for c in hexd)):
            return None
        return self.cas_root / "sha256" / hexd[:2] / hexd

    def cas_link_from(self, final_path: Path, sha256_hex: str) -> None:
        """Publish a just-committed file into the CAS by hardlink, so another project
        fetching the same content later links it instead of downloading it again.

        Best-effort: a CAS hiccup must never fail the download that triggered it. Idem-
        potent (an existing entry is left as-is — same content by construction)."""
        cp = self.cas_path(sha256_hex)
        if cp is None or cp.exists():
            return
        try:
            cp.parent.mkdir(parents=True, exist_ok=True)
            tmp = cp.parent / (cp.name + f".{os.getpid()}.{id(final_path):x}{PART_SUFFIX}")
            os.link(final_path, tmp)          # hardlink the committed inode
            os.replace(tmp, cp)               # atomic publish (last writer wins; same bytes)
        except OSError:
            with contextlib.suppress(OSError):
                tmp.unlink()

    def cas_materialize(self, sha256_hex: str, final_path: Path) -> bool:
        """If the CAS holds this content, hardlink it into place at `final_path` and
        return True (served without a download); else return False.

        The link lands atomically (link to a sibling temp, then rename), so a reader
        never sees a partial file and a concurrent committer of the same key is a
        harmless overwrite with identical bytes."""
        cp = self.cas_path(sha256_hex)
        if cp is None or not cp.exists():
            return False
        try:
            final_path.parent.mkdir(parents=True, exist_ok=True)
            tmp = final_path.parent / (final_path.name + f".{os.getpid()}.{id(final_path):x}{PART_SUFFIX}")
            os.link(cp, tmp)
            os.replace(tmp, final_path)
            try:
                os.chmod(final_path, 0o644)
            except OSError:
                pass
            return True
        except OSError:
            with contextlib.suppress(OSError):
                tmp.unlink()
            return False

    # ---- serving -------------------------------------------------------------
    @staticmethod
    def file_response(path: Path, *, media_type: str | None = None,
                      headers: dict | None = None, method: str = "GET") -> Response:
        """Serve a cached file. FileResponse handles Range (206 / Content-Range /
        Accept-Ranges) and conditional requests for us — this is what fixes the
        devpi 'no Range → re-download huge wheels' defect."""
        if method == "HEAD":
            size = path.stat().st_size
            h = {"Content-Length": str(size), "Accept-Ranges": "bytes"}
            if headers:
                h.update(headers)
            return Response(status_code=200, media_type=media_type, headers=h)
        return FileResponse(path, media_type=media_type, headers=headers or {})
