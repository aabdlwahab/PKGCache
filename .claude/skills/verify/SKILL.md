---
name: verify
description: How to run and drive this repo's services locally (outside docker) to verify changes end-to-end — pkgcache on alternate ports against a temp registry, the stdlib webui against the same file.
---

# Verifying package-registry changes locally

The compose stack usually occupies 8443/3142/8088 on this host — never restart it
for verification. Run isolated processes on alternate ports against a temp registry.

## pkgcache (the cache process)

`pkgcache/.venv-test/` has all deps (uvicorn, starlette, httpx, pytest).

- The apt port is HARDCODED 3142 (`pkgcache/src/pkgcache/__main__.py::_APT_PORT`) and
  collides with the live stack. The process survives the bind failure (logs
  `server apt:3142 failed to start` and keeps the unified port serving), so plain
  `.venv-test/bin/python -m pkgcache` works unless you need the apt role itself —
  then relocate it via a driver script:

```python
# run_pkgcache.py
import pkgcache.__main__ as m
m._APT_PORT = 13142
m.main()
```

```bash
mkdir -p /tmp/verify/caches
printf '{"projects": {"gamma": {}}}' > /tmp/verify/projects.json
cd pkgcache && PKGCACHE_PROJECTS=/tmp/verify/projects.json \
  PKGCACHE_CACHE_ROOT=/tmp/verify/caches OFFLINE=0 PKGCACHE_PROJECT_POLL=1 \
  PKGCACHE_UNIFIED_PORT=18443 PKGCACHE_SPEEDTEST_URL= PKGCACHE_HOST=127.0.0.1 \
  .venv-test/bin/python run_pkgcache.py
```

No `PKGCACHE_TLS_CERT` → plain HTTP (curl without -k). Per-project admin surface:
`http://127.0.0.1:18443/<project>/<role>/healthz` (and `/+progress`, `/+ledger/…`).
`PKGCACHE_PROJECT_POLL=1` makes registry changes apply within ~1s.
`PKGCACHE_SPEEDTEST_URL=` (empty) keeps it from touching upstream on its own.

## webui (stdlib control API)

```bash
cd webui && PKGCACHE_PROJECTS=/tmp/verify/projects.json \
  UI_HOST=127.0.0.1 UI_PORT=18088 python3 server.py
```

Shares the registry file with pkgcache — a webui write is picked up by the pkgcache
supervisor on its next poll. `docker compose ps` calls in reads.status() hit the
real stack (read-only, harmless); the livefeed's `pkgcache` hostname won't resolve
locally, so /api/proxies health shows roles down — expected.

## console (React)

No node on PATH. Typecheck via docker against the local node_modules:

```bash
cd webui/console && docker run --rm -v "$PWD":/w -w /w node:20-alpine \
  sh -c "node_modules/.bin/tsc --noEmit"
```

Driving the UI itself needs the full compose stack (console nginx proxies /api to
the webui container) — not available as an isolated local run.

## Gotchas

- Test suites: `cd pkgcache && .venv-test/bin/python -m pytest tests/ -q` and
  `cd webui && python3 -m unittest discover -s tests -q` (CI territory, not
  verification evidence).
- `grep` is aliased to ugrep in this shell and intermittently mis-handles some
  absolute-path multi-file invocations; retry with `cd` + relative paths if it
  claims files don't exist.
