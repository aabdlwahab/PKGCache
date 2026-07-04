#!/usr/bin/env python3
"""Control-UI HTTP layer — thin routing over the data/live/jobs modules.

Why stdlib only: this project serves air-gapped networks, where pulling a web
framework's dependency tree is the exact problem we're solving. The backend uses
nothing but the standard library.

The React console (webui/console, served by the separate `console` nginx
container) calls these JSON endpoints; nginx reverse-proxies /api here. For direct
access this server still serves the legacy single-file UI at / if present.

  python3 webui/server.py            # then open http://127.0.0.1:8088

SECURITY: these endpoints run real git/dvc/docker commands and there is NO auth.
The server binds 0.0.0.0 — only run it on a trusted network. Set UI_HOST=127.0.0.1
to restrict it to localhost.
"""
import http.client
import json
import re
import ssl
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import projects
from config import HOST, PORT, WEBROOT, endpoints
from jobs import Jobs
from live import LiveFeed
from ops import Operations, lockwarm_path, shuttle_info
from reads import Reads
from usage import Usage

# The control-plane collaborators, wired once and shared by the request handler:
# Jobs runs cache workflows through the Operations service; LiveFeed polls the
# proxies; Reads serves the read side (its disk-usage cache injected).
_operations = Operations()
_jobs = Jobs(_operations)
_live = LiveFeed()
_reads = Reads(Usage())


def proxies(project=projects.GLOBAL):
    """Compose container status (best-effort) plus live per-role health for a
    project: the real 'N roles up' count and online/offline state the console's top
    bar shows. The container status is instance-wide; health is per-project."""
    out = _reads.status()
    out.update(_live.health(project))
    return out

_JOB_RE = re.compile(r"/api/jobs/(\d+)")
_PROJECT_RE = re.compile(r"/api/projects/([a-z0-9-]+)")


