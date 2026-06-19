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
import json
import re
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from config import ENDPOINTS, HOST, PORT, WEBROOT
from jobs import get_job, jobs_snapshot, start_job
from live import live_downloads, recent_pulls, roles_health, start_refresher
from reads import git_history, live_manifests, proxy_status, read_packages


def proxies():
    """Compose container status (best-effort) plus live per-role health: the real
    'N roles up' count and online/offline state the console's top bar shows."""
    out = proxy_status()
    out.update(roles_health())
    return out

_JOB_RE = re.compile(r"/api/jobs/(\d+)")


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

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path

        # Static API routes (no params) → handler returning a JSON-able object.
        routes = {
            "/api/manifests": live_manifests,
            "/api/history": git_history,
            "/api/proxies": proxies,
            "/api/endpoints": lambda: ENDPOINTS,
            "/api/downloads": live_downloads,
            "/api/recent": recent_pulls,
            "/api/jobs": jobs_snapshot,
            "/healthz": lambda: {"status": "ok"},
        }
        if path in routes:
            return self._send_json(routes[path]())
        if path == "/api/packages":
            params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            return self._send_json(read_packages(params))
        m = _JOB_RE.fullmatch(path)
        if m:
            job = get_job(int(m.group(1)))
            return self._send_json(job or {"error": "no such job"}, 200 if job else 404)
        if path in ("/", "/index.html"):
            return self._send_file(WEBROOT / "index.html", "text/html; charset=utf-8")
        self.send_error(404)

    def do_POST(self):
        if urllib.parse.urlparse(self.path).path != "/api/jobs":
            return self.send_error(404)
        length = int(self.headers.get("Content-Length", 0))
        try:
            params = json.loads(self.rfile.read(length) or b"{}")
            action = params.pop("action", "")
            jid = start_job(action, params)
        except (ValueError, RuntimeError) as exc:
            return self._send_json({"error": str(exc)}, 400)
        return self._send_json({"id": jid})


def main():
    print(f"package-cache UI on http://{HOST}:{PORT}  (Ctrl-C to stop)")
    if HOST not in ("127.0.0.1", "localhost"):
        print(f"WARNING: bound to {HOST} — these endpoints run real commands.")
    start_refresher()  # poll proxy downloads in the background
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
