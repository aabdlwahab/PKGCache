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
import http.cookies
import json
import re
import urllib.parse

from app import settings, urls
from app.api import files_proxy
from app.errors import ApiError, AuthError, ForbiddenError
from app.services import projects
from app.services.operations import lockwarm_path, shuttle_info

# Distinguishes "unresolved" from "resolved to None" when caching req.user.
_UNSET = object()


class Request:
    """A parsed request plus shortcuts to the query, JSON body, path captures and the
    wired services. Thin wrapper over the live BaseHTTPRequestHandler so a controller
    never touches raw sockets or urllib itself."""

    def __init__(self, handler, path):
        self.handler = handler
        self._url = urllib.parse.urlparse(path)
        self.query = urllib.parse.parse_qs(self._url.query)
        self.match = {}  # path-capture groups, filled by dispatch()
        self._user = _UNSET  # resolved lazily + cached (see .user)

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

    @property
    def sessions(self):
        return self.handler.sessions

    @property
    def accounts(self):
        return self.handler.accounts

    # ---- auth ----------------------------------------------------------------
    @property
    def cookies(self):
        jar = http.cookies.SimpleCookie()
        try:
            jar.load(self.handler.headers.get("Cookie", ""))
        except http.cookies.CookieError:
            pass  # a malformed Cookie header just means no cookies, not a 500
        return jar

    @property
    def client_ip(self):
        addr = getattr(self.handler, "client_address", None)
        return addr[0] if addr else "?"

    @property
    def user(self):
        """The authenticated Account for this request, or None. Resolved once from the
        session cookie: token → sessions → username → account."""
        if self._user is _UNSET:
            self._user = self._resolve_user()
        return self._user

    def require_user(self):
        """The authenticated Account, or an AuthError(401) — the guard every
        auth-required controller calls first."""
        user = self.user
        if user is None:
            raise AuthError("authentication required")
        return user

    def _resolve_user(self):
        morsel = self.cookies.get(settings.SESSION_COOKIE)
        if morsel is None:
            return None
        username = self.sessions.resolve(morsel.value)
        if username is None:
            return None
        return self.accounts.get(username)

    # ---- authorization guards ------------------------------------------------
    # Every guard is a no-op when auth is not configured (accounts.enabled() False),
    # so an un-migrated deployment keeps working exactly as before; setting a root
    # superuser turns enforcement on. When enabled they require a session and the
    # right relationship to the project's owner, else raise Auth/Forbidden.

    def require_authed(self):
        """Any signed-in caller (used for instance-wide reads like the job list)."""
        if not self.accounts.enabled():
            return None
        return self.require_user()

    def require_view(self, project):
        """The caller must be able to view `project` — its owner, one of the owner's
        reports, or a superuser."""
        if not self.accounts.enabled():
            return None
        actor = self.require_user()
        if not self.accounts.can_view(actor, projects.owner(project)):
            raise ForbiddenError(f"not authorized for project '{project}'")
        return actor

    def require_operate(self, project):
        """The caller must own `project` (or be a superuser) to run owner-level ops."""
        if not self.accounts.enabled():
            return None
        actor = self.require_user()
        if not self.accounts.can_operate(actor, projects.owner(project)):
            raise ForbiddenError(f"not authorized to operate project '{project}'")
        return actor

    def require_create(self):
        """Only an admin or superuser may create a project (and becomes its owner)."""
        if not self.accounts.enabled():
            return None
        actor = self.require_user()
        if actor.role not in ("admin", "superuser"):
            raise ForbiddenError("only admins and superusers can create projects")
        return actor

    def require_superuser(self):
        """Superuser-only actions (instance mode, reassigning a project's owner)."""
        if not self.accounts.enabled():
            return None
        actor = self.require_user()
        if actor.role != "superuser":
            raise ForbiddenError("superuser only")
        return actor


class Response:
    """A JSON response the dispatcher serializes. Controllers that stream a body
    themselves (downloads, the artifact proxy) return None instead. `headers` carries
    extra response headers (e.g. Set-Cookie on login/logout)."""

    __slots__ = ("body", "status", "headers")

    def __init__(self, body, status=200, headers=None):
        self.body = body
        self.status = status
        self.headers = headers


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
    req.require_authed()
    return Response(req.jobs.snapshot())


