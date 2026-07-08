"""Users file gateway: the one place the backend reads and writes the account store
(config/users.json). Isolating the file I/O here keeps the accounts service pure
domain logic and makes the store a single swappable boundary — the same shape as the
project-registry gateway beside it.

The store holds ONLY stored accounts (the env-superuser is never written here). Each
record is `{"role", "salt", "hash", "reports_to"}`; the accounts service owns the
meaning, this module just persists the dict.

The path is resolved from UI_USERS on every call, so runtime and tests pick up the
current value with no import-time capture. LOCK serializes the read-modify-write a
mutation performs (the webui is the only writer)."""
import json
import os
import pathlib
import threading

from app import settings
from app.errors import ApiError

# The account server is multi-threaded (ThreadingHTTPServer); a mutation holds this
# across the whole load→mutate→save so concurrent writes can't clobber each other.
LOCK = threading.Lock()


def path():
    """The store file path: env UI_USERS, else config/users.json."""
    return pathlib.Path(os.environ.get("UI_USERS") or (settings.ROOT / "config" / "users.json"))


def load():
    """The store dict with defaults filled in. A MISSING file is a legitimate first
    run → empty store. A file that EXISTS but is unreadable/corrupt is NOT treated as
    empty (that would silently drop every account and let the next save clobber the
    file) — fail loudly with an ApiError so a mutation aborts instead."""
    p = path()
    data = {}
    if p.is_file():
        try:
            data = json.loads(p.read_text()) or {}
        except (OSError, ValueError) as exc:
            raise ApiError(
                f"users store {p} is unreadable/corrupt ({exc}); refusing to proceed "
                f"— fix or remove the file"
            ) from exc
    data.setdefault("users", {})
    return data


def save(data):
    """Persist the store atomically (temp→rename) so a crash never leaves a
    half-written file. The temp name is unique per writer so two concurrent saves
    can't corrupt a shared .new file (the rename is atomic; the write is not)."""
    p = path()
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_name(f"{p.name}.{os.getpid()}.{threading.get_ident()}.new")
    tmp.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    os.replace(tmp, p)
