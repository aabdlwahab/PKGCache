"""The pkgcache-container HTTP boundary: the one gateway for talking to the running
proxies over the compose network. Owns the internal (unverified) TLS context — the
roles terminate TLS in-process with the private CA, so internal calls skip
verification — and builds every project-prefixed URL through the projects service so
the prefix rules live in one place.

Read feeds (progress/health/ledger) go through fetch_json; the checkpoint's git
maintenance and the console's artifact upload build their target URLs here and drive
the request themselves, since those need bespoke error handling / body streaming."""
import http.client
import json
import ssl
import time
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

from app import manifest
from app.services import projects

# Internal polls/among roles that serve HTTPS with the private CA — skip verification
# (same context the live poller and the lock warmer use).
INTERNAL_TLS = ssl._create_unverified_context()

# eco label → pkgcache role. The apt subdir/ledger carries BOTH apt and apk, so both
# resolve to the apt role (distinguished by the `eco` filter on the ledger query).
_ECO_ROLE = {"docker": "oci", "npm": "npm", "pip": "pypi", "apt": "apt",
             "apk": "apt", "git": "git", "files": "files"}
_ROLES = ("oci", "npm", "pypi", "apt", "git", "files")

# Fan-out pool for the per-role /+ledger/stats calls (6 roles); bounded like the live
# poller so a stats request can't spawn threads without limit.
_POOL = ThreadPoolExecutor(max_workers=6)


def fetch_json(url, timeout=2):
    """GET `url` and parse JSON, or None if unreachable / not JSON. Used by the live
    poller for the progress and /healthz feeds, where a down role is expected and
    must not raise."""
    try:
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        ctx = INTERNAL_TLS if url.startswith("https") else None
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception:  # noqa: BLE001 - unreachable proxy / between requests
        return None


# ---- ledger reads (the control UI's package / stats views) ----------------
# The webui no longer opens ledger.db directly; it reads pkgcache's /+ledger/* admin
# endpoints (one per role, prefixed per project) and combines them in the reads
# service. A short last-good cache smooths a single role blipping between polls —
# a momentary miss serves the previous value (up to _STALE_OK) instead of flashing
# an empty panel, which the old direct-file read never did.
_STALE_OK = 30.0
_cache: dict = {}  # url -> (monotonic_ts, value)


def _ledger_url(project, role, path, params=None):
    scheme = "http" if role == "apt" else "https"
    port = projects.ROLE_PORT[role]
    url = f"{scheme}://pkgcache:{port}{projects.role_prefix(project, role)}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    return url


def _fetch_ledger(url, timeout):
    """fetch_json with a last-good fallback: on a miss, serve the cached value if it
    is still fresh enough, so one unreachable poll doesn't blank the view."""
    now = time.monotonic()
    data = fetch_json(url, timeout)
    if data is not None:
        _cache[url] = (now, data)
        return data
    hit = _cache.get(url)
    if hit and now - hit[0] < _STALE_OK:
        return hit[1]
    return None


def ledger_artifacts(project, eco, q=None, sort="name", page=1, page_size=0):
    """Artifact rows for one ecosystem of a project, from its role's /+ledger/artifacts
    (page_size=0 → the full inventory, for the manifest view). [] if unreachable."""
    role = _ECO_ROLE[eco]
    _, ecosystem = manifest.ECOS[eco]
    params = {"eco": ecosystem, "sort": sort, "page": page, "page_size": page_size}
    if q:
        params["q"] = q
    data = _fetch_ledger(_ledger_url(project, role, "/+ledger/artifacts", params), timeout=5)
    return data.get("artifacts", []) if isinstance(data, dict) else []


def ledger_stats(project):
    """{role: stats-dict|None} — each role's /+ledger/stats, fetched concurrently. The
    reads service combines these across roles into the /api/stats view."""
    urls = {role: _ledger_url(project, role, "/+ledger/stats") for role in _ROLES}
    values = _POOL.map(lambda u: _fetch_ledger(u, timeout=8), urls.values())
    return dict(zip(urls.keys(), values))


def git_maintain_url(project):
    """The git role's /+maintain endpoint for THIS project: the shared git port plus
    the project's URL prefix (all projects share the role ports; the project rides the
    path — see projects.role_prefix). Raises ProjectError if the project is unknown."""
    port = projects.ports(project)["git"]
    return f"https://pkgcache:{port}{projects.role_prefix(project, 'git')}/+maintain"


def files_target(project, rel, overwrite=False):
    """The path (plus query) an artifact PUT/DELETE must hit on the shared files port
    for THIS project: the project's URL prefix (see projects.role_prefix) + the quoted
    artifact path. Raises ProjectError for an unknown project."""
    projects.ports(project)  # existence check — raises ProjectError on a typo
    url = projects.role_prefix(project, "files") + "/" + urllib.parse.quote(rel, safe="/")
    return url + "?overwrite=1" if overwrite else url


def files_connection(timeout=3600):
    """An HTTPS connection to the shared files role on the `pkgcache` container, for
    the console upload/delete proxy to stream a request body through."""
    return http.client.HTTPSConnection(
        "pkgcache", projects.ROLE_PORT["files"], timeout=timeout, context=INTERNAL_TLS)
