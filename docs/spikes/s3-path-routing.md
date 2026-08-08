# Spike S3 — path routing and escaping

**Date:** 2026-07-27 (Go 1.26.3) · **Verdict: PASS, with a correction.**
The custom router is still required, but for **two** reasons rather than the three
originally claimed — and one new reason the measurement surfaced.

## Why this spike existed

The design asserted that `http.ServeMux` could not serve this project and specified a
custom router on that basis. That claim was written from memory of older Go
behaviour. This spike tests it against the standard library actually in use.

## What was measured

Every case below is now a permanent assertion in
`internal/router/mux_test.go:TestServeMuxLimitations`, run against the live
`net/http`. If a future Go release changes any of them, the test fails and the
decision gets re-examined rather than the comment going quietly stale.

| Case | Claimed | Measured (Go 1.26.3) | Verdict |
|---|---|---|---|
| npm `/@babel%2Fcore` | ServeMux splits on the encoded slash and 301-redirects | **Handled correctly.** One segment; `PathValue` returns `@babel/core`; no redirect | ❌ **claim was wrong** |
| OCI `/v2/{name...}/manifests/{ref}` | `{x...}` must be terminal | **Panics at registration:** `{...} wildcard not at end` | ✅ holds |
| apt `/a/../b`, `/a//b`, `/x/./y` | Cleans and redirects | **307** to `/b`, `/a/b`, `/x/y` | ✅ holds |
| `%2B` vs literal `+` | *(not considered)* | **Indistinguishable** — both reach `PathValue` as `+` | ⭐ **new reason** |

## Conclusion

**One of three stated reasons was wrong.** Modern `ServeMux` handles encoded slashes
properly, so npm scoped packages are not a problem for it.

**Two reasons hold, and either is sufficient on its own:**

- OCI's route shape is *inexpressible* — not merely awkward. `ServeMux` panics when
  the pattern is registered, so there is no workaround short of hand-parsing the path
  inside a catch-all handler, which is the custom router with worse ergonomics.
- The apt forward proxy must relay a path byte-for-byte. A 307 to a cleaned path is
  the wrong answer to `GET http://archive.ubuntu.com/a//b`.

**A fourth reason emerged that is arguably the most important long-term.**
`PathValue` only ever yields the *decoded* segment, so `%2B` and a literal `+` are
indistinguishable to a handler. A cache that reconstructs upstream URLs cannot work
from a lossy view of what the client asked for. Our `Params` therefore returns the
**raw** segment and makes decoding explicit via `Unescape`.

## The router

~230 lines, `internal/router/mux.go`. Matches on `r.URL.EscapedPath()`; never cleans,
never redirects.

- literal segments, `{name}` (one segment, raw), `{name...}` (greedy, raw)
- **the greedy wildcard may be non-terminal**, anchored by the literal suffix after
  it. With at most one per pattern, matching is linear with no backtracking: consume
  the fixed prefix from the front, the fixed suffix from the back, and the middle is
  the capture
- suffix anchoring is from the *end*, so an image legitimately named
  `org/manifests` still resolves in `/v2/org/manifests/manifests/v1`
- registration order decides, so `/+indexes` beats `/{index...}/+simple/{project}/`
- a trailing slash is significant (PyPI's `/simple/{project}/` is a distinct resource)
  except after a terminal greedy, which must serve both a directory and a file
- a malformed pattern panics at startup — a programming error, not a runtime 404

`BenchmarkLookup` over a five-route table: matching is not on any hot path worth
optimising further.

## Bug found and fixed

The root pattern `/` did not match `/`. `splitPath("/")` reports the path as
trailing, but `compile` derived `trailingSlash` with `len(pattern) > 1`, so the root
route claimed to be non-trailing and the comparison failed. Fixed, with
`TestRootPath` covering it.

## Documents corrected

The wrong claim appeared in three places and has been amended in all of them, each
noting what was measured:

- `docs/go-architecture.md` §3.1
- `docs/go-implementation-plan.md` §3.3
- `docs/language-choice.md` §2.1
