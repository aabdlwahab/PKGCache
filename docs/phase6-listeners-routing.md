# Phase 6 — Listeners and routing

**Date:** 2026-07-27 · **Verdict: COMPLETE**

Phase 6 turns the six independently tested adapters into the production data plane:
one shared engine and catalog, project-aware dispatch, real listeners, reloadable
TLS, and bounded graceful shutdown.

## Delivered

| Plan item | Evidence |
|---|---|
| P6-01 project resolution | `/<project>/<eco>/...`, OCI image-name prefixes, and apt/apk proxy usernames resolve against the live immutable config snapshot; an explicitly requested unknown project returns a helpful 404 instead of global content |
| P6-02 unified namespace | `/v2/` dispatches OCI; fully qualified ecosystem paths dispatch the other adapters; `/`, `/healthz`, `/readyz`, `/metrics`, and `/version` are served without namespace collision |
| P6-03 listener modes | Explicit mode binds unified, proxy, and admin sockets; default single-port mode peeks the first byte, sends TLS records to unified HTTP, and sends plain absolute-form requests to apt/apk |
| P6-04 TLS reload | TLS 1.2+, SNI validation, and an atomic `GetCertificate` pointer are wired; `SIGHUP` reloads both files only after successful parsing, preserving the prior pair on failure |
| P6-05 graceful drain | Readiness becomes false before shutdown; every HTTP server drains active handlers, then the engine waits for detached shared fetches; expiry closes remaining connections |

`pkgreg serve` now starts the production listeners rather than only the admin
surface. In single-port mode the same socket serves TLS data/operations and plain
apt/apk proxy traffic. In explicit mode the configured addresses remain available
for deployments that prefer separate firewall rules.

## Permanent coverage

- `go/internal/app/dataplane_test.go` covers global and named path routing, OCI
  project stripping and name restoration, live project publication, proxy usernames,
  unknown-project isolation, encoded npm scopes, CONNECT refusal, and unified help.
- `go/internal/app/listeners_test.go` uses real TCP sockets for single-port TLS/plain
  classification and explicit mode, validates SNI against the native CA, retains an
  established TLS connection across a certificate swap, rejects a malformed reload
  without losing the good certificate, and proves shutdown blocks until a streamed
  download completes.
- `go/internal/engine/inflight.go` supplies the process-level fetch drain barrier,
  covering work intentionally detached from a client that disconnected.

## Verification

```bash
cd go
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
go mod verify
```

All commands passed.
