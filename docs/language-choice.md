# Language choice for pkgreg — analysis

Written 2026-07-27. Companion to [go-architecture.md](go-architecture.md).

**Verdict: Go.** Not by a landslide on any single axis, but it wins the axes that
actually bind here and loses only on ones that measurement shows are irrelevant to
this workload. Rust is the credible runner-up and would cost roughly 2× the schedule
for benefits this system cannot use.

This document exists so the decision is defensible later, and so the one place Go is
genuinely weaker is written down with its mitigation rather than discovered in
month four.

---

## 1. What actually decides it

The wrong way to pick is "which language is fastest". The right way is to characterise
the workload, find which language properties are load-bearing, and score only those.

### 1.1 Measured workload profile

From the live cache in this repo:

| Property | Measured | Implication |
|---|---|---|
| Cache size | **119 GB** | Storage-layout and GC decisions matter; language does not |
| Largest artifacts | **2.5 GB** (torch CUDA wheels) | Must stream, never buffer. Memory discipline is load-bearing |
| Total cached files | **32,080** | Catalog is *small* — tens of thousands of rows, not millions |
| Per-file work | stream + sha256 + one row insert | I/O-bound, not CPU-bound |
| Concurrency shape | N readers tail-following one in-flight download | Cheap concurrency primitives are load-bearing |
| Deployment target | bare metal, air-gapped, one binary | Static linking + cross-compilation are load-bearing |

Two of these overturn assumptions I made in the earlier plan:

**The catalog is tiny.** 32 k entries — not the millions I assumed. Even 100× growth
is 3.2 M rows. This all but eliminates SQLite driver performance as a decision factor
(§5.1), which had been my main reservation about Go.

**Storage is already being wasted 2× by DVC.** The 2.5 GB torch wheel exists as inode
`3054130950` with `nlink=2` (the pip tree and `.cas` correctly share it) — but DVC's
object store holds a *separate* inode with `nlink=1`. Every checkpointed artifact is
stored twice, because the online checkpoint deliberately uses `cache.type=reflink,copy`
with hardlinks disabled to protect the live `ledger.db`
([`operations.py:190`](../webui/app/services/operations.py#L190)). A large fraction of
that 119 GB is duplicate. This is an architecture problem, not a language problem —
but it confirms where the real wins are.

### 1.2 Is this ever CPU-bound? No.

sha256 with hardware acceleration (SHA-NI on amd64, crypto extensions on arm64) runs
around 2 GB/s per core in Go, Rust, C and C# alike — they all reach for the same CPU
instructions.

Hashing the 2.5 GB wheel costs **~1.25 s of one core**. Pulling it over a 1 Gbps
uplink takes **~20 s**. Hashing is ~6 % of transfer wall time, on one core, while the
other cores idle.

Serving a cache *hit* is `sendfile` — the kernel copies; the language is not in the
data path at all.

**Therefore raw execution speed cannot be the deciding criterion.** Any language that
can saturate 10 GbE is fast enough, and all serious candidates can. What decides it is
deployment, concurrency ergonomics, ecosystem fit, and development velocity.

---

## 2. Candidates and elimination

| Language | Verdict | Reason |
|---|---|---|
| **Go** | **selected** | see §3–§4 |
| **Rust** | serious runner-up | see §5 |
| **C# / .NET NativeAOT** | eliminated | §2.1 |
| **Java/Kotlin + GraalVM** | eliminated | §2.2 |
| **Zig** | eliminated | pre-1.0 language, immature HTTP/TLS stack. Irresponsible for an ops-critical daemon |
| **C / C++** | eliminated | memory-unsafe code parsing untrusted registry data on a network port; no upside here |
| **Nim / Crystal / D** | eliminated | tiny ecosystems for OCI/package formats; single-digit hiring pool; bus-factor risk on an infra service |
| **OCaml** | eliminated | genuinely good fit technically (static binaries, fast, safe) but near-zero library coverage for OCI/Debian/RPM and effectively unhireable |
| **Elixir / BEAM** | eliminated | best-in-class concurrency and supervision, but no static binary (needs ERTS), and byte-shuffling throughput is its weakest axis — which is this system's entire job |
| **Node / Bun single-executable** | eliminated | Bun `--compile` does produce one file, but a JS runtime for a long-lived streaming daemon means high RSS, GC pauses under multi-GB streams, and a weak systems-programming story |
| **Python (Nuitka/PyInstaller)** | eliminated | §2.3 |

### 2.1 Why not C# / .NET NativeAOT

Genuinely strong on the merits: `System.IO.Pipelines` is purpose-built for streaming
proxies, Kestrel is very fast, `async`/`await` is mature, and SQLite bundles natively.
It fails on deployment, which is the stated requirement:

- **TLS uses the host's OpenSSL on Linux.** The single-binary, zero-dependency goal
  dies immediately — and on an air-gapped RHEL box, "which OpenSSL" becomes a support
  question.
- **Cross-compilation with NativeAOT is not supported** across OS/architecture. You
  must build on the target platform. Go cross-compiles to linux/arm64 from a laptop
  with an environment variable.
- NativeAOT restricts reflection and dynamic loading, which constrains the plugin
  direction in §6.
- ASP.NET Core has a long history of trouble with encoded slashes in routing — and
  npm's `/@babel%2Fcore` is a hard requirement here. (Go's own `ServeMux` was
  measured on this during phase 3 and handles it correctly; the custom router
  exists for non-terminal greedy wildcards and byte-faithful proxy paths instead.)

### 2.2 Why not GraalVM native-image

Build times in minutes, memory-hungry builds, reflection configuration maintenance,
50–100 MB binaries, and no cross-compilation. The JVM's strengths (JIT peak
throughput, mature concurrency) are exactly the ones §1.2 shows are not needed.

