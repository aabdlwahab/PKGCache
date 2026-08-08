# Porting package-registry to Go — a single-binary, container-free deployment

> **Superseded by [go-architecture.md](go-architecture.md).** This document is the
> conservative option: a faithful 1:1 port that preserves the current architecture,
> file formats and behaviour. It is kept because its component-by-component mapping
> (routing traps, streaming, SQLite driver choice, cert minting) still applies, and
> because it is the lower-risk fallback if the rearchitecture is judged too large.

Status: **plan / superseded**. Written 2026-07-27.

Goal: replace the current three-container Python + nginx stack with **one statically
linked Go executable** that serves the six cache roles, the control API, and the
console, deployable on bare metal as `./pkgreg serve` behind a systemd unit.

---

## 1. What exists today

| Component | Language / runtime | Source LOC | Role |
|---|---|---|---|
| `pkgcache/` | Python 3.12, Starlette + uvicorn + httpx | ~2,900 | Six pull-through cache roles on 2 ports |
| `webui/app/` | Python 3.12, **stdlib only** (`ThreadingHTTPServer`) | ~2,600 | Control API: projects, auth, jobs, air-gap ops |
| `webui/console/` | React 18 + TypeScript + Vite | ~2,500 | Operator SPA |
| `webui/console/public/` | Static HTML/CSS/JS | ~175 KB | Landing page + porting tutorial |
| Tests | pytest | ~2,650 | pkgcache router/CAS/ledger + webui auth/projects/ops |

Runtime dependencies that are **not** Python packages: `git`, `dvc`, `openssl`,
`docker` + `docker compose`, `nginx` (in the console image), `node` (build only).

### Deployment friction the port is meant to remove

- Three images to build, a compose file with profiles, and a `bootstrap.sh` that
  exists almost entirely to work around bind-mount ownership (`PKGCACHE_UID`/`GID`).
- `dvc` — a large Python application — is a hard runtime dependency of the control
  plane on **both** sides of the air gap.
- The webui talks to pkgcache over **HTTPS against the deployment CA** even though
  the two processes are on the same host, which forces `INTERNAL_TLS`,
  `check_hostname = False`, a 6-role fan-out per stats request, and a 30 s
  "last-good" cache to paper over blips ([`gateways/pkgcache.py`](../webui/app/gateways/pkgcache.py)).
