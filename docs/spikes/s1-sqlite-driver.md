# Spike S1 — pure-Go SQLite under load

**Date:** 2026-07-27 · **Verdict: PASS.** `modernc.org/sqlite` is adequate with
2–4× margin. No CGO fallback needed.

## Why this spike existed

`modernc.org/sqlite` is a machine translation of SQLite's C source into Go. It is
what keeps the binary CGO-free and genuinely static, and it is the one place
[language-choice.md](../language-choice.md) conceded Rust would be better —
`rusqlite` statically links the real C SQLite at full speed.

The bet was that the difference is irrelevant at this workload's scale. The measured
production cache holds **32,080 files**, so even 100× growth is ~3M rows. This spike
tests that bet at 100,000 entries — 3× headroom over 100× the real cache.

If it failed, the fallback was CGO + `mattn/go-sqlite3` linked statically against
musl: still one binary, just not CGO-free.

## Method

`internal/catalog/spike_s1_test.go`, run on the target host (112 cores, Rocky 10,
Go 1.26.3, NVMe). The LRU is disabled (`CacheSize: 1`) so the numbers measure SQLite
itself rather than the cache in front of it.

The file carries `//go:build !race`. The race detector instruments every memory
access and costs 10–20×; the same suite measured **1,223 inserts/s under `-race`**
against 15,396 without. Performance thresholds are meaningless under instrumentation,
so they are excluded from race builds while the correctness tests in
`catalog_test.go` still run under `-race`.

## Results

| Measure | Threshold | Measured | Margin |
|---|---|---|---|
| Batched insert rate | ≥ 5,000 rows/s | **15,396 rows/s** | 3.1× |
| Point lookup p50 (100k rows) | — | **24.7 µs** | — |
| Point lookup p99 (100k rows) | < 200 µs | **47.7 µs** | 4.2× |
| Aggregate over 100k rows | < 50 ms | **21.8 ms** | 2.3× |

Microbenchmarks:

```
BenchmarkGetEntryCached-112        137.3 ns/op    # LRU hit — never reaches SQLite
BenchmarkGetEntryUncached-112    27,360   ns/op
BenchmarkPutEntryBatched-112      3,111   ns/op
```

**WAL concurrency:** 431 read queries completed while 2,000 rows were being written,
with all 2,000 durable afterwards. Readers genuinely run alongside the single writer,
which is what justifies the two-pool design (one writer connection, a pool of
readers) rather than serialising everything through `MaxOpenConns(1)`.

## What this means

The hot path for a cache hit is one entry lookup. At **137 ns** through the LRU and
**27 µs** through SQLite, neither is remotely close to being the bottleneck: pulling
a 2.5 GB wheel over a 1 Gbps link takes ~20 seconds, so the lookup is roughly one
part in a million of the request.

This retires the last reservation about Go for this project. The driver's slower
per-query cost is real and simply does not matter at this cardinality.

## Follow-ups

- Re-run if the catalog ever approaches 10M entries (~300× the current cache).
- The thresholds are enforced as test assertions, not documentation, so a regression
  fails CI rather than being discovered in production.