### 2.3 Why not stay in Python and just fix the architecture

Worth taking seriously — it is by far the cheapest option, and **every architectural
improvement in [go-architecture.md](go-architecture.md) is language-independent.** The
blob store, the unified catalog, the `Ecosystem` interface, GC, eviction and
manifest-based snapshots could all be built in Python.

What it cannot deliver:

- **No single static binary.** Nuitka/PyInstaller produce a self-extracting bundle
  that still carries the interpreter, and DVC's native dependencies do not bundle
  cleanly. The explicit goal — `scp pkgreg host: && systemctl start` — is unreachable.
- **The GIL** under hundreds of concurrent multi-GB streams with inline hashing. The
  current code already works around this: the ledger hops to a one-thread executor and
  *busy-polls it every 1 ms* ([`ledger.py:428`](../pkgcache/src/pkgcache/core/ledger.py#L428))
  because the async↔thread handoff was unreliable.
- **The air-gap contradiction.** This project exists to spare other people from
  dependency trees, while shipping Python + DVC + `pip` to every air-gapped host.

If the goal were *only* the architecture, staying in Python would be right. The goal
includes the deployment model, so it is not.

---

## 3. Go vs Rust on the criteria that bind

| Criterion | Weight | Go | Rust |
|---|---|---|---|
| Static binary, zero runtime deps | **critical** | `CGO_ENABLED=0 go build` → truly static, no libc | musl target; static achievable, one extra step |
| Cross-compilation | **critical** | `GOOS/GOARCH`, no toolchain setup | needs `cross`/Docker or a cross-linker |
| Air-gapped build (vendored deps, offline toolchain) | **critical** | `go mod vendor`; toolchain is one self-contained tarball | `cargo vendor`; toolchain via rustup, larger but workable |
| Concurrency ergonomics for tail-follow streaming | **high** | goroutines + `sync.Cond`/channels — the canonical use case | `Arc<Mutex<…>>` + `tokio::sync::Notify`; correct but materially more design effort |
| Range/conditional serving of large files | **high** | `http.ServeContent` — Range, If-Range, If-None-Match, multipart, free | `tower-http` or hand-rolled; less batteries-included |
| Forward-proxy absolute-form targets | **high** | `net/http` populates `r.URL.Host` natively | `hyper` gives raw access; fine |
| Raw/escaped path control (`%2F`, `%2B`) | **high** | `r.URL.EscapedPath()` + a ~230-line custom mux | `http::Uri` preserves raw path |
| TLS without OpenSSL | **high** | `crypto/tls` + `crypto/x509` CA minting | `rustls` + `rcgen`; arguably better security posture |
| Embedded SQLite | medium | ⚠️ pure-Go transpilation, or CGO | ✅ `rusqlite` bundles real C SQLite |
| OCI / package-format libraries | **high** | reference implementations *are* Go | thinner, less canonical |
| Subprocess streaming + cancel (`git upload-pack`) | medium | `exec.CommandContext` → ResponseWriter | `tokio::process`, `kill_on_drop` |
| Memory determinism | low *(see §1.2)* | GC; negligible with pooled buffers | no GC; exact |
| Peak throughput ceiling | **irrelevant** | saturates 10 GbE | saturates 10 GbE |
| Development velocity | **high** | high | ~2× slower for async streaming code |
| Hiring / onboarding for an ops tool | **high** | days | weeks–months for async Rust |

The three `critical` rows all favour Go, and the two `irrelevant`/`low` rows are where
Rust's advantages sit.

---

## 4. Why Go wins here specifically

### 4.1 The deployment requirement is the whole point

The user's requirement is *"runnable baremetal without containers through one
executable for easy deployment."* Go's static-binary and cross-compilation story is
the best in the industry, and it is not close: no libc, no linker configuration, no
cross toolchain, `GOARCH=arm64` and you have an ARM build.

For an **air-gapped** project this compounds. `go mod vendor` produces a fully
self-contained source tree, and the Go toolchain is a single tarball with no
dependencies — so the build host inside the gap needs nothing else. That property is
directly aligned with what this product sells.

### 4.2 This is Go's domain, with unusually direct precedent

Every comparable system is Go, including the two this project replaced:

| Tool | Language |
|---|---|
| **zot** (the OCI registry this project replaced) | Go |
| distribution/distribution (Docker Registry) | Go |
| Harbor | Go |
| Athens (Go module proxy) | Go |
| Kraken, Dragonfly (P2P image distribution) | Go |
| **Gitea package registry** | Go |

**Gitea is the strongest evidence.** Its package registry implements ~20 ecosystems —
Alpine, Cargo, Composer, Conan, Conda, Container, CRAN, Debian, Go, Helm, Maven, npm,
NuGet, Pub, PyPI, RPM, RubyGems, Swift, Vagrant — behind a shared interface. That is
precisely the `Ecosystem`/`Descriptor` model proposed in
[go-architecture.md §3](go-architecture.md), demonstrated at scale, in Go, in
production. The extensibility design is not speculative.

Practically, this means the OCI libraries (`opencontainers/*`,
`google/go-containerregistry`), Helm's own library, `golang.org/x/mod` for Go modules,
and Debian/RPM parsers are all available and canonical. In Rust these exist but are
second-hand ports.

### 4.3 The hardest code in the system is the easiest thing in Go

Single-flight with N tail-following readers over a growing file is the one genuinely
tricky concurrency problem here. In Go it is a struct, a mutex, and a
closed-and-replaced broadcast channel — a few dozen lines, verifiable under `-race`.

In async Rust the same thing means an `Arc<Download>` shared across tasks, interior
mutability, a `Notify`/`watch` broadcast, and careful work to expose it as a `hyper`
body without lifetime friction. All tractable, all well-trodden — and all several
times the design and review effort, on the code path where a bug corrupts a 2.5 GB
wheel.

### 4.4 `http.ServeContent` is worth more than it looks

The original defect that motivated replacing devpi was *no Range support, so multi-GB
torch wheels were re-downloaded every install*
([`pypi.py:3`](../pkgcache/src/pkgcache/handlers/pypi.py#L3)). `ServeContent` handles
Range, `If-Range`, `If-None-Match`, `If-Modified-Since` and multipart ranges correctly,
in the standard library, with `sendfile` underneath. In Rust that is a dependency or
hand-written code — and hand-written HTTP range handling is a classic source of subtle
bugs.

---

## 5. Where Rust would genuinely be better — and what it costs

Stated plainly, because the answer should survive scrutiny.

1. **SQLite.** `rusqlite` statically links the real C SQLite: full speed, battle-tested,
   no transpilation layer. Go's options are `modernc.org/sqlite` (a machine translation
   of SQLite's C into Go — real, used in production, but slower and a large generated
   codebase) or CGO, which costs the CGO-free static binary.
2. **No GC.** Deterministic memory and marginally better tail latency under adversarial
   load.
3. **Compile-time race freedom** on the streaming code, where Go relies on `-race` in CI.
4. **Lower RSS**, roughly 2–3×.
5. **Sum types.** The `Resolution` discriminated union would be a real `enum` with
   exhaustive matching, rather than a struct with a `Kind` field.

**What the measurements say about each:**

- (1) is the strongest — but §1.1 shows the catalog is **32 k rows**. Point lookups are
  microseconds under any driver; a 30–50 % slower driver on a 5 µs query is 7 µs, behind
  an in-memory LRU that most requests never get past. Bulk inserts are batched into
  ~100 ms transactions. **Measurement demotes this from a decisive concern to a
  phase-0 checkbox.**
- (2) and (4): with pooled 64 KB buffers, 1,000 concurrent streams cost ~64 MB of
  buffers and ~8 MB of goroutine stacks. Go's GC handles this allocation profile with
  sub-millisecond pauses. Non-issue.
- (3) is real, and the honest mitigation is `-race` in CI plus concentrated review on
  the one file that needs it.
- (5) is real and mildly annoying, forever.

**Cost of choosing Rust:** roughly **2× the schedule** — the 20-week solo estimate
becomes 35–40 — driven by async streaming ergonomics, plus a materially smaller hiring
pool for what is fundamentally an ops tool that a platform engineer should be able to
patch on a Friday.

**Trading ~18 additional weeks and long-term maintainability for advantages the
workload cannot exercise is not a good trade.**

---

## 6. Second-order: extensibility and the plugin path

[go-architecture.md §3](go-architecture.md) notes that third-party ecosystems
eventually want to be out-of-process. Go has an unusually clean answer:
**[wazero](https://wazero.io) is a pure-Go WebAssembly runtime with zero CGO**, so
ecosystem adapters could be compiled to WASM from any language and embedded without
breaking the static-binary property.

Rust has `wasmtime`, which is excellent but heavier. C#/Java have no equivalent that
survives their AOT constraints. This is a small point today and a meaningful one in
two years.

---

## 7. Honest risks of choosing Go, and their mitigations

| Risk | Mitigation |
|---|---|
| `modernc.org/sqlite` performance or an edge-case bug | Phase-0 spike against a copy of a real ledger (already planned). Documented fallback: CGO + `mattn/go-sqlite3` statically linked against musl — still one binary, just not CGO-free. Measured catalog size makes this low-probability. |
| `net/http` normalises paths; `ServeMux` mishandles `%2F` and greedy segments | Custom escape-safe router matching on `EscapedPath()` — already specified, ~150 lines, spiked in phase 0 |
| No sum types → `Resolution` is a struct with a `Kind` field | Constructor functions + an exhaustive-switch lint; accept the residual |
| Error-handling verbosity inflates LOC | Already priced into the 15–18 k estimate |
| GC pause under pathological allocation | Pooled buffers, `GOMEMLIMIT`, and a load test in phase 2 |
| Go's `net/http` enables HTTP/2 automatically, which some registry clients dislike | Explicit per-listener protocol configuration |

None of these is architectural. Each is a known quantity with a known workaround.

---

## 8. Verdict

**Go**, with `modernc.org/sqlite` and a CGO+musl fallback documented from day one.

The decision rests on: the deployment requirement is the entire point of the project
and Go's static-binary/cross-compile story is unmatched; this is Go's native domain
with directly applicable precedent (Gitea's multi-ecosystem registry, and zot, which
this project already replaced); the hardest concurrency in the system is the thing Go
is best at; and measurement shows the workload never becomes CPU- or database-bound,
which removes Rust's advantages from the table.

**The choice is also cheaply hedged.** The phase-0 spikes test exactly the assumptions
that could overturn it — SQLite driver behaviour, path escaping, streaming under
`-race`, static linking. If a spike fails, the fallback is a CGO build or a different
driver, not a different language. Nothing in
[go-architecture.md](go-architecture.md) is Go-specific: the blob store, the catalog
schema, the `Ecosystem` interface and the manifest-based snapshot design would all
survive a language change, so even a late reversal would cost implementation, not
design.
