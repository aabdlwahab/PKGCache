"""Cache disk-usage scan, TTL-cached.

The console shows two totals side by side:
  * the logical *package* total — sum of cached artifact payloads, and for docker
    the DEDUPLICATED bytes (shared layers counted once, since the CAS stores each
    blob once even when many images reference it); and
  * the actual *on-disk* footprint — every byte under caches/<eco>, including the
    index/metadata files the proxies keep that are not artifacts (npm packuments,
    pip simple indices, apt Release/Packages, the ledger DBs, …).

Walking the tree is cheap (st only) but not free, so results are cached briefly.
"""
import os
import shutil
import threading
import time

import config  # noqa: F401 -- ensures scripts/ is on sys.path for gen_manifest
import gen_manifest

# eco cache subdirs to measure. apt + apk share the "apt" subdir, so we measure
# by subdir (four), not by ecosystem (five).
_SUBDIRS = ("docker", "npm", "pip", "apt")
_TTL = 20.0
_cache = {"data": None, "ts": 0.0}
_lock = threading.Lock()


def _tree_bytes(path) -> int:
    """Sum apparent file sizes under `path` (recursive, symlink-safe, tolerant of
    unreadable entries). Close to `du --apparent-size` (block rounding aside)."""
    total = 0
    stack = [str(path)]
    while stack:
        try:
            with os.scandir(stack.pop()) as it:
                for e in it:
                    try:
                        if e.is_dir(follow_symlinks=False):
                            stack.append(e.path)
                        elif e.is_file(follow_symlinks=False):
                            total += e.stat(follow_symlinks=False).st_size
                    except OSError:
                        pass
        except OSError:
            pass
    return total


def _compute() -> dict:
    caches = gen_manifest.CACHES
    disk = {sub: (_tree_bytes(caches / sub) if (caches / sub).is_dir() else 0) for sub in _SUBDIRS}
    blobs = caches / "docker" / "blobs"
    return {
        "disk": disk,
        "disk_total": sum(disk.values()),
        # The CAS holds each docker blob once → its byte total is the deduplicated
        # docker size, which the grand package total uses instead of the per-image sum.
        "docker_deduped": _tree_bytes(blobs) if blobs.is_dir() else 0,
    }


def _fs_stats() -> dict | None:
    """Filesystem capacity for the volume holding the cache: total/used/free bytes.
    Cheap (one statvfs), so it's read fresh on every call — free space is the whole
    point of the storage monitor and changes independently of our walk."""
    try:
        total, used, free = shutil.disk_usage(str(gen_manifest.CACHES))
        return {"total": total, "used": used, "free": free}
    except OSError:
        return None


def disk_usage() -> dict:
    now = time.time()
    with _lock:
        cached = _cache["data"] if (_cache["data"] is not None and now - _cache["ts"] < _TTL) else None
    if cached is None:
        cached = _compute()  # walk outside the lock
        with _lock:
            _cache["data"] = cached
            _cache["ts"] = time.time()
    return {**cached, "fs": _fs_stats()}
