"""The HTTP controller: request parsing, routing, status codes and JSON
serialization over the domain services. No git/dvc/sqlite/socket work happens here —
every side effect is delegated to a service or gateway.

The stateful collaborators (jobs, live feed, reads) are injected by app.main via
configure() before the server starts, so this module holds no wiring of its own. The
imperative do_GET/do_POST routing is preserved from the original server.py; Phase 2
replaces it with a declarative route table + a single error contract."""
import json
import re
import urllib.parse
from http.server import BaseHTTPRequestHandler

from app import settings, urls
from app.api import files_proxy
from app.services import projects
from app.services.operations import lockwarm_path, shuttle_info

# Injected by app.main.configure() before serving.
JOBS = None
LIVE = None
READS = None


def configure(jobs, live, reads):
    """Wire the stateful services the handler dispatches to (called once at startup
    by app.main, so this controller owns no construction of its own)."""
    global JOBS, LIVE, READS
    JOBS, LIVE, READS = jobs, live, reads


def _proxies(project=projects.GLOBAL):
    """Compose container status (best-effort) plus live per-role health for a
    project: the real 'N roles up' count and online/offline state the console's top
    bar shows. The container status is instance-wide; health is per-project."""
    out = READS.status()
    out.update(LIVE.health(project))
    return out


_JOB_RE = re.compile(r"/api/jobs/(\d+)")
# Capture any non-empty segment and let projects.validate_name decide — a stricter
# pattern here silently 404s names the create API accepts (e.g. with '.' or '_').
_PROJECT_RE = re.compile(r"/api/projects/([^/]+)")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):  # quiet
        pass

    def send_json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def send_file(self, path, ctype):
        try:
            body = path.read_bytes()
        except OSError:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def send_download(self, path, filename):
        try:
            body = path.read_bytes()
        except OSError:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Disposition", f'attachment; filename="{filename}"')
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _project(self):
        """The ?project=<name> query param, defaulting to the global project."""
        qs = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        return (qs.get("project", [projects.GLOBAL])[0] or projects.GLOBAL)

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path

        # Global / aggregate routes (not per-project) → JSON-able object.
        routes = {
            "/api/jobs": JOBS.snapshot,
            "/api/projects": lambda: {"projects": projects.list_projects()},
            "/healthz": lambda: {"status": "ok"},
        }
        if path in routes:
            return self.send_json(routes[path]())

        # Project-scoped routes → take ?project=<name> (default: global). Live feeds
        # (proxies/downloads/recent) and cache views are all per-project.
        scoped = {
            "/api/proxies": _proxies,
            "/api/downloads": LIVE.downloads,
            "/api/recent": LIVE.recent,
            "/api/manifests": READS.manifests,
            "/api/stats": READS.stats,
            "/api/history": READS.history,
            "/api/endpoints": urls.endpoints,
            "/api/shuttle": shuttle_info,
        }
        if path in scoped:
            try:
                return self.send_json(scoped[path](self._project()))
            except (ValueError, RuntimeError) as exc:
                return self.send_json({"error": str(exc)}, 400)
        if path == "/api/packages":
            params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            return self.send_json(READS.packages(params))
        # files write-token status — never returns the token itself, only whether one exists.
        if path == "/api/token":
            return self.send_json({"set": projects.has_write_token(self._project())})
        # Download the rewritten uv.lock a lockwarm job produced for this project.
        if path == "/api/lockfile":
            project = self._project()
            try:
                if project != projects.GLOBAL:
                    projects.validate_name(project)
            except projects.ProjectError as exc:
                return self.send_json({"error": str(exc)}, 400)
            lock = lockwarm_path(project)
            if not lock.is_file():
                return self.send_json({"error": "no rewritten lock for this project yet"}, 404)
            return self.send_download(lock, "uv.lock")
        m = _JOB_RE.fullmatch(path)
        if m:
            job = JOBS.get(int(m.group(1)))
            return self.send_json(job or {"error": "no such job"}, 200 if job else 404)
        if path in ("/", "/index.html"):
            return self.send_file(settings.WEBROOT / "index.html", "text/html; charset=utf-8")
        self.send_error(404)

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(length) or b"{}")

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        # Create a project: register it + create its cache tree. The always-on cache
        # process starts routing its prefix on the next registry poll (no recreate).
        if path == "/api/projects":
            try:
                rec = projects.create((self._read_body() or {}).get("name", ""))
            except (ValueError, RuntimeError) as exc:
                return self.send_json({"error": str(exc)}, 400)
            return self.send_json(rec, 201)
        # Run a cache op as a background job (checkpoint/export/import/rollback/mode);
        # `project` may be in the body to scope it (default: global).
        if path == "/api/jobs":
            try:
                params = self._read_body()
                action = params.pop("action", "")
                jid = JOBS.start(action, params)
            except (ValueError, RuntimeError) as exc:
                return self.send_json({"error": str(exc)}, 400)
            return self.send_json({"id": jid})
        # Generate/rotate a project's files write token; returned ONCE (copy it now).
        if path == "/api/token":
            body = self._read_body() or {}
            project = body.get("project", projects.GLOBAL) or projects.GLOBAL
            try:
                token = projects.rotate_write_token(project)
            except (ValueError, RuntimeError) as exc:
                return self.send_json({"error": str(exc)}, 400)
            return self.send_json({"token": token})
        # Console upload proxy: stream the raw body → PUT to the files role with the
        # project's Bearer token injected here (the browser never sees the token).
        if path == "/api/artifacts":
            return files_proxy.proxy(self, "PUT")
        return self.send_error(404)

    def do_DELETE(self):
        if urllib.parse.urlparse(self.path).path == "/api/artifacts":
            return files_proxy.proxy(self, "DELETE")
        # Drop a project from the registry (leaves cached bytes on disk).
        # Path: /api/projects/<name>.
        m = _PROJECT_RE.fullmatch(urllib.parse.urlparse(self.path).path)
        if not m:
            return self.send_error(404)
        try:
            rec = projects.delete(m.group(1))
        except (ValueError, RuntimeError) as exc:
            return self.send_json({"error": str(exc)}, 400)
        return self.send_json(rec)
