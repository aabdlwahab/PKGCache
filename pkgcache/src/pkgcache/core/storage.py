"""On-disk cache: path-safe layout, content-addressed blob store, atomic commits,
and Range-capable serving. World-readable so a checkpoint needs no chmod dance.

Two addressing modes:
  * by relative path  — npm tarballs, pip wheels, apt files (the URL path is the key)
  * content-addressed — OCI blobs/manifests under blobs/sha256/<aa>/<hex>

Every write lands via an atomic temp-in-same-dir → fsync → rename, so a checkpoint
can hash the cache live (no proxy quiesce) and DVC never observes a partial file.
"""
from __future__ import annotations

import os
from pathlib import Path

from starlette.responses import FileResponse, Response

PART_SUFFIX = ".part"


class UnsafePath(ValueError):
    """A derived cache path tried to escape the cache root."""


class Storage:
    def __init__(self, root: Path) -> None:
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)

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
