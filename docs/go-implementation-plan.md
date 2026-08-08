# pkgreg — implementation plan

Written 2026-07-27. Build spec for [go-architecture.md](go-architecture.md);
language rationale in [language-choice.md](language-choice.md).

Status: **implemented; retained as the historical build spec**. The final frontend
differs from the planned React/build-tag tasks: checked-in HTML/CSS/ES modules are
embedded in every build, with no Node toolchain or `embedconsole` tag. Current
verification and delivered behavior are in [phase 10](phase10-cutover.md) and the
[system overview](system-overview.md).

This document fixes the package contracts, database schemas, wire formats, and the
numbered task breakdown used during implementation. Design debates belong in the
architecture doc; this one is kept for the acceptance criteria and rationale.

**Scope:** 76 tasks, ~101 engineer-days, 11 phases. Solo ≈ 20 weeks; three engineers
on the parallel tracks in §9.2 ≈ 9–11 weeks.

---

## 1. Ground rules

Decided once, applied everywhere. Deviations need a comment saying why.

| Rule | Detail |
|---|---|
| Go version | 1.23+; `CGO_ENABLED=0` for release builds |
| Context | Every function that does I/O takes `ctx context.Context` first. **Exception, deliberate:** the fetch goroutine runs on a detached context so a client disconnect never aborts a download other readers want. |
| Errors | Wrap with `fmt.Errorf("...: %w", err)`. Sentinels in the package that owns them (`blob.ErrNotFound`, `catalog.ErrConflict`). No panics outside `main` init. |
| Logging | `log/slog`, JSON. Never log secrets, tokens, or `Authorization` headers. One structured access line per request. |
| Concurrency | Everything shared is either immutable or mutex-guarded. `go test -race` is mandatory in CI. No naked `go func()` — every goroutine has a documented owner and a stop path. |
| Interfaces | Defined by the *consumer*, kept small. Only `catalog.Store` is wide, and only because it fronts one database. |
| Config | Never read `os.Getenv` outside `internal/config`. Handlers read `*config.Snapshot`. |
| SQL | Hand-written, no ORM. All queries prepared and named as constants. Migrations are numbered and forward-only. |
| Tests | Table-driven. `t.TempDir()` for filesystem. `httptest` for HTTP. A synthetic upstream fixture server (`internal/testutil/upstream`) with failure injection. Golden files for rendered indexes. |
| Performance assertions | Guard with `if race.Enabled { t.Skip(race.SkipReason) }`, never a `//go:build !race` tag — a build tag makes the test vanish silently from a `-race`-only CI job. ThreadSanitizer costs 5–20×, and the cost is *not* uniform: `catalog` sits at the top of that range because `modernc.org/sqlite` is transpiled C and therefore memory-access-dense (measured 1,223 vs 15,396 inserts/s). Correctness tests always run instrumented. |
| Metrics | A Prometheus `*Vec` is a factory, not a metric: with no observed label combination it contributes nothing to a scrape, so `rate(...)` reads as *no data* rather than `0` and dashboards show a false outage. Pre-create the series whose labels are bounded (`obs.InitProjectSeries` at project registration). Never assert on a bare metric name in a test — assert the full exposition line, which also proves the label names match the declaration. |
| Naming | Package names are singular nouns (`blob`, `catalog`, `eco`). No `utils`, no `common`, no `helpers`. |
| Dependencies | Adding one requires a line in `docs/dependencies.md` saying why stdlib is insufficient. |

### 1.1 Definition of done (every task)

1. Code + doc comments on exported identifiers
2. Unit tests, `-race` clean
3. `golangci-lint` clean
4. The task's acceptance criterion demonstrably met
5. No TODOs without an issue reference

---

## 2. Repository layout

```
cmd/pkgreg/main.go            subcommand dispatch only
internal/
  buildinfo/                  version, commit, date (ldflags)
  config/     snapshot.go store.go sources.go types.go
  blob/       store.go writer.go gc.go
  catalog/    store.go sqlite.go schema.go queries.go batch.go cache.go
  engine/     engine.go fetch.go inflight.go serve.go ref.go document.go
  upstream/   pool.go creds.go bearer.go peer.go
  eco/        registry.go descriptor.go ctx.go route.go resolution.go
    oci/ npm/ pypi/ apt/ git/ files/
  gitmirror/  mirror.go uploadpack.go
  router/     mux.go project.go listener.go portmux.go
  control/
    api/      v1.go legacy.go sse.go
    auth/     accounts.go sessions.go tokens.go password.go guard.go
    project/  project.go quota.go upstream.go
    job/      queue.go runner.go
    ops/      checkpoint.go export.go importer.go lockwarm.go
  snapshot/   manifest.go diff.go pack.go
  maintenance/ gc.go evict.go quota.go scheduler.go
  obs/        log.go metrics.go events.go
  pki/        ca.go leaf.go
  migrate/    frompython.go
  testutil/   upstream/ clients/ golden/
web/console/                  React source (kept) + embed.go
docs/
```

---

## 3. Package contracts

These are the interfaces the phases implement. Getting them right up front is what
lets phases 3/4/5 and 7/8 run in parallel.

### 3.1 `blob` — content-addressed store

```go
type Digest string // lowercase sha256 hex, 64 chars

type Stat struct { Size int64; ModTime time.Time }

type Store interface {
    Create(ctx context.Context) (*Writer, error)
    Open(d Digest) (*os.File, Stat, error)   // caller closes
    Stat(d Digest) (Stat, bool)
    Delete(d Digest) error
    Walk(fn func(Digest, Stat) error) error
    ManagedDir(eco, project string) (string, error)
}

// Writer hashes inline; Commit fsyncs, links to the final path, fsyncs the dir.
// Commit is idempotent: an existing target means an identical-bytes race, not an error.
type Writer struct{ /* ... */ }
func (w *Writer) Write(p []byte) (int, error)
func (w *Writer) Commit() (Digest, int64, error)
func (w *Writer) Abort() error                 // safe to call after Commit
```

**Invariants.** A blob is immutable once linked. Nothing rewrites one in place. That
single property is what makes concurrent GC, live snapshots, and cross-project
hardlinking safe with no locking.