def get_job(req):
    req.require_authed()
    job = req.jobs.get(int(req.match["id"]), offset=_int(req.q1("offset"), 0))
    return Response(job or {"error": "no such job"}, 200 if job else 404)


def list_projects(req):
    """Projects the caller may see: a superuser sees all; an admin sees the ones they
    own; a user sees their admin's. Open (all) when auth is not configured."""
    visible = projects.list_projects()
    if req.accounts.enabled():
        actor = req.require_user()
        visible = [p for p in visible if req.accounts.can_view(actor, p["owner"])]
    return Response({"projects": visible})


def get_proxies(req):
    """Container status (instance-wide) merged with live per-role health (per-project)
    — the 'N roles up' count and online/offline state the console's top bar shows."""
    req.require_view(req.project)
    out = req.reads.status()
    out.update(req.live.health(req.project))
    return Response(out)


def get_downloads(req):
    req.require_view(req.project)
    return Response(req.live.downloads(req.project))


def get_recent(req):
    req.require_view(req.project)
    return Response(req.live.recent(req.project))


def get_manifests(req):
    req.require_view(req.project)
    return Response(req.reads.manifests(req.project))


def get_stats(req):
    req.require_view(req.project)
    return Response(req.reads.stats(req.project))


def get_history(req):
    req.require_view(req.project)
    return Response(req.reads.history(req.project))


def get_endpoints(req):
    req.require_view(req.project)
    return Response(urls.endpoints(req.project))


def get_shuttle(req):
    req.require_view(req.project)
    return Response(shuttle_info(req.project))


def get_packages(req):
    req.require_view(req.project)
    return Response(req.reads.packages(
        req.project, eco=req.q1("eco"), q=req.q1("q"),
        sort=req.q1("sort", "name"), page=_int(req.q1("page"), 1),
    ))


def get_token(req):
    """files write-token status — never the token itself, only whether one exists."""
    req.require_view(req.project)
    return Response({"set": projects.has_write_token(req.project)})


def get_lockfile(req):
    """Download the rewritten uv.lock a lockwarm job produced for this project."""
    req.require_view(req.project)
    project = req.project
    if project != projects.GLOBAL:
        projects.validate_name(project)  # ProjectError → 400
    lock = lockwarm_path(project)
    if not lock.is_file():
        return Response({"error": "no rewritten lock for this project yet"}, 404)
    req.handler.send_download(lock, "uv.lock")
    return None


def create_project(req):
    """Register a project + create its cache tree, owned by the creating admin/
    superuser. The always-on cache process starts routing its prefix on the next
    registry poll (no container recreate)."""
    actor = req.require_create()
    name = (req.body() or {}).get("name", "")
    owner = actor.username if actor else None
    return Response(projects.create(name, owner=owner), 201)


def post_jobs(req):
    """Run a cache op as a background job. Authorization is per action: the instance-
    wide `mode` recreate is superuser-only; `lockwarm` (a read-through warm) is view-
    level; checkpoint/rollback/export/import are owner-level. `project` in the body
    scopes it (default: global)."""
    params = req.body()
    action = params.pop("action", "")
    project = params.get("project", projects.GLOBAL) or projects.GLOBAL
    if action == "mode":
        req.require_superuser()
    elif action == "lockwarm":
        req.require_view(project)
    else:
        req.require_operate(project)
    return Response({"id": req.jobs.start(action, params)})


def set_project_mode(req):
    """Flip ONE project's soft offline flag (global included) — a registry write the
    always-on cache process applies on its next poll, serving that project cache-only
    without touching the others. Distinct from the instance-wide `mode` job, which
    recreates the container and takes every project with it."""
    req.require_operate(req.match["name"])
    target = (req.body() or {}).get("target", "")
    if target not in ("online", "offline"):
        raise ApiError("mode target must be 'online' or 'offline'", 400)
    return Response(projects.set_offline(req.match["name"], target == "offline"))


