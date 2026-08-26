# pkgreg — Go rearchitecture plan

Status: **implemented, with recorded final-state differences**. Written 2026-07-27.
Supersedes the original port plan, which described the conservative
1:1 port and is kept for comparison.

> This document preserves the design reasoning. Two things in the shipped system
> deliberately differ from the proposal, and both are recorded rather than edited away.
> The frontend is not §11.5: it is checked-in HTML/CSS/ES modules embedded in every
> build, with no React, Node, bundler, or `embedconsole` build tag. And OCI's upstream
> schema is no longer only the named set §3 describes — a registry named by the image
> path resolves without being configured at all, so the named set is a floor for
> credentials and overrides rather than the whole list of reachable registries. See
> the [system overview](system-overview.md).

**Goal:** one static Go binary, deployable on bare metal with no containers, that
does everything the current stack does — and is structured so that adding an
ecosystem, adding a tenant, or adding a site is cheap.

This is not a transliteration. The current design has three structural limits that a
straight port would carry forward, and this plan removes them.

---

## 1. What's structurally wrong today

Not a criticism of the code — it's clean, and the comments are unusually good. These
are consequences of how it grew (six separate proxies unified into one process).

### 1.1 State is sharded by (project × role), so nothing can be asked globally

`build_core()` constructs a `Storage`, a **`Ledger` (its own SQLite file)**, an
`Upstream` (its own connection pool), a `Progress` and a `Stats` **per (project,
role)** (``core/__init__.py:38``).

This checkout already has **24 `ledger.db` files** for 4 projects. Ten projects would
be 60 databases and 60 HTTP connection pools.

The direct consequences:

- Every cross-cutting question needs a fan-out and a manual merge. `/api/stats`
  fetches six endpoints concurrently and reassembles them in ~80 lines of Python
  (``reads.py:70``) — logic that is one `GROUP BY`
  in a single database.
- No query can span projects ("what is `torch` costing us across all teams?") or span
  ecosystems ("top 20 artifacts by size on this host").
- The bandwidth-sample median has to be computed per-role and then blended, because
  no single ledger sees all of it.
- Adding a project multiplies fixed cost by 6 instead of adding a row.

### 1.2 Content is stored path-first, so dedup, GC and eviction are all impossible

Artifacts live at URL-shaped paths inside per-project trees, with a sha256 CAS
hardlinked *alongside* as an optimisation
(``storage.py:114``). That CAS only
engages when the digest is known **before** the download — pypi index hashes and OCI
digests. npm tarballs and apt `.deb`s are explicitly excluded by that comment, so the
same bytes are stored once per project.

More seriously, path-first storage means:

- **No garbage collection.** Deleting a project from the registry deliberately leaves
  its bytes on disk (``projects.py:226``),
  because nothing knows what else references them.
- **No eviction.** `package_stats.last_access` exists with the comment *"leaderboard +
  future LRU eviction"* — nothing consumes it. This cache is **119 GB** and grows
  monotonically. This is the failure mode that arrives first in production.
- **No quotas.** A single team can fill the disk for everyone.

### 1.3 Adding an ecosystem means editing ten places in two codebases

The claim in ``repositories.py`` is
*"Adding a 5th ecosystem is: implement Repository, add it here, add a compose
service."* In practice it is also:

> The files below are from the Python implementation, which has since been removed. The
> table is kept because it is the argument for the design that replaced it: one list, in
> one place, rather than nine copies that drift.

| File | Table |
|---|---|
| `pkgcache/core/config.py` | `_DEFAULT_PORTS`, `_ROLE_SUBDIR`, `_HTTPS_ROLES`, `_ALL_ROLES` |
| `webui/app/services/projects.py` | `ROLES`, `ROLE_PORT`, `ROLE_SUBDIR` |
| `webui/app/settings.py` | `ECOS` |
| `webui/app/manifest.py` | `ECOS` |
| `webui/app/urls.py` | `_ECO_ROLE`, `_ECO_SCHEME`, `_PROGRESS_PATH`, and a hand-written `endpoints()` block |
| `webui/app/gateways/pkgcache.py` | `_ECO_ROLE`, `_ROLES` |
| `webui/app/services/reads.py` | `_ECO_ROLE` |
| `webui/app/services/usage.py` | `_SUBDIRS` |
| `scripts/cas_dedupe.py` | `ROLE_SUBDIRS` |
| `console/src/lib/types.ts` | the eco union type + per-eco UI |

Eight duplicated mapping tables, verified by grep. Three independent copies of
`_ECO_ROLE` alone. Every one is a place a seventh ecosystem gets forgotten.

### 1.4 Smaller things that bite at scale

- **Upstreams are static YAML.** Adding an internal PyPI index or an OCI mirror means
  editing `pkgcache.yaml` on disk and restarting. No per-project overrides.
- **No upstream credentials** — anonymous pulls only, so private registries are out of
  scope by construction (``upstream.py:5``).