### 3.2 `catalog` — the metadata store

```go
type Store interface {
    // blobs
    UpsertBlob(ctx context.Context, b Blob) error
    Blob(ctx context.Context, d blob.Digest) (Blob, bool, error)
    WalkBlobs(ctx context.Context, fn func(Blob) error) error
    UnreferencedBlobs(ctx context.Context, olderThan time.Time) ([]blob.Digest, error)

    // entries — the byte cache
    Entry(ctx context.Context, k EntryKey) (Entry, bool, error)
    PutEntry(ctx context.Context, e Entry) error        // batched (§3.2.1)
    DeleteEntry(ctx context.Context, k EntryKey) error
    DeleteProject(ctx context.Context, project string) (int64, error)
    ListEntries(ctx context.Context, q EntryQuery) ([]Entry, error)
    EntriesForSnapshot(ctx context.Context, project string) (iter.Seq2[Entry, error], error)

    // refs — mutable names
    Ref(ctx context.Context, k RefKey) (Ref, bool, error)
    PutRef(ctx context.Context, r Ref) error
    ListRefs(ctx context.Context, project, eco, prefix string) ([]Ref, error)

    // artifacts — semantic inventory
    PutArtifact(ctx context.Context, a Artifact) error
    QueryArtifacts(ctx context.Context, q ArtifactQuery) ([]Artifact, int, error)

    // snapshots
    PutSnapshot(ctx context.Context, s Snapshot) error
    Snapshot(ctx context.Context, id string) (Snapshot, bool, error)
    ListSnapshots(ctx context.Context, project string, limit int) ([]Snapshot, error)

    // stats
    RecordAccess(ctx context.Context, batch []AccessDelta) error
    Stats(ctx context.Context, q StatsQuery) (StatsResult, error)

    Tx(ctx context.Context, fn func(Store) error) error
    Close() error
}

type EntryKey struct{ Project, Eco, Key string }

type Entry struct {
    EntryKey
    Digest     blob.Digest
    Size       int64
    MediaType  string
    CachedAt   time.Time
    LastAccess time.Time
    Hits       int64
}

type Ref struct {
    Project, Eco, Name string
    Target             string // digest, commit sha, or entry key
    ETag, LastModified string
    FetchedAt          time.Time
    TTL                time.Duration
}
```

#### 3.2.1 Write-path durability

The blob is fsync'd and linked **before** the entry row is written. A lost entry
insert therefore costs a re-fetch, never corruption — which makes it safe to batch
entry inserts and access-stat updates into ~100 ms transactions. That is what keeps a
`uv sync` burst of thousands of small files from becoming thousands of fsyncs.

`PutEntry` enqueues; a single writer goroutine flushes on a 100 ms tick or at 500
rows. `Flush(ctx)` forces it (used by checkpoint and shutdown).

### 3.3 `eco` — the extension point

Refined from the architecture doc: adapters get a **`Ctx` with engine primitives**
rather than a single `Resolve()`. `Resolve()` alone could not express the files role's
PUT/DELETE or git's managed directory without special cases.

```go
type Ecosystem interface {
    Descriptor() Descriptor
    Routes() []Route
}

type Descriptor struct {
    ID, Display string
    Storage     StorageMode  // Blob | ManagedDir
    Listener    ListenerKind // PathPrefixed | ProtocolRooted | ForwardProxy
    Upstreams   UpstreamSchema
    Freshness   func(key string) Freshness
    ParseArtifact func(key string) (name, version, arch string, ok bool)
    Setup       func(SetupCtx) []SetupStep  // the console's copy-paste instructions
}

type Route struct {
    Methods []string
    Pattern string // relative to the eco root; "" = the eco root itself
    Handler func(*Ctx) error
    Admin   bool   // "+"-prefixed; registered before greedy routes
}
```

```go
// Ctx is the adapter's entire view of the system. Adapters never touch SQL,
// the filesystem, HTTP clients, or single-flight.
type Ctx struct {
    Ctx     context.Context
    R       *http.Request
    W       http.ResponseWriter
    Project string
    Eco     string
    Params  Params // path captures, still percent-escaped
}

// serving
func (c *Ctx) Serve(r Resolution) error       // the full hit/dedup/peer/offline/miss pipeline
func (c *Ctx) ServeBytes(code int, contentType string, body []byte) error
func (c *Ctx) JSON(code int, v any) error
func (c *Ctx) Text(code int, s string) error

// upstream documents (indexes, packuments) — fetched, cached, revalidated by TTL/ETag
func (c *Ctx) Document(d DocSpec) ([]byte, error)

// refs
func (c *Ctx) Ref(name string) (Ref, bool, error)
func (c *Ctx) PutRef(r Ref) error

// writes (files role, LFS)
func (c *Ctx) PutBlob(key string, r io.Reader, o PutOptions) (PutResult, error)
func (c *Ctx) DeleteEntry(key string) error
func (c *Ctx) ListEntries(prefix string) ([]Entry, error)

// environment
func (c *Ctx) Upstreams() []Upstream
func (c *Ctx) Offline() bool
func (c *Ctx) ManagedDir() (string, error)
func (c *Ctx) ExternalBase() string  // scheme://host/<project>/<eco> for URL rewriting
func (c *Ctx) RecordArtifact(a Artifact)
```

```go
type Resolution struct {
    Key        string        // cache key within (project, eco)
    Upstream   *UpstreamReq  // nil ⇒ cache-only
    ExpectSHA  string        // known up front (pypi hashes, OCI digests) ⇒ dedup before fetch
    ExpectSize int64
    MediaType  string
    Headers    http.Header
    Immutable  bool
    Artifact   *ArtifactSpec // recorded on commit
}
```

**Registration is one line:**

```go
func init() { eco.Register(npm.New()) }
```

Everything else — routing, the console's endpoints panel, snapshot inclusion, the
manifest exporter, `/api/v1/ecosystems` — derives from `Descriptor`. This is the fix
for the eight duplicated mapping tables.

### 3.4 `engine` — the pipeline

```go
type Engine struct { /* blobs, catalog, upstreams, inflight, events, cfg */ }

func (e *Engine) Serve(c *eco.Ctx, r eco.Resolution) error
```