class _LimitedReader:
    """Read exactly `n` bytes from a socket rfile, then EOF — so http.client streams
    the upload body through without over-reading into the next request on a keep-alive
    connection. http.client calls read(blocksize), which is all we implement."""

    def __init__(self, fp, n):
        self._fp = fp
        self._n = n

    def read(self, size=-1):
        if self._n <= 0:
            return b""
        want = self._n if (size is None or size < 0) else min(size, self._n)
        data = self._fp.read(want)
        self._n -= len(data)
        return data


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):  # quiet
        pass

    def _send_json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_file(self, path, ctype):
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

    def _send_download(self, path, filename):
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
            "/api/jobs": _jobs.snapshot,
            "/api/projects": lambda: {"projects": projects.list_projects()},
            "/healthz": lambda: {"status": "ok"},
        }
        if path in routes:
            return self._send_json(routes[path]())

        # Project-scoped routes → take ?project=<name> (default: global). Live feeds
        # (proxies/downloads/recent) and cache views are all per-project.
        scoped = {
            "/api/proxies": proxies,
            "/api/downloads": _live.downloads,
            "/api/recent": _live.recent,
            "/api/manifests": _reads.manifests,
            "/api/stats": _reads.stats,
            "/api/history": _reads.history,
            "/api/endpoints": endpoints,
            "/api/shuttle": shuttle_info,
        }
        if path in scoped:
            try:
                return self._send_json(scoped[path](self._project()))
            except (ValueError, RuntimeError) as exc:
                return self._send_json({"error": str(exc)}, 400)
        if path == "/api/packages":
            params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            return self._send_json(_reads.packages(params))
        # files write-token status — never returns the token itself, only whether one exists.
        if path == "/api/token":
            return self._send_json({"set": projects.has_write_token(self._project())})
        # Download the rewritten uv.lock a lockwarm job produced for this project.
        if path == "/api/lockfile":
            project = self._project()
            try:
                if project != projects.GLOBAL:
                    projects.validate_name(project)
            except projects.ProjectError as exc:
                return self._send_json({"error": str(exc)}, 400)
            lock = lockwarm_path(project)
            if not lock.is_file():
                return self._send_json({"error": "no rewritten lock for this project yet"}, 404)
            return self._send_download(lock, "uv.lock")
        m = _JOB_RE.fullmatch(path)
        if m:
            job = _jobs.get(int(m.group(1)))
            return self._send_json(job or {"error": "no such job"}, 200 if job else 404)
        if path in ("/", "/index.html"):
            return self._send_file(WEBROOT / "index.html", "text/html; charset=utf-8")
        self.send_error(404)

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(length) or b"{}")

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        # Create a project: allocate its ports + cache tree. The always-on cache
        # process binds the new ports on its next poll (no container recreate).
        if path == "/api/projects":
            try:
                rec = projects.create((self._read_body() or {}).get("name", ""))
            except (ValueError, RuntimeError) as exc:
                return self._send_json({"error": str(exc)}, 400)
            return self._send_json(rec, 201)
        # Run a cache op as a background job (checkpoint/export/import/rollback/mode);
        # `project` may be in the body to scope it (default: global).
        if path == "/api/jobs":
            try:
                params = self._read_body()
                action = params.pop("action", "")
                jid = _jobs.start(action, params)
            except (ValueError, RuntimeError) as exc:
                return self._send_json({"error": str(exc)}, 400)
            return self._send_json({"id": jid})
        # Generate/rotate a project's files write token; returned ONCE (copy it now).
        if path == "/api/token":
            body = self._read_body() or {}
            project = body.get("project", projects.GLOBAL) or projects.GLOBAL
            try:
                token = projects.rotate_write_token(project)
            except (ValueError, RuntimeError) as exc:
                return self._send_json({"error": str(exc)}, 400)
            return self._send_json({"token": token})
        # Console upload proxy: stream the raw body → PUT to the files role with the
        # project's Bearer token injected here (the browser never sees the token).
        if path == "/api/artifacts":
            return self._files_proxy("PUT")
        return self.send_error(404)

    def _files_proxy(self, method):
        """Proxy a console PUT/DELETE to the project's files role, injecting the write
        token from the registry so the browser never holds it. Streams the request
        body straight through (no buffering)."""
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        project = (q.get("project", [projects.GLOBAL])[0] or projects.GLOBAL)
        rel = (q.get("path", [""])[0] or "").strip("/")
        if not rel:
            return self._send_json({"error": "path required"}, 400)
        try:
            port = projects.ports(project).get("files")
        except projects.ProjectError as exc:
            return self._send_json({"error": str(exc)}, 400)
        if not port:
            return self._send_json({"error": "files role not allocated for this project"}, 400)
        token = projects.write_token(project)
        if not token:
            return self._send_json({"error": "no write token set — generate one first"}, 409)

        url = "/" + urllib.parse.quote(rel, safe="/")
        if method == "PUT" and q.get("overwrite", ["0"])[0] in ("1", "true", "yes"):
            url += "?overwrite=1"
        headers = {"Authorization": f"Bearer {token}"}
        body = None
        if method == "PUT":
            length = int(self.headers.get("Content-Length", 0))
            headers["Content-Length"] = str(length)
            headers["Content-Type"] = "application/octet-stream"
            body = _LimitedReader(self.rfile, length)
        try:
            conn = http.client.HTTPSConnection(
                "pkgcache", port, timeout=3600, context=ssl._create_unverified_context())
            conn.request(method, url, body=body, headers=headers)
            resp = conn.getresponse()
            payload = resp.read()
            code, ctype = resp.status, resp.getheader("Content-Type", "application/json")
        except OSError as exc:
            return self._send_json({"error": f"files role unreachable: {exc}"}, 502)
        finally:
            try:
                conn.close()
            except Exception:  # noqa: BLE001
                pass
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_DELETE(self):
        if urllib.parse.urlparse(self.path).path == "/api/artifacts":
            return self._files_proxy("DELETE")
        # Drop a project from the registry (frees its ports; leaves cached bytes on
        # disk). Path: /api/projects/<name>.
        m = _PROJECT_RE.fullmatch(urllib.parse.urlparse(self.path).path)
        if not m:
            return self.send_error(404)
        try:
            rec = projects.delete(m.group(1))
        except (ValueError, RuntimeError) as exc:
            return self._send_json({"error": str(exc)}, 400)
        return self._send_json(rec)


def main():
    print(f"package-cache UI on http://{HOST}:{PORT}  (Ctrl-C to stop)")
    if HOST not in ("127.0.0.1", "localhost"):
        print(f"WARNING: bound to {HOST} — these endpoints run real commands.")
    _live.start()  # poll proxy downloads in the background
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
