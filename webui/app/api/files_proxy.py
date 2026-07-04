"""The console artifact upload/delete proxy.

The browser never holds the files-role write token: it PUT/DELETEs /api/artifacts
here, and this proxy injects the project's Bearer token (from the registry) and
streams the body straight through to the files role on the `pkgcache` container. The
target URL, port and TLS context all come from the pkgcache gateway, so the
project-prefix rules live in one place.

`proxy(handler, method)` operates on the live BaseHTTPRequestHandler so it can stream
the socket body with no buffering; it's a free function (not a handler method) to
keep the controller thin and this streaming logic isolated."""
import urllib.parse

from app.gateways import pkgcache
from app.services import projects


class LimitedReader:
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


def proxy(handler, method):
    """Proxy a console PUT/DELETE to the project's files role, injecting the write
    token from the registry so the browser never holds it. Streams the request body
    straight through (no buffering)."""
    q = urllib.parse.parse_qs(urllib.parse.urlparse(handler.path).query)
    project = (q.get("project", [projects.GLOBAL])[0] or projects.GLOBAL)
    rel = (q.get("path", [""])[0] or "").strip("/")
    if not rel:
        return handler.send_json({"error": "path required"}, 400)
    overwrite = method == "PUT" and q.get("overwrite", ["0"])[0] in ("1", "true", "yes")
    try:
        url = pkgcache.files_target(project, rel, overwrite)
    except projects.ProjectError as exc:
        return handler.send_json({"error": str(exc)}, 400)
    token = projects.write_token(project)
    if not token:
        return handler.send_json({"error": "no write token set — generate one first"}, 409)

    headers = {"Authorization": f"Bearer {token}"}
    body = None
    if method == "PUT":
        length = int(handler.headers.get("Content-Length", 0))
        headers["Content-Length"] = str(length)
        headers["Content-Type"] = "application/octet-stream"
        body = LimitedReader(handler.rfile, length)
    conn = None
    try:
        conn = pkgcache.files_connection()
        conn.request(method, url, body=body, headers=headers)
        resp = conn.getresponse()
        payload = resp.read()
        code, ctype = resp.status, resp.getheader("Content-Type", "application/json")
    except OSError as exc:
        return handler.send_json({"error": f"files role unreachable: {exc}"}, 502)
    finally:
        if conn is not None:
            try:
                conn.close()
            except Exception:  # noqa: BLE001
                pass
    handler.send_response(code)
    handler.send_header("Content-Type", ctype)
    handler.send_header("Content-Length", str(len(payload)))
    handler.end_headers()
    handler.wfile.write(payload)