`Serve` is the only place the cache decision lives:

```
1. entry hit                     → ServeContent (sendfile)                [HIT]
2. ExpectSHA and blob present    → link entry, serve                      [DEDUP]
3. peer configured and has blob  → fetch from sibling                     [PEER]
4. offline                       → 404 with a specific reason
5. single-flight fetch           → progressive tail-follow                [MISS]
```

Steps 1–2 are new for npm and apt: every download is hashed inline, so a blob one
project fetched is reusable by every other project even when the digest was not known
in advance. Today that only works for pypi and OCI.

```go
// inflight
type Fetch struct {
    mu      sync.Mutex
    written int64
    done    bool
    err     error
    notify  chan struct{} // closed and replaced on each change = broadcast

    HeadersReady chan struct{}
    Total        int64 // -1 unknown
    MediaType    string
    Digest       blob.Digest
}

type Registry struct{ mu sync.Mutex; m map[string]*Fetch }
func (r *Registry) Do(key string, mk func() *Fetch) (f *Fetch, created bool)
```

**Invariants for `Fetch`** (each has a dedicated test):
- runs on a detached context — client disconnect never aborts it
- `HEAD` and `Range` wait for commit, then hand off to `http.ServeContent`
- error before first byte propagates to readers; error after first byte truncates
- `Content-Length` set only when upstream declared one
- sha256/size/truncation mismatch ⇒ reject commit, unlink staging, no entry row

---

## 4. Database schemas

Two SQLite databases (`modernc.org/sqlite`), both `journal_mode=WAL`,
`synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`.

### 4.1 `catalog.db`

```sql
CREATE TABLE schema_version (version INTEGER PRIMARY KEY);

CREATE TABLE blobs (
  sha256      TEXT PRIMARY KEY,
  size        INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  last_access INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX ix_blobs_access ON blobs(last_access);

CREATE TABLE entries (
  project     TEXT NOT NULL,
  eco         TEXT NOT NULL,
  key         TEXT NOT NULL,
  sha256      TEXT NOT NULL REFERENCES blobs(sha256),
  size        INTEGER NOT NULL,
  media_type  TEXT,
  cached_at   INTEGER NOT NULL,
  last_access INTEGER NOT NULL,
  hits        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project, eco, key)
) WITHOUT ROWID;
CREATE INDEX ix_entries_sha    ON entries(sha256);     -- refcount / GC mark
CREATE INDEX ix_entries_access ON entries(last_access); -- eviction
CREATE INDEX ix_entries_prefix ON entries(project, eco, key); -- autoindex, covered by PK

CREATE TABLE refs (
  project TEXT NOT NULL, eco TEXT NOT NULL, name TEXT NOT NULL,
  target        TEXT NOT NULL,
  media_type    TEXT,
  etag          TEXT,
  last_modified TEXT,
  fetched_at    INTEGER NOT NULL,
  ttl_seconds   INTEGER NOT NULL,
  PRIMARY KEY (project, eco, name)
) WITHOUT ROWID;

CREATE TABLE artifacts (
  project TEXT NOT NULL, eco TEXT NOT NULL,
  name TEXT NOT NULL, version TEXT NOT NULL, arch TEXT NOT NULL DEFAULT '',
  sha256 TEXT REFERENCES blobs(sha256),
  size INTEGER, origin TEXT, cached_at INTEGER NOT NULL, extra TEXT,
  PRIMARY KEY (project, eco, name, version, arch)
) WITHOUT ROWID;
CREATE INDEX ix_artifacts_name ON artifacts(name);
CREATE INDEX ix_artifacts_size ON artifacts(size DESC);

CREATE TABLE access_stats (
  project TEXT NOT NULL, eco TEXT NOT NULL, name TEXT NOT NULL,
  count INTEGER NOT NULL DEFAULT 0, last_access INTEGER,
  PRIMARY KEY (project, eco, name)
) WITHOUT ROWID;

CREATE TABLE traffic_stats (
  project TEXT NOT NULL, eco TEXT NOT NULL,
  hit_count INTEGER NOT NULL DEFAULT 0,  hit_bytes  INTEGER NOT NULL DEFAULT 0,
  miss_count INTEGER NOT NULL DEFAULT 0, miss_bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project, eco)
) WITHOUT ROWID;

CREATE TABLE snapshots (
  id TEXT PRIMARY KEY, project TEXT NOT NULL, parent TEXT,
  manifest_sha256 TEXT NOT NULL REFERENCES blobs(sha256),
  entry_count INTEGER NOT NULL, total_bytes INTEGER NOT NULL,
  created_at INTEGER NOT NULL, subject TEXT NOT NULL, author TEXT
);
CREATE INDEX ix_snapshots_project ON snapshots(project, created_at DESC);

CREATE TABLE heads (project TEXT PRIMARY KEY, snapshot_id TEXT NOT NULL) WITHOUT ROWID;
```

Refcounts are **derived** (`ix_entries_sha` + artifacts + snapshot manifests), not
maintained. A stored counter is one more thing to get wrong; the mark phase of GC is a
cheap index scan at this cardinality.

### 4.2 `control.db`

