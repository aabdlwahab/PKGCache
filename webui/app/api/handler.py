"""The HTTP server plumbing: a BaseHTTPRequestHandler that parses nothing itself and
delegates every request to the declarative route table (app.api.routes). It owns
only the response-writing helpers and the wiring of the stateful services, which
app.main injects once via configure() before the server starts.

No git/dvc/sqlite/socket work happens here or in routes — controllers call services,
services call gateways."""
import json
from http.server import BaseHTTPRequestHandler

from app.api import routes


def configure(jobs, live, reads, sessions, accounts):
    """Wire the stateful services the controllers dispatch to (called once at startup
    by app.main). They live as class attributes so every request handler instance —
    and thus every Request — sees them, with no module-level singletons."""
    Handler.jobs = jobs
    Handler.live = live
    Handler.reads = reads
    Handler.sessions = sessions
    Handler.accounts = accounts


class Handler(BaseHTTPRequestHandler):
    # Injected by configure() before serving.
    jobs = None
    live = None
    reads = None
    sessions = None
    accounts = None

    def log_message(self, *_):  # quiet
        pass

    def send_json(self, obj, code=200, headers=None):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for name, value in headers or ():
            self.send_header(name, value)
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

    def do_GET(self):
        if not routes.dispatch(self, "GET", self.path):
            self.send_error(404)

    def do_POST(self):
        if not routes.dispatch(self, "POST", self.path):
            self.send_error(404)

    def do_PATCH(self):
        if not routes.dispatch(self, "PATCH", self.path):
            self.send_error(404)

    def do_DELETE(self):
        if not routes.dispatch(self, "DELETE", self.path):
            self.send_error(404)
