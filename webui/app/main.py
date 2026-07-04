"""Composition root: build the domain services, inject them into the HTTP handler,
and serve. This is the ONE place construction happens — services take their
collaborators as arguments, so nothing else reaches for a global.

Why stdlib only: this project serves air-gapped networks, where pulling a web
framework's dependency tree is the exact problem we're solving. The backend uses
nothing but the standard library.

The React console (webui/console, served by the separate `console` nginx container)
calls the JSON endpoints; nginx reverse-proxies /api here. webui is reached over the
compose network as webui:8088 and publishes no host port of its own.

SECURITY: these endpoints run real git/dvc/docker commands and there is NO auth. The
server binds 0.0.0.0 — only run it on a trusted network. Set UI_HOST=127.0.0.1 to
restrict it to localhost."""
from http.server import ThreadingHTTPServer

from app import settings
from app.api import handler
from app.api.handler import Handler
from app.services.jobs import Jobs
from app.services.livefeed import LiveFeed
from app.services.operations import Operations
from app.services.reads import Reads
from app.services.usage import Usage


def build():
    """Construct and wire the control-plane collaborators, returning the LiveFeed so
    the caller can start its background poller. Jobs runs cache workflows through the
    Operations service; LiveFeed polls the proxies; Reads serves the read side with
    its disk-usage cache injected."""
    operations = Operations()
    jobs = Jobs(operations)
    live = LiveFeed()
    reads = Reads(Usage())
    handler.configure(jobs, live, reads)
    return live


def main():
    print(f"package-cache UI on http://{settings.HOST}:{settings.PORT}  (Ctrl-C to stop)")
    if settings.HOST not in ("127.0.0.1", "localhost"):
        print(f"WARNING: bound to {settings.HOST} — these endpoints run real commands.")
    live = build()
    live.start()  # poll proxy downloads in the background
    ThreadingHTTPServer((settings.HOST, settings.PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