```sql
CREATE TABLE projects (
  name TEXT PRIMARY KEY, owner TEXT, created_at INTEGER NOT NULL,
  offline INTEGER NOT NULL DEFAULT 0,
  quota_bytes INTEGER NOT NULL DEFAULT 0,      -- 0 = unlimited
  quota_artifacts INTEGER NOT NULL DEFAULT 0,
  data_plane_auth TEXT NOT NULL DEFAULT 'public'  -- public | token
) WITHOUT ROWID;

CREATE TABLE instance_flags (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;

CREATE TABLE users (
  username TEXT PRIMARY KEY, role TEXT NOT NULL,
  salt TEXT NOT NULL, hash TEXT NOT NULL,      -- scrypt N=2^14 r=8 p=1 dklen=32
  reports_to TEXT, created_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE tokens (
  id TEXT PRIMARY KEY, secret_sha256 TEXT NOT NULL,
  project TEXT, eco TEXT, scope TEXT NOT NULL,  -- read | write | admin
  label TEXT, created_by TEXT, created_at INTEGER NOT NULL,
  expires_at INTEGER, last_used INTEGER
) WITHOUT ROWID;

CREATE TABLE upstreams (
  id INTEGER PRIMARY KEY, project TEXT, eco TEXT NOT NULL,
  name TEXT NOT NULL, url TEXT NOT NULL, kind TEXT NOT NULL,  -- origin | peer
  priority INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
  credential_id INTEGER REFERENCES credentials(id),
  UNIQUE (project, eco, name)
);

CREATE TABLE credentials (
  id INTEGER PRIMARY KEY, label TEXT NOT NULL,
  kind TEXT NOT NULL,        -- basic | bearer | none
  sealed BLOB NOT NULL       -- nacl/secretbox under the host key
);

CREATE TABLE jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT, project TEXT, action TEXT NOT NULL,
  status TEXT NOT NULL,      -- queued | running | done | failed | cancelled
  params TEXT, started_at INTEGER, finished_at INTEGER, error TEXT, actor TEXT
);

CREATE TABLE audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  actor TEXT, action TEXT NOT NULL, target TEXT, detail TEXT, client_ip TEXT
);
CREATE INDEX ix_audit_ts ON audit(ts DESC);
```

**Token hashing:** tokens carry ~256 bits of entropy, so a plain sha256 of the secret
is correct and avoids a per-request scrypt cost. Passwords keep scrypt at the existing
parameters, so every current `users.json` hash migrates and verifies unchanged.

---

## 5. Wire formats

### 5.1 Snapshot manifest

Sorted TSV, gzipped, stored as an ordinary blob. Sort order `(eco, key)` byte-wise, so
diffing two snapshots is a linear merge with no memory growth.

```
#pkgreg-manifest v1 project=<p> created=<rfc3339>
<eco>\t<key>\t<sha256>\t<size>\n
```

### 5.2 Transfer pack (`pkgreg export`)

Uncompressed tar of already-compressed members, streamed:

```
pack.json                       {version, project, base, target, snapshots[], blobs, bytes}
snapshots/<id>.manifest.gz
blobs/<aa>/<sha256>
certs/{ca.crt,server.crt,server.key}   # global project only, never ca.key
```

Import verifies every blob's sha256 against its filename before linking, and rejects a
non-fast-forward by parent-pointer check.

### 5.3 Peer protocol

```
HEAD /peer/v1/blob/{sha256}     → 200 + Content-Length | 404
GET  /peer/v1/blob/{sha256}     → the bytes (Range supported)
GET  /peer/v1/have?d=<a>&d=<b>  → {"have":["<sha>",…]}   batch probe
```

Digest-first, so a peer is consulted without re-resolving any ecosystem index.
Authenticated with a `peer` scope token.

### 5.4 API v1

```
GET    /api/v1/ecosystems                          descriptors — drives the console
GET    /api/v1/projects                POST
GET    /api/v1/projects/{p}            PATCH (mode|quota|owner|auth)   DELETE
GET    /api/v1/projects/{p}/artifacts  ?eco=&q=&sort=&page=
GET    /api/v1/projects/{p}/endpoints
GET    /api/v1/projects/{p}/grants     PUT /{name} | DELETE /{name}
GET    /v2/{name...}                   OCI; with server.registry_mirror set, a
                                       bare /v2/library/alpine also resolves
GET    /api/v1/projects/{p}/upstreams  POST | PATCH /{id} | DELETE /{id}
GET    /api/v1/projects/{p}/snapshots  POST
POST   /api/v1/projects/{p}/snapshots/{id}/rollback
POST   /api/v1/projects/{p}/export     POST /api/v1/projects/{p}/import
GET    /api/v1/stats                   ?project=&eco=&group_by=
GET    /api/v1/jobs                    GET /api/v1/jobs/{id}   DELETE /api/v1/jobs/{id}
GET    /api/v1/tokens                  POST | DELETE /{id}
GET    /api/v1/users                   POST | PATCH /{n} | DELETE /{n}
POST   /api/v1/login                   POST /api/v1/logout    GET /api/v1/me
GET    /api/v1/events                  SSE: progress | job | health | audit
GET    /healthz  /readyz  /metrics
```

Errors are uniform: `{"error": "...", "code": "..."}` with the carried status.

---

## 6. Phase 0 — spikes (5 d)

Each spike either passes or selects a documented fallback. **Nothing else starts until
these are done** — they test the assumptions that could change the plan.

| ID | Spike | Pass criterion | Fallback if it fails |
|---|---|---|---|
| **S1** | `modernc.org/sqlite` under load | 100 k entries; point lookup p99 < 200 µs; 5 k batched inserts/s; WAL reader sees committed writes; `GROUP BY` over 100 k < 50 ms | CGO + `mattn/go-sqlite3` statically linked against musl |
| **S2** | Progressive delivery | 20 readers on a 2 GB stream all get byte-identical content + correct sha256; mid-stream disconnect does not abort; upstream failure at 50 % surfaces without goroutine leak; `-race` clean | redesign `Fetch` around a ring buffer |
| **S3** | Path escaping | Custom mux round-trips `@babel%2Fcore`, `/v2/library/alpine/manifests/3.20`, apt absolute-form target, and a filename with `+`/`%2B` | — (must pass; it is 150 lines) |
| **S4** | Native PKI | `crypto/x509`-minted CA + leaf accepted by `docker pull`, `git clone`, `pip`, `curl`; reuses an existing `certs/ca.key` | keep `gen-certs.sh` for one release |
| **S5** | Port multiplexing | One listener serves TLS + plain HTTP + apt absolute-form by first-byte sniff; no measurable latency cost | ship three explicit listeners (config already supports it) |
| **S6** | Console embed | `go:embed` of a built `dist/` serves the SPA; `!embedconsole` tag builds with no Node present | — |

**Exit:** a one-page spike report per item, committed under `docs/spikes/`.

---

## 7. Phase breakdown

Format: `ID | task | days | depends on | acceptance`.

