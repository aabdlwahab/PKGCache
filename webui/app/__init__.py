"""The control-UI backend, layered as controllers (app.api) over domain services
(app.services) over gateways (app.gateways) — the subprocess, sqlite and pkgcache
HTTP boundaries. Stdlib-only, so an air-gapped host needs no extra dependencies.

Import root is `app` (this package), reached because `webui/` is on sys.path when
the server or the tests run. See app.main for how the pieces are wired."""