- nginx exists only to serve `dist/` and reverse-proxy `/api`.
- The ledger's async wrapper busy-polls a single-thread executor every 1 ms
  ([`ledger.py:428`](../pkgcache/src/pkgcache/core/ledger.py#L428)) because
  asyncio↔thread handoff was unreliable in the target runtime.

---

## 2. Target architecture

One process. Three listeners. Everything else in-process.

```
pkgreg (single static binary, ~25–35 MB with the embedded SPA)
│
├── :8443  TLS  unified listener
│      /v2/…                    → oci  (project rides the image name)
│      /<project>/<role>/…      → npm | pypi | git | files (+ admin form for oci/apt)
│      /healthz, /
│
├── :3142  plain HTTP  apt/apk forward proxy  (project = proxy username)
│
├── :8088  console + control API
│      /                        → landing.html      (embedded)
│      /tutorial                → tutorial.html     (embedded)
│      /console, /console/*     → SPA shell         (embedded)
│      /assets/*                → fingerprinted bundle, immutable cache
│      /api/*                   → control API
│
├── supervisor goroutine        → re-reads config/projects.json, reconciles roles
├── stats flush goroutine       → per (project, role) core
├── speed-test goroutine        → one per process
└── job runner goroutine        → checkpoint / export / import / rollback / lockwarm
```

### 2.1 The single biggest simplification: kill the internal HTTP hop

Because the control plane and the cache roles now live in the same address space,
the webui's `pkgcache` gateway becomes an **interface with an in-process
implementation**:

```go
// internal/control/cacheview
type CacheView interface {
    Artifacts(project, eco string, q ArtifactQuery) ([]Artifact, error)
    Stats(project string) (map[string]RoleStats, error)   // role → stats
    Progress(project string) map[string]Snapshot          // eco → snapshot
    Health(project string) map[string]RoleHealth
    GitMaintain(project string) (int, error)
    FilesPut(project, rel string, body io.Reader, ...) (PutResult, error)
}
```

Two implementations: `Local` (direct calls into the running `Registry` of cores) and
`HTTP` (today's behaviour, kept so a future split deployment still works). `Local` is
the default.

This deletes, outright:

- `INTERNAL_TLS` and the `check_hostname = False` compromise
- the `_STALE_OK` last-good cache in the ledger gateway
- the **entire `LiveFeed` background poller** — progress becomes a direct read of the
  in-memory `Progress` registry
- `_MODE_CONFIRM_TIMEOUT` polling in `Operations.mode()` — flipping the flag and
  applying it are now the same call
- the `files_proxy` socket-streaming shim — the console upload writes straight into
  the files role's handler

**API compatibility:** `/api/downloads` etc. keep their `age` field (now always
`0.0`) and `/api/proxies` keeps its `services[].state` shape, so the SPA needs no
changes. The `/+ledger/*` HTTP endpoints on the roles stay published — they are a
documented surface and the `HTTP` implementation needs them.

### 2.2 Repository layout

```
cmd/pkgreg/            main.go — subcommand dispatch
internal/
  buildinfo/           version, embedded at link time
  config/              pkgcache.yaml + env + projects.json → Config per (role, project)
  cache/
    storage.go         safe paths, atomic commit, CAS hardlinks, Range serving
    ledger.go          SQLite (schema v3, unchanged)
    inflight.go        single-flight + tail-follow readers
    progress.go        in-flight + recent feed
    stats.go           in-memory deltas, periodic flush
    upstream.go        pooled client + OCI bearer dance
    cache.go           the fetch() facade
    core.go            Core bundle + lifecycle
  roles/
    oci/ npm/ pypi/ apt/ git/ files/
    gitmirror/         bare-mirror lifecycle, `git` subprocess
  router/
    mux.go             custom path router (see §3.1)
    roleserver.go      per-role, per-project dispatch
    unified.go         :8443 namespace split
    supervisor.go      registry reconcile loop
  control/
    api/               route table + controllers + CSRF/origin guard
    accounts/ sessions/ passwords/
    projects/          registry read/write, tokens, ownership
    jobs/              one-at-a-time runner, streaming log
    ops/               checkpoint | export | import | rollback | lockwarm | mode
    reads/ usage/ urls/
    lockwarm/          uv.lock parse → warm → rewrite
  vcs/                 native snapshot store — the DVC + git replacement (§4)
  pki/                 CA + leaf cert minting (replaces gen-certs.sh + openssl)
web/
  console/             React source, unchanged
  embed.go             //go:embed dist landing tutorial
```

### 2.3 Dependency budget

Deliberately small — this project exists to make dependency trees someone else's
problem.

| Module | Why | Alternative if rejected |
|---|---|---|
| `modernc.org/sqlite` | pure-Go SQLite → **no CGO**, true static binary | `mattn/go-sqlite3` (CGO, breaks static linking) |
| `gopkg.in/yaml.v3` | read `pkgcache.yaml` unchanged | convert config to JSON, drop the dep |
| standard library schema-v1 parser | parse canonical `uv.lock` fields without another air-gap dependency | `pelletier/go-toml/v2` if uv broadens the schema |
| standard-library `crypto/pbkdf2` + local RFC 7914 mixer | **verifies existing `users.json` hashes unchanged** | `golang.org/x/crypto/scrypt` |
| `golang.org/x/sync` | `errgroup`, `semaphore` | hand-rolled |

Everything else — HTTP server and client, TLS, x509/CA minting, gzip, tar, sha256,
file serving with Range — is standard library.

Notably **not** included: no web framework, no logging framework, no ORM, no
`go-git` (see §4.3).

---

## 3. Component-by-component port

### 3.1 Routing — write a custom mux, do **not** use `http.ServeMux`

This is the first real trap. Three route shapes in this codebase break Go 1.22's
`ServeMux`:

1. **npm scoped packages.** npm clients send `/@babel%2Fcore` (percent-encoded
   slash). `ServeMux` matches against the *unescaped* path, so `%2F` becomes a real
   segment separator and `/@babel%2Fcore` matches the two-segment `/@{scope}/{pkg}`
   route — with `scope` and `pkg` split wrongly, and worse, `ServeMux` will issue a
   301 to the "cleaned" path first. Starlette matched on the raw path and the handler
   called `unquote` itself ([`npm.py:43`](../pkgcache/src/pkgcache/handlers/npm.py#L43)).
2. **OCI greedy names.** `/v2/{name:path}/manifests/{ref}` — `name` contains slashes
   and the discriminator is a *suffix*. `{name...}` in `ServeMux` must be terminal.
3. **The apt forward proxy** receives absolute-form request targets
   (`GET http://archive.ubuntu.com/... HTTP/1.1`). Go's server parses these correctly
   (`r.URL.Host` is populated), but `ServeMux` will try to match `r.URL.Path` and its
   redirect-on-clean behaviour is actively wrong here.

**Decision:** a ~150-line router in `internal/router/mux.go` that matches on
`r.URL.EscapedPath()`, supports literal segments, `{name}` (one segment, kept
escaped), `{name...}` (greedy, may be non-terminal, anchored by the literal suffix
that follows), and preserves registration order so `/+indexes` still wins over
`/{index...}/+simple/{project}/`. Handlers unescape what they need, as today.

Scope rewriting (`router.py`'s `_strip_prefix` / `_replace_path`) becomes a small
`RequestContext` carried in `r.Context()`:

```go
type reqctx struct {
    Project    string // "global" or a registry name
    RootPath   string // the stripped "/<project>/<role>" prefix; re-emitted by externalBase
    OCIProject string // set when the project rode the image name
}
```

`externalBase(r)` ([`common.py:12`](../pkgcache/src/pkgcache/handlers/common.py#L12))
ports directly, reading `RootPath` from the context instead of `scope["root_path"]`.

### 3.2 Storage

Direct port, and it gets *better*:

| Python | Go |
|---|---|
| `safe_path` via `Path.resolve()` + parent check | `filepath.Join` + `filepath.Clean` + `strings.HasPrefix(dir+sep)` check. Keep the same `UnsafePath` rejection semantics. |
| `open_part` / `commit_part` (tmp → fsync → rename → fsync dir) | identical; `os.File.Sync()`, `os.Rename`, then `os.Open(dir).Sync()` |
| `cas_link_from` / `cas_materialize` (hardlink + atomic publish) | `os.Link` + `os.Rename`; same same-filesystem `st_dev` guard via `syscall.Stat_t` |
| `FileResponse` handles Range | **`http.ServeContent`** — Range, `If-Range`, `If-Modified-Since`, `If-None-Match`, multipart ranges. Strictly more correct than Starlette's. |
| `gc_parts()` rglob at startup | `filepath.WalkDir` |

One behavioural upgrade to take: `.part` temp names currently embed `os.getpid()` and
`id(obj)` — in Go use a random suffix, since `id()` has no analogue and PID reuse
across a restart is a real (if unlikely) collision source.

### 3.3 Ledger (SQLite)

**The schema does not change.** `SCHEMA_VERSION = 3` and all six tables port
verbatim, so an existing `caches/<eco>/ledger.db` is read and written by the Go
binary with no migration.

- Driver: `modernc.org/sqlite` (pure Go). Same pragmas: `journal_mode=WAL`,
  `synchronous=NORMAL`, `busy_timeout=5000`.
- Single-writer discipline: one `*sql.DB` per ledger with `SetMaxOpenConns(1)`.
  `database/sql` then serialises for us and the `threading.Lock` + 1-worker executor
  + 1 ms busy-poll all disappear.
- All the `a*` async wrappers vanish — Go calls block on a goroutine, which is the
  point.
- `apply_stats`, `sync_git_refs` keep their explicit transactions
  (`db.BeginTx` / `tx.Commit`).

**Spike required before committing to this** — see §6.1.

### 3.4 Single-flight + progressive (tail-follow) delivery

The most subtle piece in the codebase
([`inflight.py`](../pkgcache/src/pkgcache/core/inflight.py)). One background task
streams upstream → `.part`, hashing as it goes; N readers tail-follow the growing
file and converge on the committed one.

Go shape:

```go
type Download struct {
    Key, Name  string
    FinalPath  string
    TmpPath    string

    mu       sync.Mutex
    written  int64
    done     bool
    err      error
    notify   chan struct{}   // closed + replaced on every change → broadcast

    HeadersReady chan struct{}  // closed once Total/MediaType are known
    Total        *int64
    MediaType    string
    SHA256       string
}

func (d *Download) wait(seen int64) { /* snapshot notify under mu, then <-ch */ }
```

The closed-channel broadcast replaces `asyncio.Condition.notify_all()`; readers grab
`d.notify` under the mutex, release, then block on it. `d.pulse()` closes the current
channel and installs a fresh one.

Points to get right, each of which the Python version already handles and the port
must preserve:

- The download runs in **its own goroutine with `context.Background()`**, not the
  triggering request's context — a client disconnect must not abort a fetch that
  other readers (and the cache) still want.
- `HEAD` and `Range` requests wait for `completion()` and then serve the committed
  file, rather than riding the progressive stream.
- On error before any bytes, the reader must see the error; after bytes, the stream
  just ends (a truncated response, same as today).
- `Content-Length` is set only when upstream declared one.
- Integrity: sha256 mismatch, size mismatch, and truncation all reject the commit and
  unlink the `.part`.

Do **not** use `golang.org/x/sync/singleflight` — it has no notion of progressive
readers.

### 3.5 Upstream client

```go
&http.Client{
    Transport: &http.Transport{
        // Accept-Encoding: identity. Go's transport adds gzip transparently and
        // decompresses, which would make our bytes differ from the upstream
        // Content-Length and from the index-declared hash. This is the Go
        // equivalent of the httpx `Accept-Encoding: identity` header.
        DisableCompression:  true,
        MaxIdleConnsPerHost: 32,
        // ...
    },
    Timeout: 0, // per-request via context; a global timeout would kill multi-GB pulls
}
```

Per-request deadlines come from `context.WithTimeout(ctx, cfg.RequestTimeout)`, with
the connect phase bounded by `net.Dialer{Timeout: 30s}` — matching
`httpx.Timeout(timeout, connect=30.0)`.

OCI anonymous bearer: `WWW-Authenticate` parse + token cache
(`map[string]tokenEntry` + `sync.RWMutex`), direct port of
[`upstream.py`](../pkgcache/src/pkgcache/core/upstream.py).

### 3.6 The six roles

Ordered by porting difficulty (this is also the recommended implementation order):

| Role | Difficulty | Notes |
|---|---|---|
| **files** | low | Autoindex HTML, PUT with streaming sha256, token check via `subtle.ConstantTimeCompare`, DELETE + empty-parent pruning. `http.ServeContent` for GET. |
| **npm** | low | Packument fetch + `dist.tarball` rewrite; `encoding/json` round-trip must preserve unknown fields → decode into `map[string]any`, not a struct. |
| **pypi** | medium | PEP 503 HTML **and** PEP 691 JSON parsing and rendering. The HTML anchor regex ports as-is; `_norm_core_metadata` (PEP 658/714 normalisation) is a real correctness detail uv depends on — port with its tests. |
| **apt** | medium | Absolute-form target reconstruction from `r.URL` / `Host`; volatile vs immutable classification; ETag/Last-Modified revalidation with a `.meta` sidecar. |
| **oci** | medium-high | Manifest/index handling, the `_pending` child-digest map that back-fills a tag row's real image size, `tags/list` name re-prefixing, digest verification. |
| **git** | high | Streams `git upload-pack --stateless-rpc` over `os/exec`. Needs: concurrent stdout stream + stderr drain, kill-on-client-disconnect, gzip request bodies, a 64 MiB negotiation cap, `GIT_PROTOCOL` passthrough, and the LFS batch/`+lfs/<oid>` path. |

**`git` keeps the `git` binary as an external dependency.** go-git cannot serve
protocol-v2 `upload-pack` negotiation with filters and shallow clients reliably, and
that negotiation is the entire reason this role exists as mirror-and-serve rather
than byte-caching. This is the single remaining non-Go runtime requirement; it is
documented, and the role degrades to a clear 503 if `git` is absent rather than
failing at startup.

Streaming `upload-pack` in Go is actually cleaner than the asyncio version:

```go
cmd := exec.CommandContext(ctx, "git", args...)
cmd.Stdin  = bytes.NewReader(body)
cmd.Stdout = w                      // straight to the ResponseWriter
cmd.Stderr = &tailBuf{}             // bounded stderr tail for the error message
// ctx cancellation (client disconnect) kills the process — no manual kill dance
```

with `http.NewResponseController(w).Flush()` after the advertisement preamble.

### 3.7 Control plane

| Python | Go |
|---|---|
| `ThreadingHTTPServer` + `BaseHTTPRequestHandler` | `net/http` server + the custom mux |
| `routes.ROUTES` table of `(method, regex, fn)` | same table, `[]route{Method, Pattern, Handler, Guard}` |
| `Request` wrapper (`req.user`, `require_view`, …) | middleware chain producing an `authCtx` in `r.Context()`; guards as `func(*authCtx, string) error` |
| `ApiError` / `AuthError` / `ForbiddenError` | one `apiError{Msg string; Status int}` implementing `error`; a top-level handler renders `{"error": …}` |
| `_same_origin` CSRF guard | direct port, same semantics (absent `Origin` allowed; `PUBLIC_ORIGIN` pin; `X-Forwarded-Proto` check under `TRUST_PROXY`) |
| `Sessions` (dict + lock, monotonic TTL, per-IP throttle) | `map` + `sync.Mutex`, `time.Now()` is monotonic-backed in Go by default |
| `PasswordHasher` (stdlib scrypt N=2^14 r=8 p=1 dklen=32) | local RFC 7914 mixer + `crypto/pbkdf2` with **identical params** → existing hashes verify |
| `Accounts` policy (user < admin < superuser, reporting graph) | pure logic, direct port; the store stays an injected interface |
| `Jobs` (generator of log lines, one at a time) | `func(emit func(string)) error`; the runner appends to a `[]byte` log under a mutex; `GET /api/jobs/{id}?offset=` unchanged |
| `registry.py` / `users.py` JSON gateways with atomic temp→rename | direct port; keep `LOCK` as a `sync.Mutex`, keep the "corrupt file is an error, missing file is first run" distinction |

Config validation currently happens at **import time** in
[`settings.py`](../webui/app/settings.py) (it raises `RuntimeError` for a bad
`UI_PUBLIC_ORIGIN`, a non-positive TTL, etc.). In Go this becomes a `LoadSettings()
(Settings, error)` called from `main`, which is a strict improvement — the errors get
reported once, with a usable message, instead of as an import traceback.

---

## 4. Replacing DVC and git for cache-state versioning

This is the largest piece of genuinely new code and the one with data-safety
implications. It deserves its own decision.

### 4.1 What DVC is actually doing here

Reading [`operations.py`](../webui/app/services/operations.py), DVC provides exactly
four things:

1. **Content-addressed snapshot** of `caches/<repo>/<role>/` trees (`dvc add`),
   producing `<role>.dvc` pointer files that git commits.
2. **Materialisation** of a snapshot back onto disk (`dvc checkout`), using
   reflink → hardlink → copy.
3. **Cross-repo dedup** via a shared object store (`.dvc-shared`), so an artifact
   several projects hold is stored once.
4. **Delta transfer** across the air gap (`dvc push -r shuttle --rev target <paths>`),
   paired with `git bundle` for the pointer history.

And git provides: the checkpoint log (`git log` → the History panel), rollback to a
commit (`git checkout <sha>` + `dvc checkout`), and fast-forward import
(`git fetch bundle` + `git merge --ff-only`).

None of that is consumed by anything outside this project.

### 4.2 Proposed native replacement — `internal/vcs`

The proxy **already** maintains a sha256 content-addressed store with hardlink
materialisation (`caches/.cas`, see
[`storage.py:114`](../pkgcache/src/pkgcache/core/storage.py#L114)). The snapshot
store is the same idea applied to whole trees.

**On-disk format**, inside each project's cache repo (`caches/` or
`caches/projects/<name>/`):

```
.pkgvcs/
  objects/sha256/<aa>/<hex>          # hardlinked from the live tree — zero-copy
  snapshots/<id>.json                # {role: [{path, sha256, size, mode, mtime}]}
  log.jsonl                          # append-only: {id, parent, ts, subject, author, snapshot}
  HEAD                               # current checkpoint id
```

- **Checkpoint** = walk each role subdir → sha256 each file (skip `*.part`,
  `tmp_pack_*`, `*.lock`) → `os.Link` new content into `objects/` → write
  `snapshots/<id>.json` → append to `log.jsonl` → update `HEAD`.
  Files whose `(size, mtime, inode)` match the previous snapshot are **not
  re-hashed** — this is what keeps a checkpoint of a 500 GB cache cheap, and it is
  the same trick DVC's state DB plays.
- **Rollback** = read `snapshots/<id>.json`, materialise each entry by hardlink from
  `objects/`, remove paths not in the snapshot, update `HEAD`.
- **Cross-project dedup** = one `objects/` store per host (`caches/.pkgvcs/objects`),
  shared by every project repo. Same filesystem requirement as today, same guard.
- **`ledger.db` handling** — carried as ordinary content, but materialised as a
  **copy, never a hardlink**, because the live proxy rewrites it in place. This is
  exactly the hazard `_unshare_ledgers` exists to repair after the fact; doing it
  correctly up front removes that whole function.
- **Export** = `out/pack-<base>..<target>.tar` containing the snapshot JSONs, the
  log entries, and only the objects present in `target` but not `base` (or all
  objects for a full export) + `checkpoints.json` + (global only) `certs/`.
  Streamed and gzip'd with stdlib `archive/tar` + `compress/gzip`.
- **Import** = read the pack, verify every object's sha256 against its name, append
  log entries (rejecting a non-fast-forward), materialise `HEAD`.

This is roughly **900–1,200 lines of Go** including tests. It replaces `dvc` (a
~200 MB Python dependency tree) *and* `git` on the control plane, *and* the
`_find_md5_tree` / `_normalize_dvcstore` mess that exists purely to tolerate the ways
an operator's manual copy mangles DVC's `dvcstore/files/md5` layout.

### 4.3 Decision needed: keep git for the checkpoint log?

**Recommendation: no.** Use `log.jsonl` as above.

- *For dropping git:* removes `go-git` (a large dependency) or the `git` binary from
  the control plane; export/import become one format we control end-to-end; the
  History panel's four fields (`hash`, `short`, `date`, `subject`) come straight out
  of the log; a fast-forward check is a parent-pointer comparison.
- *Against:* an operator can no longer `cd caches && git log` to inspect state, and
  existing `caches/.git` history is not carried forward.
- *Mitigation:* `pkgreg history [--project P]` prints the same thing; the log is
  plain JSONL, greppable with `jq`.

**Alternative if git history is considered load-bearing:** use `go-git` for the
pointer repo (commits, log, ff-merge all supported) and keep only the object
store/transfer native. `git bundle` has no go-git equivalent, so the transfer format
would be ours regardless. Cost: one large dependency; benefit: `git log` in `caches/`
keeps working.

### 4.4 Migration from an existing DVC-versioned deployment

Two supported paths, documented in the cutover guide:

1. **Fresh lineage (default, recommended).** The bytes on disk are the truth. Run
   `pkgreg checkpoint -m "migrated from dvc"` — it snapshots the live tree as
   checkpoint #1. The old `caches/.git` and `caches/.dvc` are left untouched on disk
   for as long as the operator wants them.
2. **`pkgreg vcs import-dvc`** (opt-in, ~150 lines). Reads `.dvc` pointer files and
   the `.dvc/cache/files/md5` tree and replays each git commit's pointer set as a
   `log.jsonl` entry. Objects are re-hashed sha256 (DVC uses md5) while hardlinking
   into the new store. Worth building only if a deployment has checkpoint history it
   genuinely needs to roll back to.

---

## 5. Frontend and static assets

**Keep the React console exactly as it is.** It is well-structured, and rewriting
2,500 lines of working TSX buys nothing.

```go
//go:embed all:console/dist
var consoleFS embed.FS
```

Serving replaces `nginx.conf` one-for-one:

| nginx rule | Go |
|---|---|
| `location = /` → `landing.html`, no-cache | exact-match handler, `Cache-Control: no-cache` |
| `location = /tutorial` → `tutorial.html` | same |
| `/assets/` → `expires 1y; immutable` | `Cache-Control: public, max-age=31536000, immutable` |
| SPA fallback → `index.html` | catch-all, `Cache-Control: no-cache` |
| CSP / `Permissions-Policy` / `Referrer-Policy` / `X-Content-Type-Options` / `X-Frame-Options` | one `securityHeaders` middleware, same values verbatim |
| `client_max_body_size 64m` | `http.MaxBytesReader` bound by `UI_MAX_JSON_MB` (already the setting) |
| `proxy_pass` to webui | gone — same process |

**Build coupling:** `go:embed` needs `web/console/dist` to exist at compile time.

- `make build` runs `npm ci && npm run build && go build`.
- A `//go:build !embedconsole` variant embeds a stub `index.html` reading *"console
  bundle not built — run `make console`"*, so `go build ./...`, `go test ./...` and
  API-only iteration work with no Node installed.
- Release builds are `make release` → `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X …/buildinfo.Version=…"`
  for `linux/amd64` and `linux/arm64`.

Result: **nginx and the entire `console` container disappear.**

---

## 6. De-risking: spikes to run before committing

Each is a day or less, and each can invalidate a design choice cheaply.

### 6.1 `modernc.org/sqlite` against a real ledger

Highest-value spike. Open a copy of an existing `caches/pip/ledger.db` (there are
populated ones in this checkout), run the full `stats()` query set and a
`apply_stats` transaction, and confirm:

- WAL mode engages and a concurrent reader sees committed writes
- the `ON CONFLICT … DO UPDATE` upserts behave identically
- `DELETE FROM bandwidth_samples WHERE rowid NOT IN (SELECT … LIMIT ?)` works
- write throughput is adequate under a burst (target: ≥2,000 `arecord`-equivalent
  inserts/sec, which comfortably exceeds a `uv sync` storm)

If it fails: fall back to `mattn/go-sqlite3` + CGO and accept a glibc-linked binary
(still one file, just not fully static), or link statically against musl.

### 6.2 Tail-follow streaming under load

Prototype `Download` + 20 concurrent readers against a 2 GB upstream. Assert: every
reader receives byte-identical content, the sha256 matches, a mid-stream reader
disconnect does not abort the fetch, and an upstream failure at 50 % surfaces to
readers without a goroutine leak. Run under `-race`.

### 6.3 Path escaping through the whole stack

Fire real clients at a stub server: `npm install @babel/core` (percent-encoded
scope), `docker pull …/dockerhub/library/alpine` (multi-segment name), an apt
`Acquire::http::Proxy` fetch (absolute-form target), and a pypi filename containing
`+` and `%2B`. Confirm the custom mux and the handlers agree on what is escaped.

### 6.4 Native CA + leaf minting

Replace `gen-certs.sh` with `crypto/x509`: reuse an existing `certs/ca.crt`+`ca.key`
if present (so already-distributed trust survives), else mint a 4096-bit CA; then a
2048-bit leaf with the same SAN discovery (localhost, 127.0.0.1, hostname, FQDN,
primary IP, plus arguments). Verify a Go-minted cert is accepted by `docker pull`,
`git`, `curl`, and `pip`.

### 6.5 `git upload-pack` streaming

Clone a large repo (e.g. `torvalds/linux`) through a prototype handler with
protocol v2, a partial-clone filter, and a shallow clone; kill the client mid-pack
and confirm the `git` child is reaped.

---

## 7. Phasing

Estimates assume one engineer working steadily; phases 2–4 parallelise well across
two or three.

| Phase | Scope | Est. | Exit criterion |
|---|---|---|---|
| **0** | Spikes §6.1–6.5 | 3–5 d | Every spike green or a documented fallback chosen |
| **1** | Skeleton + `internal/cache` (config, storage, ledger, upstream, progress, stats, inflight, cache) + unit tests | 1.5–2 w | `go test ./internal/cache/...` green under `-race`; ledger reads an existing `ledger.db` |
| **2** | Roles: files → npm → pypi → apt → oci | 2–2.5 w | Each role serves real clients on an alternate port against a temp cache root |
| **3** | Role `git` + `gitmirror` | 1 w | Clone/fetch/LFS parity with the Python role, incl. shallow + filter |
| **4** | Router, unified listener, supervisor, `serve` command | 4–5 d | `pkgreg serve` passes a port of `test_router.py` + `test_unified.py` |
| **5** | Control plane: settings, projects, accounts, sessions, jobs, reads, urls, API table, CSRF | 1.5–2 w | Port of `test_auth.py`, `test_projects.py`, `test_ownership.py`, `test_multiproject.py` green |
| **6** | `internal/vcs` + ops (checkpoint / export / import / rollback / mode / lockwarm) | 2–2.5 w | Round-trip: checkpoint → export → import on a second machine → rollback, byte-identical trees |
| **7** | Console embed, static serving, security headers, `pki`, CLI subcommands, systemd unit, docs | 1 w | `./pkgreg serve` on a clean VM with only `git` installed serves everything |
| **8** | Parity harness + cutover (§8) | 1–1.5 w | Differential test suite clean; a real deployment migrated |

**Total: roughly 10–13 weeks solo, 6–8 weeks with two or three engineers.**

Phase ordering is deliberate: the cache roles come first because they carry the
correctness risk and can be validated against real clients in isolation, on
alternate ports, while the Python stack keeps serving.

---

## 8. Parity testing and cutover

### 8.1 Differential harness

The strongest safety net available here, and cheap to build (~200 lines):

Run the Python stack on ports `A` and the Go binary on ports `B`, both pointed at
**separate copies of the same warm cache tree**, then replay a fixed request corpus
against both and diff status, headers (minus `Date`/`Server`), and body bytes.

Corpus, drawn from what the roles actually serve:

- pypi simple index for `numpy`, `torch`, `grpcio` in **both** HTML and PEP 691 JSON;
  a wheel `GET`, a `HEAD`, a `Range: bytes=1000-2000`, and a `.metadata` sidecar
- npm packument for `chalk` and `@babel/core`; a tarball `GET`
- OCI `/v2/`, a multi-arch tag manifest, a child manifest by digest, a blob `GET`
  and `HEAD`, `tags/list` for both a global and a project pull
- apt `InRelease` (volatile, with and without a matching ETag), a `.deb` (immutable),
  an `apk`
- files autoindex, `GET`, `PUT` (write-once, then `?overwrite=1`), `DELETE`
- git `info/refs?service=git-upload-pack` and a full clone
- every `/api/*` endpoint under: no auth configured, auth + anonymous read, auth +
  each of the three roles

Byte-for-byte equality is the target for cached responses; for the pypi HTML index
and the npm packument (where key ordering could differ), normalise JSON key order and
compare semantically.

### 8.2 Cutover

Because **`config/projects.json`, `config/users.json`, `pkgcache.yaml`, the
`caches/<eco>/` layout and `ledger.db` schema are all preserved unchanged**, cutover
on a live host is:

1. `pkgreg checkpoint -m "pre-cutover"` — *no*, run the **Python** checkpoint first;
   then verify a DVC export/import still works. This is the rollback plan.
2. Stop the compose stack.
3. `pkgreg migrate-check` — validates the registry, users store, certs and cache tree
   against what the Go binary expects; reports anything it would refuse.
4. `pkgreg checkpoint -m "migrated to pkgreg"` — establishes the native VCS lineage.
5. `systemctl start pkgreg` — same ports, same URLs, same certs. **Clients need no
   change at all.**
6. Keep the compose files and the Python source in the repo for one release cycle as
   a documented rollback.

The air-gapped side migrates independently: it is a pure mirror, so it can be
re-seeded with a full `pkgreg export` / `pkgreg import` from the online side rather
than migrating its DVC history.

---

## 9. What the port gains, and what it costs

**Gains**

- One file to ship. No docker, no compose, no uid/gid mapping, no nginx, no Python,
  no DVC. `scp pkgreg host: && systemctl start pkgreg`.
- `bootstrap.sh`'s entire reason for existing (pre-creating bind-mount sources so the
  daemon doesn't `root:root` them) evaporates.
- No `openssl` — `crypto/x509` mints the CA and leaf.
- The internal HTTPS hop, its CA-verification compromise, the 6-role stats fan-out,
  the 30 s staleness cache, and the live-feed poller all disappear (§2.1).
- The ledger's 1 ms busy-poll executor bridge disappears.
- `http.ServeContent` gives strictly better Range / conditional-request handling than
  Starlette's `FileResponse`.
- Real parallelism for hashing and SQLite instead of a single event loop hopping to a
  one-thread executor.
- Memory is bounded and predictable under many concurrent multi-GB streams.

**Costs**

- `git` remains a runtime dependency for the git mirror role.
- Node remains a *build-time* dependency for the console.
- ~900–1,200 lines of new, safety-critical code to replace DVC (§4), which must be
  tested harder than anything else in the port.
- Roughly 9,000–11,000 lines of Go to replace ~5,500 lines of Python. Go's verbosity
  is real; the tradeoff is buying it back in deployment and operational surface.
- The pytest suite (~2,650 lines) has to be re-expressed; `httptest` + `t.TempDir()`
  maps well, but it is not mechanical.
- Losing `git log` in `caches/` if §4.3 is decided as recommended.

---

## 10. Open decisions

These need an answer before Phase 1 starts; the rest of the plan holds either way.

1. **§4.3 — keep git for the checkpoint log?** Recommendation: no, use `log.jsonl`.
   Alternative: `go-git`.
2. **§4.4 — migrate DVC history, or start a fresh lineage?** Recommendation: fresh
   lineage; build `import-dvc` only if a real deployment needs its history.
3. **§2.1 — keep the `/+ledger/*` HTTP endpoints published?** Recommendation: yes
   (documented surface, and the split-deployment path needs them), even though the
   default control plane no longer calls them.
4. **Single repo or new repo?** Recommendation: same repo, Go code alongside the
   Python during the port, with the Python tree removed one release after cutover.
5. **Should the port also change any behaviour?** Recommendation: **no** — parity
   first, then improve. The differential harness in §8.1 only works if the answer is
   "no".
