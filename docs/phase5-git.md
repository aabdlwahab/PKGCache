# Phase 5 — Git

**Date:** 2026-07-27 · **Verdict: COMPLETE**

Phase 5 adds a read-only smart-HTTP Git ecosystem backed by managed bare mirrors,
plus Git LFS objects backed by the shared content-addressed store.

## Delivered

| Plan item | Evidence |
|---|---|
| P5-01 managed storage | Mirrors land at `managed/git/<project>/<host>/<path>.git`; decoded dot segments, invalid hosts, separators, and traversal are rejected |
| P5-02 mirror lifecycle | Atomic first clone, heads/tags-only fetch and prune, upstream HEAD synchronization, per-repository writer lock, `gc.auto=0`, and `maintenance.auto=false` |
| P5-03 upload-pack | `exec.CommandContext` streams to the response, stderr is retained as a 4 KiB tail, negotiation bodies are capped at 64 MiB, and an eight-process semaphore bounds pack generation |
| P5-04 smart HTTP | Advertisements and protocol v2 work; real full, shallow, `--filter=blob:none`, SHA-pinned, and offline clones pass; receive-pack is refused and dumb HTTP returns 404 |
| P5-05 Git LFS | Download-only batch negotiation returns cache URLs; `+lfs/{oid}` uses engine integrity checking, Range support, single-flight, and cross-ecosystem CAS deduplication |
| P5-06 maintenance/catalog | `+maintain` runs geometric repack and `pack-refs` under the writer lock; current heads/tags populate refs and inventory, with mirror bytes counted on exactly one row |

The outbound boundary now supports replayable small POST bodies and bounded,
non-cacheable exchanges. This is used only for LFS batch negotiation; LFS object
content still goes through the normal streaming cache engine.

## Permanent coverage

`go/internal/eco/git/git_test.go` drives:

- a real Git client through full, depth-one, protocol-v2 partial, SHA-pinned, and
  offline clone/fetch paths;
- real `git lfs pull`, followed by an offline pull with no second object-origin hit;
- branch/tag pruning, default-branch synchronization, catalog ref cleanup,
  single-count mirror inventory size, and geometric maintenance;
- a twelve-caller refresh burst proving one serialized origin fetch;
- upload-pack concurrency and cancellation with a post-return PID existence check;
- managed-path traversal rejection, gzip negotiation, body limits, protocol errors,
  and LFS reuse of a blob introduced by another ecosystem.

## Verification

```bash
cd go
go test ./... -count=1
go test -race ./...
go vet ./...
go build ./...
go mod verify
```

All commands passed. M4 is qualified: all six ecosystem shapes now have real-client
coverage.
