# Phase 8 — air-gap

Status: **complete** (2026-07-27).

Phase 8 replaces the Python/DVC/git-bundle transfer workflow with streamed native
formats over the shared content-addressed store.

## Delivered

| Plan item | Evidence |
|---|---|
| P8-01 manifests | Deterministic gzip TSV reader/writer; strict `(eco,key)` order; 100,000-entry streamed round trip |
| P8-02 checkpoint | Catalog rows stream directly into a manifest blob with no cache tree walk or quiesce; the 32,000-row/119 GiB logical fixture completes in 126 ms |
| P8-03 rollback | Blob existence and size are verified before an atomic entry/HEAD replacement; stale refs and derived inventory are cleared |
| P8-04 diff/export | Linear entry diff plus a disk-backed SQLite digest set; uncompressed tar members stream without retaining blobs in memory |
| P8-05 import | Every manifest/blob is checked against its declared SHA-256 and size; lineage and the fast-forward base are checked again in the apply transaction |
| P8-06 lockwarm | Schema-v1 `uv.lock` parser, bounded in-process PyPI warming, URL-only rewrite, atomic output, and downloadable rewritten lock |
| P8-07 round trip | A second application instance imports a full pack and receives byte-identical catalog entries and managed bare-Git files; delta import and rollback restore the base tree |

Bare Git mirrors are the documented managed-directory exception to catalog-only
checkpointing. Each mirror is streamed into a deterministic tar blob under a
reserved manifest key. Temporary Git lock/pack files are excluded, and rollback or
import stages and swaps the complete managed project tree around the transactional
catalog apply.

Transfer packs use the v1 wire layout:

```text
pack.json
snapshots/<id>.manifest.gz
blobs/<aa>/<sha256>
certs/{ca.crt,server.crt,server.key}   # global only; never ca.key
```

Full and delta exports land in `shuttle/out`; imports read `.tar` packs from
`shuttle/in`. `pkgreg checkpoint` (also `snapshot`), `rollback`, `export`, `import`
and `lockwarm` run the same registered operation services as API jobs, support
cancellation, and offer JSON progress.

## Acceptance coverage

- 100,000-entry manifest round trip and malformed/unsorted rejection;
- 32,000 catalog rows representing 119 GiB checkpointed in 126 ms;
- exact target-minus-base digest selection with cross-key deduplication;
- corrupt content rejected before publication under the declared digest;
- early and transactional non-fast-forward refusal;
- checkpoint → rollback → export → second-host import, including a bare Git mirror;
- real `uv 0.11.19` accepts the rewritten lock with both `sync --frozen` and
  `sync --locked`;
- durable job registration and legacy shuttle/lockfile compatibility.

Verification:

```text
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
go mod verify
```
