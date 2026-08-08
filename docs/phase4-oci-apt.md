# Phase 4 — OCI and apt/apk

**Date:** 2026-07-27 · **Verdict: COMPLETE**

Phase 4 adds the OCI Distribution adapter and the shared apt/apk forward-proxy
adapter on top of the Phase 2 engine and Phase 3 ecosystem framework.

## Delivered

| Plan item | Evidence |
|---|---|
| P4-01 OCI pull protocol | `/v2/`, tag/digest manifests, blobs, and `tags/list`; a real Docker daemon pulls the recorded image |
| P4-02 multi-arch inventory | Index children are associated with the parent tag; the selected child back-fills layer + config bytes without a duplicate digest-version row |
| P4-03 offline OCI | Tag refs resolve without an origin; offline `tags/list` is generated from refs and restores the project-prefixed image name |
| P4-04 apt forward proxy | Absolute-form HTTP targets are preserved; any-host relay is the default and `server.proxy_allowlist` supports exact hosts and `*.` subdomains |
| P4-05 apt freshness | `InRelease`, `Release`, `Packages*`, `Sources*`, and `Contents*` conditionally revalidate through refs; package and by-hash URLs are immutable |
| P4-06 apk + inventory | `APKINDEX.tar.gz` revalidates; `.deb`, `.udeb`, and `.apk` names, versions, architectures, and formats enter the catalog |
| P4-07 real clients | Recorded fixtures pass a multi-arch `docker pull`, `apt-get update && install`, and `apk add` |

The apt client run found and fixed one integration defect: a missing upstream
`InRelease` was initially converted from 404 to 502. Upstream status errors now
retain their HTTP code, allowing apt to perform its normal `InRelease` → `Release`
fallback and allowing OCI to return registry-compatible not-found responses.

## Permanent coverage

- `go/internal/eco/oci/oci_test.go` covers bearer auth, tag/digest linking, verified
  blobs, multi-arch size back-fill, offline refs, project-prefixed `tags/list`, and
  digest mismatch rejection.
- `go/internal/eco/apt/apt_test.go` covers absolute targets, immutable `.deb`/`.apk`
  caching, inventory parsing, ETag revalidation, offline replay, allowlist rejection,
  escaping, and exact upstream status relay.
- `go/test/acceptance/clients_test.go` drives the real clients. Debian and Alpine
  clients run from pinned images; origins and installable package content are local
  fixtures.

## Verification

```bash
cd go
go test ./... -count=1
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build ./...
go mod verify
```

All commands passed. Phase 5 subsequently completed the Git adapter and qualified
M4; see the [Phase 5 report](phase5-git.md).
