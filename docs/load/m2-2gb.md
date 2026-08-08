# M2 — 20-reader, 2 GiB qualification

**Date:** 2026-07-27 · **Verdict: PASS**

This run qualifies milestone M2 from the Go implementation plan: 20 concurrent
clients pulled one 2 GiB (2,147,483,648-byte) synthetic wheel while the origin
received exactly one request.

## Result

| Check | Observed |
|---|---:|
| Concurrent clients | 20 |
| Bytes verified per client | 2 GiB |
| Aggregate client bytes | 40 GiB |
| Client-phase elapsed time | 7.639 s |
| Aggregate throughput | 5.24 GiB/s |
| Peak sampled Go heap | 0.01 GiB |
| Origin requests | 1 |
| Committed blobs | 1 × 2 GiB |

Every client hashed its complete response and matched the independently computed
SHA-256. The final catalog entry also matched the expected digest and size. The
test hashes and discards client responses instead of buffering them, so the heap
stays bounded while the full 40 GiB passes through the reader side.

The complete test process took 12.84 s, including generation of the expected hash,
setup, the measured client phase, assertions, and temporary-store teardown.

## Reproduce

The qualification is opt-in so a normal `go test ./...` does not unexpectedly move
40 GiB:

```bash
cd go
go test -v ./test/load \
  -run '^TestM2TwentyClientsSingle2GiB$' \
  -count=1 \
  -args -m2-2gb
```

The permanent harness is `go/test/load/m2_test.go`. It uses a deterministic
streaming origin and a temporary blob store; it neither downloads an external
artifact nor retains the 2 GiB blob after the run.

The broader 1,000-reader, 24-hour soak in the verification strategy remains a
separate later scale test; it is not part of the M2 acceptance criterion.