### Phase 1 — Foundation (14 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P1-01 | Repo scaffold: module, Makefile, `golangci-lint`, CI, `buildinfo` | 1 | — | `make build test lint` green; `pkgreg version` prints commit |
| P1-02 | `obs/log`: slog setup, access-log middleware, redaction | 1 | P1-01 | One JSON line per request with project/eco/outcome/bytes/duration; no `Authorization` ever logged |
| P1-03 | `obs/events`: in-process bus, bounded per-subscriber channels, drop-on-full | 1 | P1-01 | 10 k events/s to 5 subscribers, no blocking, drops counted |
| P1-04 | `obs/metrics`: registry + the counters in §10 | 1 | P1-01 | `/metrics` scrapes clean |
| P1-05 | `config/types` + sources: defaults → file → env → flags | 1.5 | P1-01 | Precedence table test; invalid config fails at startup with a usable message, not a stack trace |
| P1-06 | `config/store`: immutable `Snapshot`, `atomic.Pointer` swap, change notify | 1 | P1-05 | Concurrent readers never tear; a mutation is visible on the next request; `-race` clean |
| P1-07 | `blob/writer`: staging, inline hash, fsync, link, idempotent commit | 1.5 | P1-01 | Kill -9 mid-write leaves only staging garbage; concurrent identical commits both succeed |
| P1-08 | `blob/store`: Open/Stat/Walk/Delete, `ManagedDir`, staging GC on boot | 1 | P1-07 | Fuzz digests for path traversal; `Walk` over 100 k blobs < 2 s |
| P1-09 | `catalog/schema` + forward-only migration runner | 1 | P1-01 | Fresh DB and re-run are both idempotent; version recorded |
| P1-10 | `catalog/sqlite`: blobs + entries | 1.5 | P1-09 | Store contract suite passes |
| P1-11 | `catalog/sqlite`: refs, artifacts, stats, snapshots | 1.5 | P1-10 | Store contract suite passes |
| P1-12 | `catalog/batch` writer + `catalog/cache` LRU | 1 | P1-10 | 5 k inserts/s sustained; `Flush` is synchronous; LRU invalidated on write |
| P1-13 | `pki`: CA + leaf minting, SAN discovery, reuse existing CA | 1 | P1-01 | S4 clients accept it; existing `certs/ca.key` reused so distributed trust survives |

### Phase 2 — Engine (10 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P2-01 | `upstream/pool`: per-origin clients, `DisableCompression`, dial/ctx timeouts | 1 | P1-05 | Byte-faithful: response bytes == upstream `Content-Length` on a gzip-advertising origin |
| P2-02 | `upstream/creds` + `bearer`: sealed credentials, OCI anonymous token dance | 1.5 | P2-01 | Anonymous ghcr/quay/dockerhub pulls; a `basic` credential reaches a private origin |
| P2-03 | `engine/inflight`: `Fetch` + broadcast + `Registry` | 2 | P1-07 | S2 criteria as a permanent test; all §3.4 invariants covered |
| P2-04 | `engine/serve`: the 5-step pipeline | 2 | P2-03, P1-10 | Table test over hit/dedup/peer/offline/miss with a synthetic upstream |
| P2-05 | Range/HEAD/conditional via `ServeContent`; offline 404 reasons | 1 | P2-04 | `Range`, `If-Range`, `If-None-Match`, multipart all correct; HEAD never streams |
| P2-06 | `engine/ref`: TTL + ETag/Last-Modified revalidation, offline last-known | 1.5 | P2-04 | 304 bumps `fetched_at` without rewriting the blob; offline resolves from the ref |
| P2-07 | `engine/document`: fetch + cache + revalidate upstream docs | 1 | P2-06 | An index is fetched once across N concurrent requests |

### Phase 3 — Framework + files / npm / pypi (12 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P3-01 | `eco`: registry, `Descriptor`, `Ctx`, `Route`, `Resolution` | 1.5 | P2-04 | A 30-line dummy ecosystem serves a cached file end to end |
| P3-02 | `router/mux`: escape-safe matcher, greedy non-terminal wildcards, ordering | 1.5 | P3-01 | S3 corpus as a permanent test |
| P3-03 | `eco/files`: GET/HEAD, catalog-backed autoindex, PUT (streaming sha256, write-once, checksum header), DELETE, token gate | 2 | P3-01 | `wget -r` walks the tree; PUT is write-once until `?overwrite=1`; `X-Checksum-Sha256` mismatch is a 400 |
| P3-04 | `eco/npm`: packument fetch + `dist.tarball` rewrite, tarball serve | 2 | P3-01 | `npm install left-pad chalk @babel/core` works; unknown packument fields preserved byte-for-byte |
| P3-05 | `eco/pypi`: PEP 503 HTML + PEP 691 JSON parse **and** render, `+f`, `.metadata` sidecars, `/+indexes` | 3 | P3-01 | `pip install`, `uv pip install`, `uv sync` work; PEP 658/714 `core-metadata` normalised (golden tests) |
| P3-06 | Client acceptance harness: real `pip`/`uv`/`npm`/`wget` in CI | 2 | P3-03..05 | Green against a live upstream and against a recorded fixture |

### Phase 4 — oci + apt (10 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P4-01 | `eco/oci`: `/v2/`, manifests by tag and digest, blobs, `tags/list` | 2.5 | P3-01 | `docker pull` of a single-arch image |
| P4-02 | oci: index children → back-fill the tag row's real image size | 1 | P4-01 | Multi-arch tag reports layer+config bytes, not manifest bytes; no duplicate digest rows |
| P4-03 | oci: offline tag resolution via `refs`; project name re-prefixing | 1 | P4-01, P2-06 | Offline `docker pull` by tag; `tags/list` echoes the project-prefixed name |
| P4-04 | `eco/apt`: absolute-form target reconstruction, any-host relay + optional allowlist | 1.5 | P3-01 | `apt-get update && apt-get install` through the proxy |
| P4-05 | apt: volatile/immutable via `Freshness`; `.meta` sidecars replaced by `refs` | 1.5 | P2-06 | `InRelease` revalidates with ETag; 304 serves cache; `.deb` never revalidates |
| P4-06 | apk support + artifact parsing for both | 1 | P4-04 | `apk add` through the proxy; `.apk`/`.deb` land in the inventory with name+version |
| P4-07 | Acceptance: multi-arch `docker pull`, `apt-get`, `apk add` in CI | 1.5 | P4-01..06 | Green |

