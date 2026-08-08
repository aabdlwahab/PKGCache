# Phase 9 — scale features

Status: **complete** (2026-07-27).

Phase 9 adds bounded-storage operation and digest-first federation without weakening
the cache's concurrent streaming guarantees.

## Delivered

| Plan item | Evidence |
|---|---|
| P9-01 online GC | Grace-based sweep over the CAS, a final flushed catalog reference check under the blob lifecycle lock, and catalog cleanup after unlink |
| P9-02 eviction | Global or project-scoped LRU, optional TTL and filesystem free-space floor, size target, dry run, and checkpoint-manifest pin traversal |
| P9-03 quotas | Entry and optional artifact publication plus byte/artifact accounting in one SQLite writer transaction; rejected PUTs return 507 with usage, limit, and attempted usage |
| P9-04 peers | Authenticated `HEAD`/`GET /peer/v1/blob/{sha256}` with Range, batch `GET /peer/v1/have`, verified streaming downloads, per-digest single-flight, and peer-before-offline/origin engine ordering |
| P9-05 scheduling/CLI | Interval scheduler plus durable `gc` and `evict` job runners; `pkgreg gc` and `pkgreg evict` expose the same path with `--dry-run` |
| P9-06 observability | Maintenance/job metrics wired and a reference dashboard covering hit rate, bytes saved, in-flight work, store size, latency, and reclaimed bytes |
| P9-07 rate limits | Live per-project and per-token requests/second and burst settings backed by isolated in-memory token buckets; 429 includes `Retry-After` |

The CAS lifecycle lock is shared by three operations: publishing or reusing a
digest, linking it into the catalog, and final maintenance deletion. Reusing an old
digest refreshes its grace timestamp. This closes the otherwise subtle case where a
new writer sees an existing old inode just as GC decides it is an orphan.

Deleting a named project removes its entries, artifacts, refs, statistics, heads,
and checkpoints before unregistering it. GC then reclaims only exclusive content;
another project's entry preserves shared bytes.

## Operator surfaces

Scheduled policy is configured under `maintenance`:

```yaml
maintenance:
  gc_interval: 6h
  gc_grace: 1h
  evict_interval: 30m
  evict_target_bytes: 0      # zero disables this trigger
  evict_min_free_bytes: 0    # zero disables this trigger
  evict_ttl: 0s              # zero disables TTL eviction
```

Manual operations:

```text
pkgreg gc --grace 1h --dry-run
pkgreg evict --target-size 536870912000 --min-free 10737418240 --ttl 720h --dry-run
pkgreg evict --project team-a --ttl 2160h
```

Project policy is updated through `PATCH /api/v1/projects/{project}`:

```json
{
  "quota_bytes": 536870912000,
  "quota_artifacts": 100000,
  "rate_limit": 100,
  "rate_burst": 200
}
```

`POST /api/v1/tokens` accepts `rate_limit` and `rate_burst`; a non-zero token limit
overrides the project limit for that credential.

To federate, create a global, ecosystem-empty token with scope `peer`, then add a
project upstream with `kind: "peer"` and a bearer credential containing that token.
The peer URL is the sibling's listener origin; pkgreg appends `/peer/v1/...`.

The reference dashboard is
[`docs/grafana/pkgreg.json`](grafana/pkgreg.json).

## Acceptance coverage

- deleting one project reclaims its exclusive blob while a cross-project shared blob
  survives;
- retained manifest and content blobs survive both maintenance policies;
- LRU removes the oldest exclusive blob and reaches the configured size target;
- GC dry run reports candidates without mutation;
- twelve concurrent commits against a ten-byte quota admit exactly one seven-byte
  entry;
- quota errors render as HTTP 507 with current and attempted usage;
- an offline engine obtains known-digest content from its peer before considering
  the origin;
- two independent stores transfer verified content through the authenticated peer
  protocol, with Range and batch-have coverage;
- token buckets enforce burst, refill, unlimited mode, and per-token isolation;
- full suite and targeted race detector pass.

Verification:

```text
go test ./...
go test -race ./internal/blob ./internal/catalog ./internal/maintenance \
  ./internal/upstream/peer ./internal/app ./internal/engine
go vet ./...
```
