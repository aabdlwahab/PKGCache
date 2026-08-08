"""Atomic persistence for JSON files that contain credentials or credential hashes."""
import contextlib
import json
import os
import pathlib
import tempfile

_PRIVATE_MODE = 0o600


def save(path, data):
    """Write JSON to a private sibling temp file, then atomically replace `path`."""
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(data, indent=2, sort_keys=True) + "\n"
    descriptor, tmp_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".new",
        dir=path.parent,
    )
    tmp = pathlib.Path(tmp_name)
    try:
        os.fchmod(descriptor, _PRIVATE_MODE)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            descriptor = None
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(tmp, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except Exception:
        if descriptor is not None:
            os.close(descriptor)
        with contextlib.suppress(OSError):
            tmp.unlink()
        raise
