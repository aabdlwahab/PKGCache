# Spike S2 — single-flight and progressive delivery

**Date:** 2026-07-27 · **Verdict: PASS.** All six properties hold, `-race` clean.
Now permanent tests in `internal/engine/engine_test.go` (`TestS2*`).

## Why this spike existed

This is risk **R2** — the only item rated *Critical* in the plan's register. A bug in
this code path does not fail a request; it silently corrupts a multi-gigabyte
artifact, and every build host downstream inherits it.

The mechanism is inherently racy by design: one goroutine writes a file while N
readers read the same file behind it, and the writer may fail at any point.

## The design under test

One goroutine streams upstream into a staging file, hashing inline. Readers
tail-follow it via `ReadAt`, which is safe concurrently because it does not touch a
shared file offset. State changes broadcast by **closing and replacing a channel** —
`close()` wakes every waiter at once, which is what `sync.Cond` would give us with
more ceremony and no `context` support.

Three decisions carry most of the risk:

- **The fetch goroutine runs on `context.WithoutCancel`.** A client pressing Ctrl-C
  must not abort a transfer that nine other readers and the cache still want.
- **A shared read descriptor with a reference count**, rather than one `os.Open` per
  reader. That removes the attach-exactly-at-commit race entirely instead of handling
  it, which is the right trade when the failure mode is corruption.
- **Verification happens before publication.** The digest and length are checked
  against `Expect` and against the declared `Content-Length` *before* `Commit`, so bad
  bytes are never linked into the store and never get a catalog row.

## Results

| Property | Test | Result |
|---|---|---|
| N concurrent readers → exactly 1 upstream fetch | `TestS2SingleFlightManyReaders` | 20 readers, 2 MiB, 1 fetch, all byte-identical |
| Client disconnect does not abort the fetch | `TestS2ClientDisconnectDoesNotAbortFetch` | cancelled mid-stream; fetch completed, content cached, next request a hit |
| Mid-stream upstream failure reaches every reader | `TestS2UpstreamFailureMidStream` | 8 readers all saw failure; no catalog entry; no registry leak |
| Delivery is genuinely progressive | `TestS2ProgressiveDeliveryIsActuallyProgressive` | first bytes received while `done == false` |
| Reader attaching after commit still succeeds | `TestS2ReaderAttachingAfterCommit` | falls back to the committed blob |
| Distinct keys are not collapsed | `TestS2DistinctKeysFetchIndependently` | 5 keys, 5 fetches |
| M2 scale qualification | `test/load/TestM2TwentyClientsSingle2GiB` | 20 readers × 2 GiB, byte-identical SHA-256, 1 origin request ([report](../load/m2-2gb.md)) |

Integrity, from the same suite:

| Failure injected | Outcome |
|---|---|
| Corrupt bytes against a known digest | `ErrDigestMismatch`; nothing published; a later good fetch succeeds cleanly |
| Body shorter than declared `Content-Length` | `ErrSizeMismatch`; no catalog entry |
| Upstream 404/500 | `ErrUpstreamStatus`; no entry |
| Blob deleted under a live catalog row | Row dropped, content re-fetched — self-heals rather than 404s |

## The one accepted limitation

Progressive delivery streams bytes to clients *before* the digest can be verified,
because verification needs the last byte. If upstream serves corrupt content, readers
attached during that fetch receive corrupt bytes.

This is inherent to progressive delivery and the previous Python implementation had
the same property. It is acceptable because the cache is not the only line of
defence: `pip` verifies wheel hashes, `docker` verifies layer digests, and `apt`
verifies package checksums, all independently. What the cache guarantees is that the
bad bytes are **never persisted and never served again** — the next request re-fetches
cleanly, which is the property the tests assert.

## Follow-ups

- Run the broader verification-strategy soak (1,000 concurrent readers, 24 h,
  watching RSS and goroutine count). M2's separate 20-reader × 2 GiB criterion has
  passed.
- `TestS2ClientDisconnectDoesNotAbortFetch` polls for completion with a 10 s
  deadline. If it ever flakes in CI, the fix is an event-bus subscription rather than
  a longer deadline.