def set_project_owner(req):
    """Reassign a project to another admin/superuser — a superuser action ('which
    admin the project belongs to')."""
    req.require_superuser()
    new_owner = (req.body() or {}).get("owner", "")
    target = req.accounts.get(new_owner)
    if target is None or target.role not in ("admin", "superuser"):
        raise ApiError("owner must be an existing admin or superuser", 400)
    return Response(projects.set_owner(req.match["name"], new_owner))


def post_token(req):
    """Generate/rotate a project's files write token; returned ONCE (copy it now)."""
    body = req.body() or {}
    project = body.get("project", projects.GLOBAL) or projects.GLOBAL
    req.require_operate(project)
    return Response({"token": projects.rotate_write_token(project)})


def put_artifact(req):
    """Console upload proxy: stream the body → PUT to the files role with the
    project's Bearer token injected server-side (the browser never sees it). Upload is
    view-level — a project's members may push artifacts."""
    req.require_view(req.project)
    files_proxy.proxy(req.handler, "PUT")
    return None


def delete_artifact(req):
    """Delete an artifact — owner-level (a project's members read/upload; only the
    owner removes)."""
    req.require_operate(req.project)
    files_proxy.proxy(req.handler, "DELETE")
    return None


def delete_project(req):
    """Drop a project from the registry (leaves cached bytes on disk). Owner-level."""
    req.require_operate(req.match["name"])
    return Response(projects.delete(req.match["name"]))


# ---- auth controllers ----------------------------------------------------

def _account_dict(account):
    return {
        "username": account.username,
        "role": account.role,
        "reports_to": account.reports_to,
        "builtin": account.builtin,
    }


def _session_cookie(token, max_age):
    """A Set-Cookie value for the session token. HttpOnly (JS can't read it) +
    SameSite=Lax (not sent on cross-site navigations) is the CSRF baseline; Secure is
    added once the console serves TLS (settings.COOKIE_SECURE)."""
    parts = [f"{settings.SESSION_COOKIE}={token}", "Path=/", "HttpOnly", "SameSite=Lax",
             f"Max-Age={max_age}"]
    if settings.COOKIE_SECURE:
        parts.append("Secure")
    return "; ".join(parts)


def login(req):
    """Verify credentials, open a session, and set the session cookie. Failed attempts
    from an IP are throttled to blunt brute force."""
    ip = req.client_ip
    if req.sessions.blocked(ip):
        raise AuthError("too many failed attempts — try again later", 429)
    body = req.body() or {}
    account = req.accounts.authenticate(body.get("username", ""), body.get("password", ""))
    if account is None:
        req.sessions.record_failure(ip)
        raise AuthError("invalid username or password")
    req.sessions.clear_failures(ip)
    token = req.sessions.create(account.username)
    cookie = _session_cookie(token, settings.SESSION_TTL)
    return Response({"username": account.username, "role": account.role},
                    headers=[("Set-Cookie", cookie)])


def logout(req):
    """Revoke the presented session and clear the cookie. Safe to call unauthenticated
    (it just clears whatever is there)."""
    morsel = req.cookies.get(settings.SESSION_COOKIE)
    if morsel is not None:
        req.sessions.drop(morsel.value)
    return Response({"ok": True}, headers=[("Set-Cookie", _session_cookie("", 0))])


def me(req):
    """The current caller — the console bootstraps its role-gated UI from this. When
    auth is not configured it reports so (auth_enabled False), letting the console skip
    the login screen entirely rather than get a bare 401."""
    if not req.accounts.enabled():
        return Response({"auth_enabled": False, "authenticated": False})
    user = req.require_user()
    return Response({
        "auth_enabled": True, "authenticated": True,
        "username": user.username, "role": user.role, "reports_to": user.reports_to,
    })


def list_users(req):
    """Accounts the caller may see (scoped by role in the accounts service)."""
    actor = req.require_user()
    return Response({"users": [_account_dict(a) for a in req.accounts.list(actor)]})


def create_user(req):
    actor = req.require_user()
    body = req.body() or {}
    account = req.accounts.create(
        actor, body.get("username", ""), body.get("password", ""),
        body.get("role", ""), body.get("reports_to"),
    )
    return Response(_account_dict(account), 201)


