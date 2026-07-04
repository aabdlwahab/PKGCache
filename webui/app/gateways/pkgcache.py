"""The pkgcache-container HTTP boundary: the one gateway for talking to the running
proxies over the compose network. Owns the internal (unverified) TLS context — the
roles terminate TLS in-process with the private CA, so internal calls skip
verification — and builds every project-prefixed URL through the projects service so
the prefix rules live in one place.

Read feeds (progress/health) go through fetch_json; the checkpoint's git maintenance
and the console's artifact upload build their target URLs here and drive the request
themselves, since those need bespoke error handling / body streaming."""
import http.client
import json
import ssl
import urllib.parse
import urllib.request

from app.services import projects

# Internal polls/among roles that serve HTTPS with the private CA — skip verification
# (same context the live poller and the lock warmer use).
INTERNAL_TLS = ssl._create_unverified_context()


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