**Status (2026-07-27): COMPLETE.** Recorded fixtures pass a real multi-arch
`docker pull`, `apt-get update && install`, and `apk add`; the adapter suites cover
offline refs, exact status relay, conditional revalidation, allowlisting, integrity,
and inventory. See the [Phase 4 report](phase4-oci-apt.md).

### Phase 5 — git (8 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P5-01 | `ManagedDir` storage mode wiring | 0.5 | P1-08 | Mirrors land under `managed/git/`; path traversal rejected |
| P5-02 | `gitmirror`: clone/fetch/HEAD-sync/prune, per-repo locks, `gc.auto=0` | 2 | P5-01 | Concurrent fetches of one repo serialise; no auto-repack ever |
| P5-03 | `upload-pack` streaming: `exec.CommandContext` → `ResponseWriter`, stderr tail, semaphore | 2 | P5-02 | Client disconnect reaps the child (verified by pid check); 8-way cap holds |
| P5-04 | `info/refs` advertisement, protocol v2, push refusal, dumb-protocol 404 | 1 | P5-03 | Full, shallow, and `--filter=blob:none` clones all succeed |
| P5-05 | Git LFS: batch endpoint + `+lfs/{oid}` through the engine | 1.5 | P5-03, P2-04 | `git lfs pull` works; LFS objects dedup against the blob store |
| P5-06 | `+maintain` geometric repack; ref sync into `refs` + inventory | 1 | P5-02 | Repack runs under the lock; mirror size reported once per repo |

**Status (2026-07-27): COMPLETE.** Real Git clients pass full, shallow,
protocol-v2 partial, SHA-pinned, and offline clones; push is refused. Real
`git lfs pull` succeeds online and from the CAS offline. Concurrency coverage
verifies serialized refresh, the upload-pack cap, and child reaping. See the
[Phase 5 report](phase5-git.md).

### Phase 6 — Listeners and routing (5 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P6-01 | Project resolution: path prefix, OCI image name, apt proxy username | 1.5 | P3-02 | Unknown project 404s with a helpful message instead of falling through to global |
| P6-02 | Unified listener namespace split, `/` index, `/healthz` | 1 | P6-01 | Port of the existing unified test suite |
| P6-03 | Port mux (TLS first-byte sniff), explicit-listener config | 1 | S5 | One port serves docker + pip + npm + apt + console |
| P6-04 | TLS config, SNI, hot cert reload on `SIGHUP` | 1 | P1-13 | Cert swap with no dropped connections |
| P6-05 | Graceful shutdown, `/readyz`, in-flight drain | 0.5 | P6-02 | `SIGTERM` completes in-flight downloads within a deadline, then exits 0 |

**Status (2026-07-27): COMPLETE.** All six adapters are mounted behind the
production project router. Single-port and explicit-listener modes pass real TCP/TLS
tests; SNI, atomic certificate reload, readiness, and graceful in-flight download
draining are covered. See the [Phase 6 report](phase6-listeners-routing.md).

### Phase 7 — Control plane (14 d)

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P7-01 | `control.db` schema + migrations | 0.5 | P1-09 | Idempotent |
| P7-02 | `auth/password` (scrypt, existing params) + `auth/accounts` policy | 1.5 | P7-01 | **An existing `users.json` hash verifies unchanged**; full role/reporting policy suite |
| P7-03 | `auth/sessions`: opaque tokens, monotonic TTL, per-IP throttle | 1 | P7-01 | Lockout after 5 failures; restart revokes all |
| P7-04 | `auth/tokens`: issue/verify/revoke, scopes, `last_used` | 1.5 | P7-01 | CI token can PUT to `files` on one project and nothing else |
| P7-05 | `project`: CRUD, ownership, offline flag, quotas | 1.5 | P1-06 | Create is routable on the **next request**, not after a 5 s poll |
| P7-06 | `project/upstream`: live add/edit/remove, per-project override | 1.5 | P2-01 | Adding a private PyPI index takes effect with no restart |
| P7-07 | `auth/guard`: view/operate/create/superuser + CSRF origin check | 1 | P7-02 | Port of `test_ownership.py`; cross-origin mutation refused |
| P7-08 | `job`: per-project queue, bounded pool, persisted logs, cancel | 1.5 | P7-01 | Checkpointing project A does not block project B |
| P7-09 | `api/v1`: route table, handlers, uniform errors | 2 | P7-02..08 | Full endpoint suite |
| P7-10 | Legacy `/api/*` shim | 1 | P7-09 | The **current** React console runs unmodified against pkgreg |
| P7-11 | SSE `/api/v1/events` from the event bus | 1 | P1-03 | Progress arrives < 100 ms after a chunk; slow client dropped, not blocking |
| P7-12 | Audit log + `pkgreg audit` | 0.5 | P7-01 | Every mutation recorded with actor and IP |

Status: **COMPLETE** (2026-07-27). `control.db`, legacy-hash-compatible accounts,
process-local throttled sessions, scoped tokens, live projects/upstreams, authorization
guards, persisted per-project jobs, API v1, the legacy console shim, SSE and audit are
wired into the production composition root. See the
[Phase 7 report](phase7-control-plane.md).

### Phase 8 — Air-gap (11 d)

