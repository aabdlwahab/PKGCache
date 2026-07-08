"""Project-registry file gateway: the one place the backend reads and writes the
shared config/projects.json (the same file pkgcache reads). Isolating the file I/O
here keeps the projects service pure domain logic and makes the registry a single
swappable boundary.

The path is resolved from PKGCACHE_PROJECTS on every call, so runtime and tests pick
up the current value with no import-time capture. LOCK serializes the
read-modify-write a mutation performs; the webui is the only writer (pkgcache reads
over the atomic temp→rename below)."""
import json
import os
import pathlib
import threading

from app import settings
from app.errors import ApiError

# The API server is multi-threaded (ThreadingHTTPServer), so two concurrent
# creates/deletes would race on the registry read-modify-write. Callers hold this
# across the whole load→mutate→save so each sees every already-committed project.
LOCK = threading.Lock()


def path():
    """The registry file path: env PKGCACHE_PROJECTS, else config/projects.json."""
    return pathlib.Path(
        os.environ.get("PKGCACHE_PROJECTS") or (settings.ROOT / "config" / "projects.json"))


def load():
    """The registry dict with defaults filled in. A MISSING file is a legitimate
    first run → empty registry. A file that EXISTS but is unreadable/corrupt is NOT
    treated as empty (that once put two projects on identical ports) — fail loudly
    with an ApiError so a mutation aborts instead of clobbering."""
    p = path()
    data = {}
    if p.is_file():
        try:
            data = json.loads(p.read_text()) or {}
        except (OSError, ValueError) as exc:
            raise ApiError(
                f"project registry {p} is unreadable/corrupt ({exc}); refusing to "
                f"proceed — fix or remove the file"
            ) from exc
    data.setdefault("projects", {})
    data.setdefault("tokens", {})   # {project: write-token} for the files role
    data.setdefault("offline", {})  # {project: true} soft offline flags (global incl.)
    data.setdefault("owners", {})   # {project: username} — absent = superuser-owned
    return data


def save(data):
    """Persist the registry atomically (temp→rename) so a crash never leaves a
    half-written file the other process would fail to parse. The temp name is unique
    per writer so two concurrent saves can't corrupt a shared .new file (the rename
    is atomic; the write to the temp is not)."""
    p = path()
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_name(f"{p.name}.{os.getpid()}.{threading.get_ident()}.new")
    tmp.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    os.replace(tmp, p)