def update_user(req):
    """Change a target account's role, manager, and/or password (each field optional;
    the accounts service enforces who may change what). reports_to is passed only when
    present in the body, so "omitted" stays distinct from "set to null"."""
    actor = req.require_user()
    body = req.body() or {}
    changes = {"role": body.get("role"), "password": body.get("password")}
    if "reports_to" in body:
        changes["reports_to"] = body["reports_to"]
    account = req.accounts.update(actor, req.match["name"], **changes)
    return Response(_account_dict(account))


def delete_user(req):
    actor = req.require_user()
    req.accounts.delete(actor, req.match["name"])
    return Response({"deleted": req.match["name"]})


# ---- the table -----------------------------------------------------------

def _route(method, pattern, fn, *, exact=True):
    """A (method, compiled-fullmatch-pattern, controller) row. `exact` escapes the
    pattern for a literal path; pass exact=False for a capturing regex."""
    return (method, re.compile(re.escape(pattern) if exact else pattern), fn)


# The per-project patterns (DELETE, POST …/mode) capture ANY non-empty segment and
# let projects.validate_name be the gatekeeper — a stricter route pattern would
# silently 404 names the create API accepts (e.g. with '.' or '_'). The per-account
# patterns are the same shape, gated by accounts.validate.
_PROJECT_PATH = r"/api/projects/(?P<name>[^/]+)"
_USER_PATH = r"/api/users/(?P<name>[^/]+)"

# Methods that change state — subject to the cross-origin guard below.
_MUTATING = {"POST", "PATCH", "DELETE"}

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
    _route("GET", "/api/me", me),
    _route("GET", "/api/users", list_users),
    _route("GET", "/", index),
    _route("POST", "/api/login", login),
    _route("POST", "/api/logout", logout),
    _route("POST", "/api/users", create_user),
    _route("PATCH", _USER_PATH, update_user, exact=False),
    _route("DELETE", _USER_PATH, delete_user, exact=False),
    _route("POST", "/api/projects", create_project),
    _route("POST", _PROJECT_PATH + "/mode", set_project_mode, exact=False),
    _route("POST", _PROJECT_PATH + "/owner", set_project_owner, exact=False),
    _route("POST", "/api/jobs", post_jobs),
    _route("POST", "/api/token", post_token),
    _route("POST", "/api/artifacts", put_artifact),
    _route("DELETE", "/api/artifacts", delete_artifact),
    _route("DELETE", _PROJECT_PATH, delete_project, exact=False),
]


def _hostname(value):
    """The bare hostname from a `host[:port]` or a full URL — port and IPv6 brackets
    handled by urlsplit. None if there's nothing parseable."""
    if not value:
        return None
    part = value if "//" in value else "//" + value
    return urllib.parse.urlsplit(part).hostname


def _same_origin(handler):
    """Reject a cross-site mutating request. When the browser sends an Origin, its
    HOSTNAME must match the request's Host hostname; an absent Origin (curl, and the
    same-origin navigations some browsers omit it on) is allowed. We compare hostnames
    rather than full host:port because a reverse proxy routinely rewrites the Host —
    nginx `$host`, for one, drops the port while the browser's Origin keeps it — and a
    strict netloc match would then reject legitimate same-site requests. The
    SameSite=Lax cookie is the primary CSRF defense; this is belt-and-braces."""
    origin = handler.headers.get("Origin")
    if not origin:
        return True
    origin_host = _hostname(origin)
    return origin_host is not None and origin_host == _hostname(handler.headers.get("Host", ""))


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
        if method in _MUTATING and not _same_origin(handler):
            handler.send_json({"error": "cross-origin request refused"}, 403)
            return True
        req = Request(handler, raw_path)
        req.match = found.groupdict()
        try:
            resp = fn(req)
        except ApiError as exc:
            handler.send_json({"error": exc.message}, exc.status)
            return True
        if resp is not None:
            handler.send_json(resp.body, resp.status, resp.headers)
        return True
    return False
