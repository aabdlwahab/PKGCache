"""The declarative route table and the controllers it dispatches to.

One table (method, pattern, controller) replaces the four ad-hoc dispatch styles
the original server.py grew (an exact-match dict, a "scoped" dict, if-path chains,
and loose regexes). A controller takes a Request and returns a Response (which the
dispatcher serializes as JSON) or None when it has already written the response body
itself (a file download or the streamed artifact proxy).

Errors are uniform: a controller signals a client problem by raising ApiError (or a
subclass — ProjectError, OpError), and dispatch() renders it as {"error": message}
with the carried status. Anything else propagates and becomes a 500, so a real bug
is never silently reported as a 400.

Controllers reach the stateful services (jobs, live, reads) through the request's
handler, which app.main wires once via handler.configure(); the stateless helpers
(projects, urls, operations) are imported directly."""
import json
import re
import urllib.parse

from app import urls
from app.api import files_proxy
from app.errors import ApiError
from app.services import projects
from app.services.operations import lockwarm_path, shuttle_info


class Request:
    """A parsed request plus shortcuts to the query, JSON body, path captures and the
    wired services. Thin wrapper over the live BaseHTTPRequestHandler so a controller
    never touches raw sockets or urllib itself."""

    def __init__(self, handler, path):
        self.handler = handler
        self._url = urllib.parse.urlparse(path)
        self.query = urllib.parse.parse_qs(self._url.query)
        self.match = {}  # path-capture groups, filled by dispatch()

    @property
    def path(self):
        return self._url.path

    @property
    def project(self):
        """The ?project=<name> query param, defaulting to the global project."""
        return self.query.get("project", [projects.GLOBAL])[0] or projects.GLOBAL

    def q1(self, key, default=None):
        """The first value of a query param, or `default`."""
        v = self.query.get(key)
        return v[0] if v else default

    def body(self):
        """The JSON request body (empty object when absent). A malformed body is a
        client error, not a server crash — surfaced as a 400."""
        length = int(self.handler.headers.get("Content-Length", 0))
        raw = self.handler.rfile.read(length) or b"{}"
        try:
            return json.loads(raw)
        except ValueError as exc:
            raise ApiError(f"invalid JSON body: {exc}", 400) from exc

    # Wired services (set on the handler by handler.configure()).
    @property
    def jobs(self):
        return self.handler.jobs

    @property
    def live(self):
        return self.handler.live

    @property
    def reads(self):
        return self.handler.reads


class Response:
    """A JSON response the dispatcher serializes. Controllers that stream a body
    themselves (downloads, the artifact proxy) return None instead."""

    __slots__ = ("body", "status")

    def __init__(self, body, status=200):
        self.body = body
        self.status = status


def _int(value, default):
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


# ---- controllers ---------------------------------------------------------

def healthz(req):
    return Response({"status": "ok"})


def index(req):
    return Response({
        "service": "pkgcache control API",
        "console": "the React console (console container) serves the UI on :8088 "
                   "and reverse-proxies /api here",
    })


def get_jobs(req):
    return Response(req.jobs.snapshot())


def get_job(req):
    job = req.jobs.get(int(req.match["id"]), offset=_int(req.q1("offset"), 0))
    return Response(job or {"error": "no such job"}, 200 if job else 404)


def list_projects(req):
    return Response({"projects": projects.list_projects()})


def get_proxies(req):
    """Container status (instance-wide) merged with live per-role health (per-project)
    — the 'N roles up' count and online/offline state the console's top bar shows."""
    out = req.reads.status()
    out.update(req.live.health(req.project))
    return Response(out)


def get_downloads(req):
    return Response(req.live.downloads(req.project))


def get_recent(req):
    return Response(req.live.recent(req.project))


def get_manifests(req):
    return Response(req.reads.manifests(req.project))


def get_stats(req):
    return Response(req.reads.stats(req.project))


def get_history(req):
    return Response(req.reads.history(req.project))


def get_endpoints(req):
    return Response(urls.endpoints(req.project))


def get_shuttle(req):
    return Response(shuttle_info(req.project))


def get_packages(req):
    return Response(req.reads.packages(
        req.project, eco=req.q1("eco"), q=req.q1("q"),
        sort=req.q1("sort", "name"), page=_int(req.q1("page"), 1),
    ))