Status: **COMPLETE** (2026-07-27). Streaming content-addressed manifests,
transactional rollback, flat-memory full/delta packs, verified fast-forward import,
managed Git mirror transfer, `uv.lock` warming and the operator CLI are wired into
the durable Phase 7 jobs. See the [Phase 8 report](phase8-air-gap.md).

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P8-01 | `snapshot/manifest`: streaming writer/reader, sorted, gzipped, stored as a blob | 1.5 | P1-07 | 100 k entries round-trip; memory flat |
| P8-02 | `ops/checkpoint` | 1.5 | P8-01 | Checkpoint of the 32 k-entry / 119 GB cache in **< 10 s** (a `SELECT`, not a tree walk); no quiesce |
| P8-03 | `ops/rollback` | 1.5 | P8-01 | Restores an older snapshot exactly; blobs still present (snapshot-pinned) |
| P8-04 | `snapshot/diff` + export pack writer | 2 | P8-01 | Delta contains exactly the blobs in target and not base; streamed, memory flat |
| P8-05 | Import: verify, link, apply, fast-forward check | 2 | P8-04 | Corrupt blob rejected by digest; non-fast-forward refused with a clear error |
| P8-06 | `ops/lockwarm`: uv.lock parse → warm → rewrite | 1.5 | P3-05 | `uv sync --frozen` and `--locked` both succeed against the rewritten lock |
| P8-07 | Two-host round-trip acceptance | 1 | P8-02..05 | checkpoint → export → import on host B → rollback; trees byte-identical |

### Phase 9 — Scale features (10 d)

Status: **COMPLETE** (2026-07-27). Online race-safe collection, LRU/TTL eviction
with checkpoint pins, transactional project quotas, authenticated peer federation,
scheduled/manual maintenance, dashboard coverage, and project/token rate limits are
wired into the production composition root. See the
[Phase 9 report](phase9-scale-features.md).

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P9-01 | `maintenance/gc`: online mark & sweep with grace period | 2 | P1-08 | Deleting a project reclaims its exclusive bytes; shared blobs survive; safe under concurrent writes |
| P9-02 | `maintenance/evict`: LRU / TTL / pin, snapshot-pinned never evicted | 2 | P9-01 | Holds a size target under sustained ingest; a pinned snapshot always restores |
| P9-03 | `maintenance/quota`: enforcement at commit, per-project | 1 | P7-05 | Over-quota PUT is a 507 with the current usage |
| P9-04 | `upstream/peer`: the §5.3 protocol, both directions | 2 | P2-01 | Two instances chained: B serves from A without touching the internet |
| P9-05 | `maintenance/scheduler` + `pkgreg gc/evict` CLI | 1 | P9-01..03 | Cron-style schedules; `--dry-run` reports without acting |
| P9-06 | Metrics coverage + a reference Grafana dashboard | 1 | P1-04 | Hit rate, bytes saved, in-flight, store size, upstream latency all graphed |
| P9-07 | Per-client rate limiting | 1 | P7-04 | Configurable per project and per token |

### Phase 10 — Console, migration, cutover (12 d)

Status: **COMPLETE — LIVE DIFFERENTIAL CUTOVER GATE PASSED** (2026-07-28).
The console, embedded delivery, resumable migration, parity harness, and clean-host
installer are implemented. With the corrected OCI tag materialization, the
repository's real 119 GB cache migrated in 52.717 s with a 0.913 s zero-work resume.
Independent hard-offline Python and Go deployments passed all 46 checked-in
differential cases. See the
[Phase 10 report and runbook](phase10-cutover.md).

The numbered Go implementation roadmap ends at Phase 10. Post-roadmap client
usability work is tracked separately in [client onboarding](client-onboarding.md);
its generated setup-script Phase 2 is implemented as of 2026-07-28.

| ID | Task | d | Dep | Acceptance |
|---|---|---|---|---|
| P10-01 | Console: migrate `lib/api.ts` to API v1 | 1.5 | P7-09 | All panels work; legacy shim no longer exercised |
| P10-02 | Console: descriptor-driven ecosystem UI from `/api/v1/ecosystems` | 2 | P7-09 | Adding a dummy ecosystem server-side makes it appear in the UI with **no frontend change** |
| P10-03 | Console: SSE replaces polling | 1 | P7-11 | Downloads/recent/jobs/health live; no 1.5 s timer |
| P10-04 | Console: new panels — quotas, GC/eviction, tokens, upstreams | 2 | P9-01..03 | Each surfaces its API |
| P10-05 | `web/embed.go`, static serving, security headers, `!embedconsole` tag | 1 | S6 | CSP byte-identical to `nginx.conf`; `go test ./...` needs no Node |
| P10-06 | `migrate/frompython`: resumable importer | 2 | P8-01 | 119 GB migrated in < 2 h, resumable, safe while the Python stack serves; `.cas` entries linked without re-hash |
| P10-07 | Differential harness (§8) | 1.5 | P10-06 | Full corpus clean |
| P10-08 | `pkgreg init` / `systemd install` / `doctor`; ops docs | 1 | P6-05 | Clean VM with only `git` installed → serving in one command |

---

## 8. Verification strategy

| Level | What | Where |
|---|---|---|
| Unit | Pure logic: parsers, renderers, policy, path safety | alongside each package |
| Contract | One suite run against every `catalog.Store` and `blob.Store` implementation | `internal/catalog/storetest` |
| Integration | Ecosystem ⇄ engine ⇄ storage against a synthetic upstream with failure injection (slow, truncating, 500, 401-then-200) | `internal/eco/*/integration_test.go` |
| Client acceptance | Real `docker`, `pip`, `uv`, `npm`, `apt-get`, `apk`, `git`, `wget` in CI containers | `test/acceptance/` |
| Differential | Python on ports A vs pkgreg on ports B over the same migrated cache; diff status, headers (minus `Date`/`Server`), body bytes | `test/differential/` |
| Load | 1 000 concurrent readers over 2 GB streams; 24 h soak watching RSS and goroutine count | `test/load/` |
| Fuzz | Path resolution, digest parsing, manifest parsing, PEP 503 HTML | `FuzzXxx` in-package |

**Differential corpus** — the contract that says the rewrite is faithful:

- pypi simple index for `numpy`, `torch`, `grpcio` in **both** HTML and JSON; wheel
  GET, HEAD, `Range: bytes=1000-2000`, `.metadata` sidecar
- npm packument for `chalk` and `@types/node` (percent-encoded scope); tarball GET
- OCI `/v2/`, multi-arch tag, child manifest by digest, blob GET+HEAD, `tags/list`
  for a global and a project pull
- apt `InRelease` with and without a retired validator; a `.deb`; an `.apk`
- files empty-root autoindex, missing GET, and hard-offline PUT/overwrite/DELETE refusal
- git `info/refs`, full clone, shallow clone, `--filter=blob:none` clone
- every legacy read endpoint used by the retired anonymous console