- **Config changes take ~5 s** and arrive by polling a JSON file
  (``__main__.py:_POLL``); the files write token
  is separately mtime-cached and re-read on the PUT path.
- **Jobs are globally serialised** — a checkpoint of project A blocks a lockwarm of
  project B (``jobs.py:_busy``).
- **No metrics, no structured logs, no tracing.** Observability is one JSON progress
  endpoint polled every 1.5 s.
- **The data plane is entirely anonymous.** No per-client identity, no read tokens, no
  audit of who pulled what.
- **Single instance by construction** — progress and single-flight are process-local,
  and `progress.py` says so explicitly. No path to a second node or a second site.

---

## 2. The new model

Four concepts replace the six ad-hoc storage schemes. Everything else is built on
them.

```
Blob      immutable bytes, addressed by sha256.  One copy per host, ever.
Entry     (project, eco, key) → blob.            The byte cache. What a GET resolves.
Ref       (project, eco, name) → target + freshness.  Mutable pointers.
Artifact  (project, eco, name, version, arch) → blob.  The semantic inventory the UI shows.
```

**`Ref` is the generalisation that pays off most.** Today, four different mechanisms
do the same job: OCI's `oci_tags` table, git's `git_refs` table, apt's `.meta`
ETag/Last-Modified sidecar files, and npm's implicit "re-fetch the packument every
time". They are all *a mutable name pointing at immutable content, with a freshness
policy*. One table, one revalidation path, one offline story ("resolve the ref from
the last known target"), one place to fix a bug.

**`Entry` + `Blob` is what makes dedup, GC and eviction fall out for free:**

- Dedup becomes universal, not digest-known-in-advance-only. We already hash every
  byte while streaming (``inflight.py:90``),
  so the npm tarball two projects both pull is *already* known to be the same blob —
  today we just throw that knowledge away.
- Deleting a project is `DELETE FROM entries WHERE project = ?`. GC then reclaims any
  blob with no remaining referent.
- Eviction is `SELECT blob FROM entries ORDER BY last_access LIMIT …` — the query
  `package_stats.last_access` was always meant to serve.
- Per-project size accounting becomes exact, and *shared* bytes can be attributed
  honestly ("47 GB total, 31 GB shared with other projects").

### 2.1 Two storage modes, honestly separated

Not everything is a blob. Git mirrors are live repositories that `git upload-pack`
runs against; they cannot be content-addressed as a unit.

| Mode | Used by | Storage |
|---|---|---|
| `blob` | oci, npm, pypi, apt/apk, files, git-LFS | CAS + catalog. No URL-shaped directories on disk at all. |
| `managed-dir` | git mirrors | A directory the ecosystem owns. Snapshotted as a tree of blobs; materialised by hardlink. |

Dropping URL-shaped directories for blob-mode ecosystems is a real change. Nothing in
the system requires them — every client goes through the HTTP API, and the files
role's autoindex becomes a catalog query (`WHERE key LIKE 'prefix/%'`) rather than a
`readdir`, which is both faster and always consistent. The cost is that an operator
can no longer `ls` the cache; `pkgreg ls`, `pkgreg cat` and `pkgreg export-tree`
replace that.

---

## 3. Extensibility: the Ecosystem interface

This is the answer to §1.3. **One declaration site.** A new ecosystem is one package
implementing one interface plus one line in a registry.

```go
type Ecosystem interface {
    Descriptor() Descriptor
    Routes() []Route                       // relative to /<project>/<eco>/
    Resolve(ctx context.Context, rq *Request) (*Resolution, error)
}

type Descriptor struct {
    ID          string        // "npm", "pypi", "oci", "apt", "git", "files", "cargo", …
    Display     string        // "npm"
    Storage     StorageMode   // Blob | ManagedDir
    Listener    ListenerKind  // PathPrefixed | ProtocolRooted (/v2/…) | ForwardProxy (apt)

    // Everything the console, the API and the docs currently hardcode in eight
    // places is declared here, once:
    UpstreamSchema  UpstreamSchema   // named-set (pypi indexes, oci registries) | single | none
    ClientSetup     func(SetupCtx) []SetupStep   // the copy-paste instructions urls.py hardcodes
    Freshness       FreshnessPolicy  // which keys are immutable vs revalidated
    ArtifactParser  func(key string) (name, version, arch string, ok bool)
}
```

`Resolution` is the crucial decoupling — an ecosystem handler **describes** what it
wants; the engine executes it. Handlers stop containing fetch, hash, single-flight,
commit and ledger logic:

```go
type Resolution struct {
    Kind        ResolutionKind  // ServeBlob | Passthrough | Generated | NotFound
    Key         string          // cache key within (project, eco)
    Upstream    *UpstreamReq    // URL, headers, auth ref
    ExpectSHA   string          // when known up front (pypi hashes, OCI digests)
    ExpectSize  int64
    MediaType   string
    Ref         *RefSpec        // when this GET should resolve/revalidate a ref
    Artifact    *ArtifactSpec   // what to record in the inventory on commit
    Rewrite     RewriteFunc     // for indexes/packuments: rewrite URLs to point back at us
}
```

Compare with today: ``pypi.py`` is 293
lines, of which maybe 120 are PyPI-specific (PEP 503/691 parsing and rendering) and
the rest is cache plumbing repeated in five other files. Under this interface each
ecosystem shrinks to its protocol logic.

**What this buys concretely:**

- A seventh ecosystem — crates.io, Go modules, Maven, RubyGems, NuGet, Helm — is one
  package and one registration line. The console renders it, the API exposes it, the
  snapshot includes it, the endpoints panel documents it, with no other edits.
- Third-party ecosystems become plausible later (Go plugins are awkward; an
  out-of-process ecosystem over a small gRPC/unix-socket contract is the realistic
  path, and this interface is already the right shape for it).

---

## 4. Architecture

```
                       ┌──────────────── one process, one binary ────────────────┐
  clients              │                                                          │
  ───────              │   Listeners                                              │
  docker  ──┐          │   ├─ :443/:8443  TLS ─┐                                  │
  pip/uv  ──┤          │   ├─ :3142       plain├─ optional single-port mux        │
  npm     ──┼─────────▶│   └─ :8088       admin┘   (peek 1 byte: 0x16 ⇒ TLS)      │
  apt/apk ──┤          │            │                                             │
  git     ──┤          │            ▼                                             │
  wget    ──┘          │   Router  ── project + ecosystem resolution               │
                       │            │                                             │
                       │            ▼                                             │
                       │   Ecosystem adapters   oci npm pypi apt git files …      │
                       │            │  (protocol only — Resolution structs)        │
                       │            ▼                                             │
                       │   ╔═══════════════════════════════════════════════╗      │
                       │   ║  Cache Engine                                 ║      │
                       │   ║   single-flight · progressive delivery ·      ║      │
                       │   ║   hash-on-write · freshness · refs · events   ║      │
                       │   ╚═══════════════════════════════════════════════╝      │
                       │        │                │              │                 │
                       │        ▼                ▼              ▼                 │
                       │   Blob store       Catalog DB     Upstream pool          │
                       │   blobs/sha256/    catalog.db     (+ credentials,        │
                       │   (+ managed dirs)                 + peer federation)    │
                       │        │                │                                │
                       │        └────────┬───────┘                                │
                       │                 ▼                                        │
                       │   Snapshots · GC · Eviction · Export/Import              │
                       │                                                          │
                       │   Control plane: control.db · API v1 · console (embed)   │
                       │   Observability: slog · /metrics · event bus · SSE       │
                       └──────────────────────────────────────────────────────────┘
```

### 4.1 Layer boundaries

| Layer | Knows about | Must not know about |
|---|---|---|
| Listener/Router | projects, ecosystems, TLS | protocols, storage |
| Ecosystem adapter | its protocol only | SQL, files, HTTP clients, single-flight |
| Cache engine | blobs, entries, refs, upstreams | any specific ecosystem |
| Storage | bytes and rows | HTTP |
| Control plane | everything, read-only + explicit mutations | request handling |

The current code violates the third row constantly — `npm.py` writes `metadata.json`
with its own atomic-rename dance, `apt.py` hand-rolls a streaming download and a
`.meta` sidecar, `pypi.py` caches `simple.json` itself. All three become
`Resolution{Kind: Generated, Ref: …}` and disappear.

---

## 5. Data layer

### 5.1 Two SQLite databases, not 6N

`control.db` — projects, users, sessions, upstreams, credentials (sealed), tokens,
quotas, ownership, audit log, jobs. Small, low write rate, transactional.

`catalog.db` — blobs, entries, refs, artifacts, access stats, snapshots. Large, high
write rate.

Splitting them means a `uv sync` storm hammering the catalog never contends with a
login or a project create. Both are `modernc.org/sqlite` (pure Go → genuinely static
binary, no CGO).

```sql
CREATE TABLE blobs (
  sha256      TEXT PRIMARY KEY,
  size        INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  last_access INTEGER NOT NULL,       -- eviction key
  refcount    INTEGER NOT NULL        -- maintained by triggers on entries/snapshots
);

CREATE TABLE entries (                -- the byte cache
  project     TEXT NOT NULL,
  eco         TEXT NOT NULL,
  key         TEXT NOT NULL,          -- e.g. "root/pypi/+f/numpy/numpy-2.0-…whl"
  sha256      TEXT NOT NULL REFERENCES blobs(sha256),
  media_type  TEXT,
  cached_at   INTEGER NOT NULL,
  last_access INTEGER NOT NULL,
  hits        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project, eco, key)
) WITHOUT ROWID;

CREATE TABLE refs (                   -- oci tags | git refs | apt Release | npm dist-tags
  project     TEXT NOT NULL, eco TEXT NOT NULL, name TEXT NOT NULL,
  target      TEXT NOT NULL,          -- digest, commit sha, or entry key
  etag        TEXT, last_modified TEXT,
  fetched_at  INTEGER NOT NULL, ttl_seconds INTEGER NOT NULL,
  PRIMARY KEY (project, eco, name)
) WITHOUT ROWID;

CREATE TABLE artifacts (              -- the semantic inventory
  project TEXT, eco TEXT, name TEXT, version TEXT, arch TEXT,
  sha256 TEXT REFERENCES blobs(sha256), size INTEGER,
  origin TEXT, cached_at INTEGER, extra TEXT,
  PRIMARY KEY (project, eco, name, version, arch)
) WITHOUT ROWID;
```

Now `/api/stats` — currently a six-way HTTP fan-out plus 80 lines of merge — is:

```sql
SELECT eco, COUNT(*), SUM(size), SUM(hits) FROM artifacts
  JOIN blobs USING (sha256) WHERE project = ? GROUP BY eco;
```

And the questions that are impossible today become one-liners: cross-project
duplication, host-wide top-N, per-team spend, blob sharing ratio.

**Write-path durability.** The blob is fsync'd and linked into the CAS **before** the
entry row is written, so a lost entry insert costs a re-fetch, never corruption. That
makes it safe to batch entry inserts into ~100 ms transactions — which is what keeps a
`uv sync` burst of thousands of small files from becoming thousands of fsyncs.

**Hot-path cache.** A small in-memory LRU of resolved entries (key → blob, size,
media type) in front of the catalog, so the hottest keys never touch SQLite.

**The seam for later.** The catalog sits behind a `Store` interface. One
implementation ships (SQLite). Postgres becomes possible for multi-instance without
touching call sites. Do not build the Postgres backend now.

### 5.2 Blob store

```
<data-dir>/blobs/sha256/<aa>/<bb>/<hex>       0644, immutable once linked
<data-dir>/blobs/staging/<random>.part        in-flight
<data-dir>/managed/git/<host>/<owner>/<repo>.git
```

Write: stream to `staging/`, hash inline, fsync, `os.Link` to the final path, fsync
dir, unlink staging. Idempotent — an existing target means someone else won the race
with identical bytes.

Read: `http.ServeContent` on the open file. That gives Range, `If-Range`,
`If-None-Match`, `If-Modified-Since` and multipart ranges for free, and lets the
kernel `sendfile` the body. Strictly more correct than the current
`FileResponse`-based path, which is already the fix for the original devpi defect.

**Deleting is always safe** because blobs are immutable and refcounted; nothing ever
rewrites a blob in place. That property is what makes concurrent GC and live
snapshots safe without quiescing anything.

---

## 6. The request pipeline

One path, all ecosystems:

```
request
  └─▶ router: project + ecosystem + strip prefix
      └─▶ adapter.Resolve() → Resolution
          ├─ Generated   : adapter renders (index/packument/autoindex), engine caches + rewrites URLs
          ├─ ServeBlob   : engine handles it:
          │     1. entry hit?            → ServeContent (sendfile)         [HIT]
          │     2. blob known by digest? → link entry → ServeContent       [DEDUP HIT]
          │     3. peer has it?          → fetch from sibling instance     [PEER HIT]
          │     4. offline?              → 404 with a clear reason
          │     5. single-flight fetch   → progressive tail-follow         [MISS]
          └─ Passthrough : stream, do not cache (health, negotiation)
```

Steps 1–2 are new for npm/apt: because every download is hashed inline, a blob one
project fetched is available to every other project even when the digest was not
known in advance. Today that only works for pypi and OCI.

### 6.1 Single-flight and progressive delivery — kept, and hardened

This is the best thing in the current codebase
(``inflight.py``) and it ports intact: one
goroutine streams upstream → staging file while N readers tail-follow it. Go improves
three things:

- The fetch goroutine runs on a **detached context**, so a client disconnect can never
  abort a download other readers and the cache still want. (The Python version gets
  this right by accident of task isolation; here it is explicit.)
- `sync.Mutex` + a closed-and-replaced broadcast channel replaces `asyncio.Condition`.
- `HEAD` and `Range` still wait for the committed blob rather than riding the stream,
  then hand off to `ServeContent`.

Every state change emits to the **event bus** (`fetch.start`, `fetch.progress`,
`fetch.done`, `cache.hit`). The in-memory progress registry becomes one subscriber;
metrics and the console SSE stream are others. That is what removes the 1.5 s polling
loop and, later, allows merging progress across instances.

### 6.2 Freshness, unified

`FreshnessPolicy` on the descriptor replaces four bespoke mechanisms:

```go
type FreshnessPolicy interface { Classify(key string) Freshness }  // Immutable | Revalidate(ttl) | Never
```

- apt `InRelease`/`Packages*` → `Revalidate` with ETag/Last-Modified — the `.meta`
  sidecar files vanish into the `refs` table.
- OCI tags → `Revalidate`; digests → `Immutable`.
- npm packuments → `Revalidate(60s)` — an actual improvement, since today every
  packument request hits upstream (``npm.py:61``).
- pypi simple indexes → `Revalidate(300s)`; wheels → `Immutable`.
- git refs → `Revalidate(refs_ttl)`.

Offline mode becomes one rule at the engine level — *never revalidate, serve last
known* — instead of an `if self._core.config.offline` branch in every handler (there
are currently eleven).

---

## 7. Control plane

### 7.1 Config: one authoritative snapshot, atomic swap, no polling

```go
type Store struct{ cur atomic.Pointer[Snapshot] }
func (s *Store) Current() *Snapshot   // lock-free read on every request
func (s *Store) Apply(mutation) error // write control.db, rebuild, swap, notify
```

Replaces: the 5-second registry poll, the mtime-cached `files_token()` re-read on the
PUT path, `RoleServer.reconcile`'s diffing, and `mode()`'s 15-second confirmation
loop. A project created in the console is routable on the next request, not in five
seconds.

### 7.2 API v1 — resource-oriented and descriptor-driven

```
GET    /api/v1/ecosystems                     ← descriptors; the console renders from these
GET    /api/v1/projects  POST  /api/v1/projects
GET    /api/v1/projects/{p}                   PATCH (mode, quota, owner)  DELETE
GET    /api/v1/projects/{p}/artifacts?eco=&q=&sort=&page=
GET    /api/v1/projects/{p}/endpoints         ← generated from Descriptor.ClientSetup
GET    /api/v1/projects/{p}/upstreams         POST/PATCH/DELETE   ← live, no restart
GET    /api/v1/projects/{p}/snapshots         POST (checkpoint)
POST   /api/v1/projects/{p}/snapshots/{id}/rollback
POST   /api/v1/projects/{p}/export            POST /api/v1/projects/{p}/import
GET    /api/v1/stats?project=&eco=&group_by=  ← one SQL query
GET    /api/v1/events                         ← SSE: progress, jobs, health
GET    /api/v1/jobs        GET /api/v1/jobs/{id}
GET    /metrics                               ← Prometheus
```

`/api/v1/ecosystems` is the extensibility payoff at the UI layer: the console
enumerates ecosystems, renders their endpoint instructions and their upstream-config
forms from the descriptor, so a new ecosystem appears in the UI with **no frontend
change**. Today `urls.endpoints()` hardcodes all seven blocks in Python and the
console hardcodes the matching union type in TypeScript.

The existing `/api/*` routes stay as a thin compatibility shim for one release so the
current console keeps working during the transition.

### 7.3 Auth, extended to the data plane

Keep: `user < admin < superuser`, the reporting graph, project ownership, scrypt
hashing with **identical parameters** so existing `users.json` hashes verify
unchanged, opaque server-side sessions, the CSRF origin guard, per-IP login throttle.

Add:

- **Service accounts / API tokens** — `pkgreg-<id>-<secret>`, scoped to
  (project, ecosystem, read|write), for CI. Replaces the single shared files write
  token, which today is one string per project with no rotation story beyond "rotate
  and update every consumer".
- **Optional data-plane auth per project** — `public` (today's behaviour, the default)
  or `token`. Some teams need a private cache; today that is impossible.
- **Upstream credentials**, sealed at rest with a host key, so private registries
  (private ghcr, an internal Artifactory, a paid index) can be cached at all.
- **Audit log** — who did what, when, from where; and optionally who pulled what.

### 7.4 Jobs, per-project

`Jobs` keeps the streaming-log model (it works well) but serialises **per project**
rather than globally, with a bounded worker pool. Checkpointing team A no longer
blocks team B. Job records persist in `control.db` so history survives a restart.

---

## 8. Air-gap, redesigned around the model

DVC and git disappear. Not by reimplementing them — by not needing them.

**A snapshot is a content-addressed manifest.** Sorted `key → sha256` lines, gzipped,
stored as a blob like anything else. A snapshot row is therefore ~100 bytes:

```sql
CREATE TABLE snapshots (
  id TEXT PRIMARY KEY, project TEXT, parent TEXT,
  manifest_sha256 TEXT REFERENCES blobs(sha256),
  created_at INTEGER, subject TEXT, author TEXT
);
```

| Operation | Implementation |
|---|---|
| **Checkpoint** | Stream the current entry set for the project, sorted, into a manifest blob. One row. Cheap and constant-time in the number of *changed* files — no tree walk, no re-hashing, because entries already carry their digests. |
| **History** | `SELECT … FROM snapshots ORDER BY created_at DESC` |
| **Rollback** | Diff the target manifest against live entries; apply. Blobs are already present (never GC'd while a snapshot references them). |
| **Export A→B** | Linear merge of two sorted manifests → the blob set present in B and not A. Stream a tar: header, manifest, blobs. |
| **Import** | Verify each blob's sha256 against its name, link into the store, apply the manifest, append the snapshot row. Non-fast-forward is rejected by parent-pointer check. |

The checkpoint cost matters at 119 GB. Today `dvc add` walks and stats the whole tree;
here the digest is already in the catalog from when the bytes were written, so a
checkpoint is a `SELECT`.

This also deletes, permanently: `_find_md5_tree` and `_normalize_dvcstore` (150 lines
of archaeology for the ways an operator's copy mangles `dvcstore/files/md5`),
`_unshare_ledgers` (repairing hardlink damage to a live SQLite file),
`_use_shared_dvc_cache`, `_ignore_path_in_repo`, the git-bundle fast-forward dance,
and the `--force` flags that exist because DVC sees the live `ledger.db` as an
unsaved change.

**Git mirrors** (managed-dir mode) snapshot as a tree of blobs — walk, hash, record —
which is the one place a walk is still needed. Mitigated by the geometric-repack step
that already exists.

---

## 9. Scaling

### 9.1 Garbage collection

Mark-and-sweep, online, no quiescing (safe because blobs are immutable):

1. Mark every blob reachable from `entries`, `artifacts`, `snapshots` (manifests are
   blobs too, and their contents are roots).
2. Sweep unmarked blobs older than a grace period (default 1 h — protects blobs
   written but not yet entry-committed).

`pkgreg gc [--dry-run]`, plus a scheduled run. Deleting a project finally reclaims
disk.

### 9.2 Eviction

Policies per project and host-wide, driven by the columns the catalog already has:

- **LRU** to a target size or free-space floor
- **TTL** — drop entries unaccessed for N days
- **Pinning** — never evict what a snapshot references (air-gap safety), or an
  explicit pin list

Eviction removes *entries*; GC reclaims the blobs. A blob shared by three projects
survives until the last entry goes. This is the single most important missing feature
for a cache that has already reached 119 GB.

### 9.3 Quotas

Per-project byte and artifact-count limits, enforced at commit. Exposed in the console
alongside the existing disk panel.

### 9.4 Federation — the horizontal scaling story

An upstream is a URL, and a pkgreg instance is a valid upstream for another pkgreg
instance. Make that explicit:

```
build hosts ─▶ site cache (branch office) ─▶ regional cache ─▶ internet
```

Add a **peer** upstream kind with a digest-first protocol
(`HEAD /peer/v1/blob/<sha256>` → `GET`), so a sibling is consulted before the internet
without re-resolving the ecosystem's index. Then:

- Multiple hosts scale reads horizontally behind a load balancer, each with its own
  catalog, sharing nothing.
- A site cache warms itself from a regional one over a fast link.
- It composes with air-gap: the offline side can peer with other offline instances.

This is a far better scale-out story than shared-state clustering, and it needs no new
storage semantics.

### 9.5 Single-instance limits, made explicit

The three process-local components (single-flight, progress registry, catalog writer)
sit behind interfaces from day one, so a coordinated implementation is a drop-in
later. Shipping target remains one instance per host; the design just stops
*preventing* the alternative.

---

## 10. Observability

| | |
|---|---|
| **Logs** | `log/slog`, JSON, one structured access line per request: project, eco, key, outcome (hit/dedup/peer/miss/fail), bytes, duration, upstream. |
| **Metrics** | Prometheus at `/metrics`: `pkgreg_requests_total{eco,project,outcome}`, `pkgreg_bytes_served_total`, `pkgreg_upstream_bytes_total`, `pkgreg_fetch_duration_seconds`, `pkgreg_blob_store_bytes`, `pkgreg_inflight_fetches`, `pkgreg_catalog_query_seconds`. |
| **Events** | Internal bus; subscribers = progress registry, SSE stream, metrics, audit log. |
| **Console** | SSE replaces 1.5 s polling for downloads/recent/jobs/health. |
| **Health** | `/healthz` (liveness), `/readyz` (DBs open, blob store writable, listeners bound). |
| **Tracing** | OpenTelemetry-shaped span boundaries in the engine, exporter off by default. |

The existing "estimated time saved" and bandwidth-sample machinery survives, but as a
derived metric over real counters rather than a bespoke `bandwidth_samples` table plus
a per-role median blend.

---

## 11. Deployment

### 11.1 One binary, one data directory

```
/usr/local/bin/pkgreg
/var/lib/pkgreg/
  ├── blobs/          content-addressed store
  ├── managed/git/    bare mirrors
  ├── db/             control.db, catalog.db
  ├── certs/          ca.crt, ca.key, server.crt, server.key   (minted natively)
  └── shuttle/        in/, out/
/etc/pkgreg/config.yaml    optional; flags > env > file > defaults
```

### 11.2 CLI

```
pkgreg init                     # data dir, CA + server cert, admin account, config
pkgreg serve
pkgreg systemd install          # writes + enables the unit
pkgreg project create|list|delete|quota
pkgreg checkpoint|rollback|export|import
pkgreg gc [--dry-run]  |  pkgreg evict --target-size 500G
pkgreg token create --project p --scope files:write
pkgreg upstream add|list|remove
pkgreg ls|cat|stat              # inspect the cache without a shell in the tree
pkgreg migrate from-python --src ./caches
pkgreg doctor                   # config, certs, perms, disk, git binary, upstream reachability
```

Every mutating subcommand talks to a running instance over a unix socket when one
exists (so the CLI and the console take the same path), and operates directly on the
data dir when it does not.

### 11.3 Single port (optional, default on)

Peek the first byte of each connection: `0x16` → TLS handshake; anything else → plain
HTTP, which is either an apt forward-proxy request (absolute-form target) or a plain
client. ~60 lines, well-trodden. Collapses three firewall rules to one. Explicit
per-listener config remains available.

Because the forward proxy currently relays to **any** host by design, add an optional
upstream allowlist at the same time.

### 11.4 What disappears

`docker-compose.yml`, three Dockerfiles, nginx + `nginx.conf`, `bootstrap.sh` and the
whole `PKGCACHE_UID`/`GID` bind-mount-ownership problem, `gen-certs.sh` + the `openssl`
dependency, `scripts/pkgops.py`, `scripts/serve-ui.sh`, `scripts/cas_dedupe.py`,
`scripts/gen_manifest.py`, `scripts/prefetch.py`, DVC, and Python.

Remaining external runtime dependency: **`git`**, for the git mirror role only. That
role reports unavailable with a clear message if the binary is absent; everything else
runs.

### 11.5 Frontend

Keep the React console — it is well-structured and rewriting it buys nothing. Restructure
it against API v1 and make the ecosystem-specific UI descriptor-driven. `go:embed` the
built `dist/` plus the landing and tutorial pages; a Go middleware carries over the
exact CSP and security headers from `nginx.conf`. A `!embedconsole` build tag ships a
stub so `go build`/`go test` work with no Node installed.

---

## 12. Repo layout and dependencies

```
cmd/pkgreg/
internal/
  config/        snapshot store, atomic swap, precedence
  blob/          CAS: write, link, serve, verify, gc
  catalog/       Store interface + SQLite impl (blobs, entries, refs, artifacts, snapshots)
  engine/        single-flight, progressive delivery, freshness, refs, events
  upstream/      pooled clients, credentials, OCI bearer, peer protocol
  eco/           registry + Descriptor/Ecosystem
    oci/ npm/ pypi/ apt/ git/ files/
  gitmirror/     managed-dir lifecycle (git subprocess)
  router/        path router (escape-safe), project resolution, listeners, portmux
  control/       api/v1, auth, accounts, sessions, tokens, projects, jobs, ops
  snapshot/      manifests, export/import, diff
  maintenance/   gc, eviction, quotas, scheduler
  obs/           slog, metrics, event bus, SSE
  pki/           CA + leaf minting
web/console/     React source + embed.go
```

| Dependency | Why |
|---|---|
| `modernc.org/sqlite` | pure Go → truly static binary |
| standard-library `crypto/pbkdf2` + local RFC 7914 mixer | scrypt compatibility without a new module |
| standard-library AES-256-GCM | authenticated credential sealing under the host key |
| `golang.org/x/sync` | errgroup, semaphore |
| `gopkg.in/yaml.v3` | config file |
| standard library | schema-v1 `uv.lock` field parsing and byte-preserving URL rewrite |
| `github.com/prometheus/client_golang` | metrics |

No web framework, no ORM, no logging framework, no go-git. Everything else is stdlib.

---

## 13. Phasing

Each phase ends with something runnable. Phases 1–8 reach **feature parity plus the new
foundations**; 9–10 deliver the capabilities that do not exist today.

| # | Phase | Est. | Exit criterion |
|---|---|---|---|
| 0 | **Spikes** — SQLite catalog at 5 M rows under burst; progressive delivery under `-race`; path-escaping (npm `%2F`, OCI greedy, apt absolute-form); native CA accepted by docker/git/pip; port-mux | 1 w | each green or a documented fallback |
| 1 | **Foundation** — config store, blob store, catalog schema + Store, event bus, slog, metrics | 2 w | blobs written/served/GC'd; catalog benchmarked |
| 2 | **Engine** — single-flight, progressive delivery, freshness, refs, upstream pool, offline | 2 w | generic fetch pipeline passes a synthetic-upstream suite |
| 3 | **Ecosystems A** — files, npm, pypi *(proves the interface with three shapes)* | 2 w | real `pip`, `uv`, `npm`, `wget` clients work end to end |
| 4 | **Ecosystems B** — oci, apt/apk | 2 w | real `docker pull` (multi-arch), `apt-get`, `apk add` |
| 5 | **Git** — managed-dir mode, mirror lifecycle, LFS | 1.5 w | clone/fetch/shallow/filter/LFS parity |
| 6 | **Router + listeners** — escape-safe mux, project resolution, port-mux, graceful shutdown | 1 w | port of the existing router/unified test suites |
| 7 | **Control plane** — control.db, auth, tokens, projects, upstreams, quotas, jobs, API v1 + compat shim | 2.5 w | port of `test_auth`/`test_projects`/`test_ownership`/`test_multiproject` |
| 8 | **Air-gap** — snapshots, checkpoint, rollback, export/import, lockwarm | 2 w | checkpoint → export → import on a second host → rollback, byte-identical |
| 9 | **Scale features** *(new)* — GC, eviction, quotas, peer federation, audit | 2 w | GC reclaims a deleted project; eviction holds a size target under load |
| 10 | **Console + cutover** — API v1 migration, SSE, descriptor-driven UI, embed, migrator, docs | 2.5 w | a real deployment migrated; differential suite clean |

**Solo: ~20 weeks. Three engineers on phases 3/4/5 and 7/8 in parallel: ~9–11 weeks.**

Roughly 15–18 k lines of Go plus 5–6 k of tests, against 5.5 k lines of Python — the
increase is Go's verbosity plus GC, eviction, quotas, federation, tokens, metrics and
upstream management, none of which exist today.

### 13.1 If that is too much

A defensible smaller cut: **phases 0–8 only** (~14 weeks solo). That is full parity on
the new foundations, with GC/eviction/quotas/federation deferred — but the data model
already supports them, so they stay a later increment rather than a redesign. I would
not defer past that: eviction is the feature a 119 GB cache needs next.

---

## 14. Migration

The new storage layout is genuinely different, so this is a real migration, not a
drop-in swap.

**`pkgreg migrate from-python --src ./caches`** — resumable, runs against a live
Python stack:

1. Read each `ledger.db` → artifacts, oci_tags, git_refs, package_stats,
   traffic_stats.
2. Walk each role tree; hash each file; link into `blobs/`; insert the entry with the
   URL-derived key. Existing `.cas` entries are already sha256-named — link them
   directly with no re-hash.
3. Convert `oci_tags` + `git_refs` + apt `.meta` sidecars → `refs`.
4. Move git mirrors into `managed/git/` (a rename when on the same filesystem).
5. `projects.json` + `users.json` → `control.db`. **scrypt parameters are identical,
   so every existing password keeps working.**
6. Take an initial snapshot.

Cost is one full read of the cache: for 119 GB, roughly 1–2 hours at disk speed,
parallelised across cores, resumable, and safe to run while the Python stack keeps
serving.

**Alternative: re-warm.** Point clients at the new instance and let it fill on demand.
Appropriate for the online side; not for the air-gapped side, which must migrate or be
re-seeded from an export.

**Cutover:** run pkgreg on alternate ports against a migrated copy → differential
suite (§15) → swap ports → keep the Python stack installed for one release as
rollback. Clients need no change: same URLs, same certs, same CA.

---

## 15. Verification

Rearchitecting means a differential harness is not optional — it is the main safety
net, and it is cheap (~250 lines).

Run Python on ports A and pkgreg on ports B over independent copy-on-write snapshots
of the same migrated cache; replay a fixed corpus; diff status, protocol headers and
body bytes:

- pypi simple index for `numpy`, `torch`, `grpcio` in **both** PEP 503 HTML and PEP 691
  JSON; a wheel GET, HEAD, `Range: bytes=1000-2000`, and a `.metadata` sidecar
- npm packument for `chalk` and `@types/node` (percent-encoded scope); a tarball GET
- OCI `/v2/`, a multi-arch tag, a child manifest by digest, blob GET + HEAD,
  `tags/list` for a global and a project pull
- apt `InRelease` with and without a retired validator; a `.deb`; an `.apk`
- files empty-root autoindex, missing GET, and hard-offline write/delete refusal
- git `info/refs?service=git-upload-pack`, full clone, shallow clone, filtered clone
- every legacy read endpoint used by the retired anonymous console

Byte equality for cached content; semantic equality (normalised key order) for
generated JSON. Hop-by-hop framing, opaque validators, deployment-specific
paths/ports, and rolling telemetry are declared per case rather than compared as
stable values. Plus real-client acceptance: `docker pull`, `pip install`, `uv sync`,
`npm ci`, `apt-get update && install`, `apk add`, `git clone`, `wget`.

---

## 16. Decisions to confirm before phase 1

1. **Drop URL-shaped directories for blob-mode ecosystems?** (§2.1) — Recommend yes.
   It is what unlocks universal dedup, GC, eviction and exact accounting. Cost: no
   `ls` in the cache tree; mitigated by `pkgreg ls/cat/export-tree`.
2. **One catalog DB, or keep per-project sharding?** — Recommend one. Sharding is the
   root of the fan-out-and-merge code and blocks every cross-cutting query. If very
   large tenants later need isolation, shard *then*, behind the `Store` interface.
3. **Data-plane auth and service tokens** (§7.3) — Recommend building it. It is the
   most-requested thing a shared cache lacks, and retrofitting auth is always worse
   than designing it in.
4. **Federation now or later?** (§9.4) — Recommend now, in phase 9. The peer protocol
   is small and the multi-site story is a large part of "scaling".
5. **Migrate or re-warm?** (§14) — Recommend building the migrator; 119 GB is too much
   to re-fetch, and the air-gapped side has no upstream to re-fetch from.
6. **Full 20-week scope, or the 14-week phases 0–8 cut?** (§13.1)
7. **Keep the legacy `/api/*` shim?** — Recommend yes, for one release.