def get_token(req):
    """files write-token status — never the token itself, only whether one exists."""
    return Response({"set": projects.has_write_token(req.project)})


def get_lockfile(req):
    """Download the rewritten uv.lock a lockwarm job produced for this project."""
    project = req.project
    if project != projects.GLOBAL:
        projects.validate_name(project)  # ProjectError → 400
    lock = lockwarm_path(project)
    if not lock.is_file():
        return Response({"error": "no rewritten lock for this project yet"}, 404)
    req.handler.send_download(lock, "uv.lock")
    return None


def create_project(req):
    """Register a project + create its cache tree. The always-on cache process starts
    routing its prefix on the next registry poll (no container recreate)."""
    return Response(projects.create((req.body() or {}).get("name", "")), 201)


def post_jobs(req):
    """Run a cache op as a background job (checkpoint/export/import/rollback/mode);
    `project` may be in the body to scope it (default: global)."""
    params = req.body()
    action = params.pop("action", "")
    return Response({"id": req.jobs.start(action, params)})


def post_token(req):
    """Generate/rotate a project's files write token; returned ONCE (copy it now)."""
    body = req.body() or {}
    project = body.get("project", projects.GLOBAL) or projects.GLOBAL
    return Response({"token": projects.rotate_write_token(project)})


def put_artifact(req):
    """Console upload proxy: stream the body → PUT to the files role with the
    project's Bearer token injected server-side (the browser never sees it)."""
    files_proxy.proxy(req.handler, "PUT")
    return None


def delete_artifact(req):
    files_proxy.proxy(req.handler, "DELETE")
    return None


def delete_project(req):
    """Drop a project from the registry (leaves cached bytes on disk)."""
    return Response(projects.delete(req.match["name"]))


# ---- the table -----------------------------------------------------------

def _route(method, pattern, fn, *, exact=True):
    """A (method, compiled-fullmatch-pattern, controller) row. `exact` escapes the
    pattern for a literal path; pass exact=False for a capturing regex."""
    return (method, re.compile(re.escape(pattern) if exact else pattern), fn)


# The DELETE project pattern captures ANY non-empty segment and lets
# projects.validate_name be the gatekeeper — a stricter route pattern would silently
# 404 names the create API accepts (e.g. with '.' or '_').
_PROJECT_PATH = r"/api/projects/(?P<name>[^/]+)"

ROUTES = [
    _route("GET", "/healthz", healthz),
    _route("GET", "/api/jobs", get_jobs),
    _route("GET", r"/api/jobs/(?P<id>\d+)", get_job, exact=False),
    _route("GET", "/api/projects", list_projects),
    _route("GET", "/api/proxies", get_proxies),
    _route("GET", "/api/downloads", get_downloads),
    _route("GET", "/api/recent", get_recent),
    _route("GET", "/api/manifests", get_manifests),
    _route("GET", "/api/stats", get_stats),
    _route("GET", "/api/history", get_history),
    _route("GET", "/api/endpoints", get_endpoints),
    _route("GET", "/api/shuttle", get_shuttle),
    _route("GET", "/api/packages", get_packages),
    _route("GET", "/api/token", get_token),
    _route("GET", "/api/lockfile", get_lockfile),
    _route("GET", "/", index),
    _route("POST", "/api/projects", create_project),
    _route("POST", "/api/jobs", post_jobs),
    _route("POST", "/api/token", post_token),
    _route("POST", "/api/artifacts", put_artifact),
    _route("DELETE", "/api/artifacts", delete_artifact),
    _route("DELETE", _PROJECT_PATH, delete_project, exact=False),
]


def dispatch(handler, method, raw_path):
    """Find the route for (method, path) and run its controller, rendering the
    Response as JSON (or letting the controller stream its own body). Returns False if
    nothing matched, so the caller can send a 404. ApiError → its carried status;
    any other exception propagates to a 500."""
    path = urllib.parse.urlparse(raw_path).path
    for m, pattern, fn in ROUTES:
        if m != method:
            continue
        found = pattern.fullmatch(path)
        if found is None:
            continue
        req = Request(handler, raw_path)
        req.match = found.groupdict()
        try:
            resp = fn(req)
        except ApiError as exc:
            handler.send_json({"error": exc.message}, exc.status)
            return True
        if resp is not None:
            handler.send_json(resp.body, resp.status)
        return True
    return False