Byte equality for cached content; semantic equality (normalised key order) for
generated JSON. Hop-by-hop framing, opaque validators, deployment-specific
storage/listener values, and rolling historical telemetry are explicitly excluded
per case; stable API fields remain compared.

---

## 9. Milestones and parallelisation

### 9.1 Demoable milestones

| M | After | Demo |
|---|---|---|
| **M1** | P1 | `pkgreg` stores, dedups and serves a blob with Range support; metrics live |
| **M2** | P2 | 20 clients pull one 2 GB wheel; upstream sees exactly one request |
| **M3** | P3 | `pip install`, `uv sync`, `npm ci`, `wget` all work against pkgreg |
| **M4** | P4+P5 | All six ecosystems serve real clients |
| **M5** | P6+P7 | Console (unmodified, via the shim) drives pkgreg end to end |
| **M6** | P8 | Air-gap round trip on two hosts, no DVC, no git-bundle |
| **M7** | P9 | GC reclaims a deleted project; eviction holds a size cap under load; two instances federate |
| **M8** | P10 | Migrated production deployment; differential suite clean |

**M2 qualification:** PASS on 2026-07-27. Twenty concurrent clients each verified
the complete 2 GiB artifact, the origin received exactly one request, and the cache
committed one 2 GiB blob. See the [measured load report](load/m2-2gb.md).

**M4 qualification:** PASS on 2026-07-27. Phase 4 exercises real Docker, apt, and
apk clients, while Phases 3 and 5 exercise pip, uv, npm, wget, Git, and Git LFS.
See the [Phase 4](phase4-oci-apt.md) and [Phase 5](phase5-git.md) reports.

**M8 qualification:** PASS on 2026-07-28. A corrected fresh migration of the 119 GB
logical cache completed in 52.717 s and resumed with zero work in 0.913 s. The live
hard-offline Python/Go differential suite passed 46/46 cases. See the
[Phase 10 report](phase10-cutover.md).

**M7 qualification:** PASS on 2026-07-27. Deleted-project GC preserves shared
content, eviction reaches its LRU size target while retaining checkpoint pins, and
an offline instance fetches verified content from an authenticated sibling. See the
[Phase 9 report](phase9-scale-features.md).

### 9.2 Three-engineer split

Serial spine: **P0 → P1 → P2 → P3-01/02**. After that:

```
Track A (data plane)     P3-03..06 → P4 → P5
Track B (control plane)  P7 → P8
Track C (platform)       P6 → P9 → P10-05..08
```

Track B needs only the `catalog.Store` and `config` contracts from P1, so it can start
as soon as those land. Console work (P10-01..04) joins Track C once P7-09 exists.
Integration points: `eco.Ctx` (frozen at P3-01) and `catalog.Store` (frozen at P1-11)
— **freeze both before splitting**, and change them only by agreement.

---

## 10. Metrics to ship from day one

```
pkgreg_requests_total{eco,project,outcome}        outcome: hit|dedup|peer|miss|fail
pkgreg_bytes_served_total{eco,project}
pkgreg_upstream_bytes_total{eco,upstream}
pkgreg_fetch_duration_seconds{eco}                histogram
pkgreg_inflight_fetches{eco}                      gauge
pkgreg_blob_store_bytes                           gauge
pkgreg_blob_count                                 gauge
pkgreg_catalog_query_seconds{query}               histogram
pkgreg_gc_reclaimed_bytes_total
pkgreg_evicted_entries_total{project,reason}
pkgreg_job_duration_seconds{action,status}
pkgreg_upstream_errors_total{upstream,code}
```

Hit rate, bytes saved, and time saved become derived queries over these, replacing the
bespoke `bandwidth_samples` table and its per-role median blend.

---

## 11. Risk register

| # | Risk | L | I | Mitigation | Trigger to act |
|---|---|---|---|---|---|
| R1 | `modernc.org/sqlite` too slow or buggy | Low | High | S1 spike; CGO+musl fallback documented | S1 misses a target |
| R2 | Progressive delivery bug corrupts a large artifact | Low | **Critical** | S2 as a permanent test; `-race` in CI; concentrated review on `inflight.go`; digest verified before commit | any digest mismatch in load tests |
| R3 | Migration of 119 GB too slow or loses data | Med | High | Resumable; runs against the live stack; verify pass compares digests; Python kept installed for one release | migration > 4 h or any verify failure |
| R4 | Scope creep from phase 9 features | **High** | Med | Phases 0–8 are the parity cut and ship independently (§11.1) | phase 8 finishes late |
| R5 | Blob-only storage blocks an unforeseen client need | Low | High | `ManagedDir` escape hatch already exists; `pkgreg export-tree` materialises a URL-shaped tree on demand | any client requires on-disk paths |
| R6 | Ecosystem interface proves too narrow at oci/git | Med | Med | Deliberately validated by three different shapes in P3 before oci/git; `Ctx` is extensible without breaking adapters | P4-01 or P5-03 needs a special case |
| R7 | Go client-library gaps for a future ecosystem | Low | Low | Gitea implements ~20 ecosystems in Go — precedent exists | — |
| R8 | Air-gapped side migration | Med | High | Re-seed from an online export rather than migrating its DVC history | — |

### 11.1 The reduced cut

If the schedule needs cutting, **phases 0–8 (≈79 d / 15 weeks solo)** deliver full
parity on the new foundations. Phase 9 (GC, eviction, quotas, federation) is deferred
without redesign, because the data model already supports it.

I would not defer past that. A 119 GB cache with no eviction is the next thing to
break, and it is the one problem the current architecture cannot be extended to solve.

---

## 12. Before writing code

1. Confirm the seven decisions in [go-architecture.md §16](go-architecture.md).
2. Run phase 0 and commit the spike reports.
3. Freeze `catalog.Store` (P1-11) and `eco.Ctx` (P3-01) — the two contracts every
   parallel track depends on.
4. Stand up CI with `-race`, lint, and the acceptance-client containers **before**
   P1-02, so no phase accumulates untested code.
