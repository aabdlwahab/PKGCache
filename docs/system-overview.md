# pkgreg — what it does and why it is built this way

Written 2026-07-29. This describes the system as built, in plain terms. For the
numbered build plan see [go-implementation-plan.md](go-implementation-plan.md); for the
argument behind the bigger decisions see [go-architecture.md](go-architecture.md).

---

## In one paragraph

pkgreg is a caching package registry. Build hosts point `pip`, `npm`, `docker`,
`apt`, `apk` and `git` at it instead of at the internet. The first time anyone asks for
something it fetches it from upstream and keeps a copy; after that it serves the copy.
It can be told to go **offline**, at which point it serves only what it already holds
and never reaches the network — which is the point, because the same cache can be
exported to a disconnected site and rebuilt there byte for byte. It is one static
binary with no runtime dependencies.

## The problem it solves

Three problems, in order of how much they hurt:

1. **Repeated downloads.** Fifty CI runs pulling the same 2.5 GB CUDA wheel is fifty
   trips to the internet.
2. **Air-gapped sites.** A disconnected environment needs the same packages, and
   "copy some files onto a USB stick" is not a reproducible process.
3. **Upstream disappearing.** A package is yanked, a registry has an outage, a
   namespace is taken over. A cache that holds what you already built with is
   insurance.

---

## The core model

Three ideas explain most of the system.

### Projects are URL prefixes

A project is a tenant. It is not a port, a hostname, or a container:

```
https://cache:8443/<project>/<ecosystem>/…
```

`global` is the default. Docker cannot be given a base path, so there the project rides
the image name (`/v2/<project>/<registry>/<image>`), and apt is a forward proxy with
nowhere to put it, so it rides the proxy username (`http://myproject@cache:3142`).

**Why:** a URL prefix is the only thing every client in this system can be told about.
One process serves any number of tenants with no per-project listener and no restart to
add one.

### Content lives in one content-addressed store

Every distinct byte-string on the host is stored once, named by its SHA-256:

```
blobs/sha256/<aa>/<bb>/<full-hex>
```

Separately, a **catalog** maps `(project, ecosystem, key)` → digest. So the same wheel
requested by four projects is four catalog rows and one file on disk.

A blob is immutable once written. Nothing ever rewrites one in place. That single
property is what makes concurrent garbage collection, live snapshots and cross-project
sharing safe without locking anything.

### Ecosystems are adapters over one engine

Six adapters — `pypi`, `npm`, `oci`, `apt`, `git`, `files` — each describe *what* they
want; a shared engine decides *how*. An adapter never touches SQL, the filesystem, HTTP
clients, or single-flight logic.

**Why:** in the previous design the same fetch-hash-commit-record logic was written six
times with small variations, and eight separate tables mapped ecosystem names to
behaviour. Now adding an ecosystem is one file plus one line of registration, and the
console, the API, snapshot inclusion and setup instructions all derive from the
adapter's descriptor automatically.

---

## What each ecosystem does

| | Clients | Shape | Notes |
|---|---|---|---|
| **pypi** | `pip`, `uv` | path-prefixed | PEP 503 HTML **and** PEP 691 JSON, parsed and re-rendered; PEP 658 `.metadata` sidecars; multiple named indexes |
| **npm** | `npm`, `yarn`, `pnpm` | path-prefixed | packument fetch with `dist.tarball` rewritten; unknown fields preserved byte for byte |
| **oci** | `docker`, `podman` | protocol-rooted `/v2/` | multi-arch images, manifests by tag or digest, `tags/list`, anonymous token dance for ghcr/quay/dockerhub |
| **apt** | `apt-get`, `apk` | forward proxy | relays any host, optional allowlist; `InRelease` revalidates, `.deb` never does |
| **git** | `git`, `git lfs` | path-prefixed | full, shallow and partial clones; protocol v2; LFS; push refused |
| **files** | `curl`, `wget`, CI | path-prefixed | the one writable role: `PUT`/`DELETE` behind a token, browsable index |

Two storage modes. Five ecosystems store content as blobs. Git is the exception: it
keeps real bare repositories under `managed/git/`, because `git upload-pack` needs a
directory it can run against.

---

## The caching engine

Every request for content takes the same five steps, in this order:

1. **Hit** — the catalog already maps this key to a blob we hold. Serve it.
2. **Dedup** — we don't have this *key*, but we already have these *bytes* (some other
   project or ecosystem fetched them). Link the key, no network.
3. **Peer** — a sibling cache has the bytes. Fetch from it instead of the internet.
4. **Offline** — if the project is offline, stop here with a specific 404.
5. **Miss** — fetch from upstream, exactly once, streaming to every waiting client.

Three things about step 5 are worth spelling out.

**One fetch, many readers.** If twenty build hosts ask for the same wheel at the same
moment, one upstream request is made. The other nineteen attach to it and start
receiving bytes immediately — they do not wait for the download to finish. This was
measured: twenty clients, one 2 GiB artifact, one upstream request.

**A client hanging up does not cancel the download.** The fetch runs on a detached
context, because the other nineteen readers and the cache itself still want those bytes.
This is the one deliberate exception to the rule that everything takes a cancellable
context.

**Everything is hashed while it streams.** The digest is computed in the same single
pass that writes the file, so a 2.5 GB wheel is never read twice or held in memory. That
is what makes step 2 work for npm and apt, where nobody told us the digest in advance.

If the bytes don't match a declared digest or size, the commit is rejected and nothing
is published. A truncated or corrupted transfer never becomes a cached artifact.

---

## Working offline and moving caches between sites

This is the part the whole design bends around.

**Offline mode** can be set per project or instance-wide. In offline mode the cache
serves what it holds and never contacts an upstream. Mutable things still resolve:
a docker tag still points at the manifest it pointed at when it was last seen, because
tags are recorded as *refs*.

**Checkpoints** are a snapshot of what a project holds. A checkpoint is a sorted,
gzipped list of `(ecosystem, key, digest, size)` — stored, like everything else, as an
ordinary blob. Creating one is a database query, not a walk of the filesystem, and nothing pauses
while it runs. A 32,000-row fixture standing in for a 119 GiB cache checkpoints in
126 ms.

**Export and import** move a project between hosts:

```sh
pkgreg export -project team-a                # full pack
pkgreg export -project team-a -base <older>  # only what changed since
# move the .tar by whatever means the air gap allows
pkgreg import -project team-a
```

Import verifies every blob's SHA-256 against its own filename before linking it, and
refuses a pack that doesn't continue from the receiving host's current state. You cannot
silently import a divergent history.

**Rollback** restores an earlier checkpoint. Content referenced by any checkpoint is
pinned and never collected, so an old checkpoint always restores.

---

## Controlling it

### Projects, users, tokens

- **Projects** have an owner, an offline flag, byte and artifact quotas, a data-plane
  auth mode, and rate limits. A newly created project is routable on the **next
  request** — there is no polling interval.
- **Users** have one of three roles: `user`, `admin`, `superuser`. Admins create
  projects and accounts; the reporting relationship decides who can see and operate
  what. Passwords use scrypt with the same parameters as the retired Python service, so
  existing accounts migrate and keep working.
- **Tokens** are scoped to a project, an ecosystem and a permission level, and follow a
  ladder: `admin` ⊃ `write` ⊃ `read`. A CI token that can publish can also install.
  `peer` sits outside the ladder — it authorises cache-to-cache fetches and grants no
  user access. Tokens are stored as a plain SHA-256 of a 256-bit secret; there is no
  slow hash on the request path because there is no low-entropy secret to protect.

### Jobs

Long operations — checkpoint, rollback, export, import, lock warming, collection,
eviction — run as queued jobs with persisted logs, per project, so checkpointing one
project doesn't block another. They survive a restart and can be cancelled.

### Audit

Every mutation is recorded with who did it, from which address, and when.

### API and console

A versioned `/api/v1` covers projects, artifacts, upstreams, snapshots, jobs, tokens,
users, stats, time series and events, plus a live event stream so the console updates
without polling. A compatibility shim keeps the older `/api/*` endpoints working.

The console is hand-written HTML, CSS and ES modules compiled into the binary — no
framework, no bundler, no Node in the build, no nginx, no second container. `go build`
alone produces the whole shipped program, which is the same bargain the rest of the
binary makes by refusing cgo.

It is six views, following what a project actually goes through rather than presenting
one wall of panels:

| View | Answers |
|---|---|
| **Overview** | Is it healthy, and what is it doing right now? |
| **Cache** | What is in it — by ecosystem, by package, and how cold? |
| **Connect** | How do I point a tool at it? |
| **Sources** | Where do misses go, and is that working? |
| **Transfer** | How do I move it, roll it back, or fill it in advance? |
| **Admin** | Who may do what, and what happened? |

A project switcher scopes everything, and a slide-out activity rail shows running jobs
and live transfers from any view — you can start a checkpoint on Transfer and watch it
while reading Cache.

`pkgreg serve --headless` removes the browser surface entirely and leaves the API,
metrics, health and the data plane untouched: for deployments driven only by CLI or CI,
a login form on a reachable port is attack surface that buys nothing.

---

## Keeping the disk from filling up

- **Garbage collection** removes blobs nothing refers to any more. It runs online, with
  a grace period protecting freshly written content, and re-checks each blob at the
  moment of deletion so a concurrent upload is never collected out from under itself.
- **Eviction** removes cache entries to hold a size target, a free-space floor, or a
  maximum idle age. It evicts least-recently-**used** first: reads advance an entry's
  recency, so the wheel everyone pulls daily is the last thing to go, not the first.
- **Quotas** are per project, in bytes and artifact count. A download that cannot fit is
  refused as soon as its size is known — from the upstream headers, or mid-transfer for
  a chunked response — so an over-quota project gets one clear error instead of
  re-downloading the same artifact forever.
- Anything a checkpoint references is pinned and survives both.

---

## Running more than one cache

Caches can be chained. Instance B, configured with A as a peer, asks A for content by
digest before going to the internet. The exchange is digest-addressed, so no ecosystem
index has to be re-resolved, and it is authenticated with a `peer`-scoped token.

This is federation, not clustering. There is no shared state and no failover; each cache
owns its own storage.

---

## Getting clients to trust it

The cache serves TLS with a certificate it mints itself. Start with the
[`pkgreg-client` tutorial](../go/internal/web/dist/tutorial.html).

**The client.** Download `pkgreg-client` from the tutorial or Console → Connect and
give it the CA fingerprint through a separate trusted channel. It checks that
fingerprint, starts a verified localhost bridge, and opens a child shell whose package
settings point at that bridge. Exiting the shell stops the bridge and discards the
environment. It writes no files and needs no administrator access.

**Publishing it.** The operator runs `pkgreg publish-client` on the cache host, passing
the release files (or nothing, to scan the directory holding the `pkgreg` binary). It
installs each binary atomically, records SHA-256 digests, and refuses a copy that
contradicts a checksum file shipped beside it. From a checkout with a Go toolchain,
`make client-publish DATA_DIR=<pkgreg-data-dir>` cross-compiles all five targets and
then calls that same command. Building the files into `go/bin` alone does not publish
them, and until something is published the tutorial has no download to offer —
`pkgreg doctor` reports that as a warning.

**Docker.** The daemon is a separate process that never reads the session's environment,
and under Docker Desktop it runs in a VM whose loopback is not the developer's — so the
session's bridge address is unreachable from it. `pkgreg-client -docker-trust` installs
the verified CA for the daemon alone, after which `docker pull` uses the cache's own
stable address. One file, no administrator access on Docker Desktop, reversible with
`-uninstall`. Details and the per-platform locations are in
[client onboarding](client-onboarding.md).

**Persistent setup.** `pkgreg-client --persist` downloads the auditable generated
setup script over fingerprint-verified TLS and applies machine trust and project
settings. This is explicit because it changes protected files and needs administrator
access. Use it for managed CI/build hosts whose setup must survive a terminal session.

The standalone [`pkgreg-bridge`](client-bridge.md) exposes the same localhost transport
for users who want to supervise it themselves instead of letting `pkgreg-client` own
its lifetime.

---

## Migrating from the Python stack

`pkgreg migrate from-python` imports an existing deployment: projects, users, package
inventories, refs, statistics, files, shared content and Git mirrors. It is:

- **resumable** — progress is durable, so an interrupted run continues where it stopped;
- **safe to run live** — the Python service can keep serving during the long pass;
- **cheap** — existing content-addressed objects are hard-linked by their filename
  digest rather than re-hashed.

The production 119 GB tree imported in 52.7 seconds, with an immediately following
resume pass doing measurably zero work.

---

## Design choices, and why

### One static binary

`CGO_ENABLED=0`, no libc, no Python, no nginx, no container runtime. Copy one file to a
host with `git` installed and run it. On an air-gapped machine, every dependency you
have to install first is a dependency you have to move across the air gap first.

### The blob is durable before the database row is

Content is fsync'd and linked into the store *before* the catalog row that references it
is written. So a lost row costs a re-fetch, never corruption.

That ordering is what buys the right to **batch** catalog writes into ~100 ms
transactions. A `uv sync` fires thousands of small requests in a burst; without batching
that is thousands of transactions and thousands of fsyncs on the hot path. The trade is
only acceptable because of the ordering — the same trick would be wrong for an account
record.

### Refcounts are derived, not stored

Nothing maintains a counter of how many things reference a blob. Garbage collection
works it out by scanning an index. A stored counter is one more thing that can drift out
of agreement with reality, and at this scale the scan is cheap.

### Mutable names are one concept, not four

A docker tag, a git branch, an apt `InRelease` file and a PyPI index are the same thing:
a name pointing at immutable content, with a freshness policy. Previously each had its
own mechanism — a table, another table, sidecar files on disk, and "just re-fetch every
time". Now they are all **refs**, maintained by one code path that handles TTLs, ETag
revalidation and offline last-known-good.

### A custom router

`http.ServeMux` cannot express OCI's `/v2/{name...}/manifests/{ref}` — a wildcard that
isn't at the end — and it rewrites paths, which breaks byte-faithful proxying for apt.
The replacement matches on the *escaped* path, so `@babel%2Fcore` stays one segment and
an apt absolute-form target survives intact. Both limitations are pinned by a test that
runs against the live standard library, so if a future Go release fixes them the test
says so rather than the comment going stale.

### Two SQLite databases

`catalog.db` holds what is cached; `control.db` holds projects, users, tokens and audit.
They are separate because their value is different: the catalog is rebuildable from the
blobs, the control database is not. Both use a pure-Go driver, which is what keeps the
binary CGO-free.

### Configuration is swapped atomically

Readers take an immutable snapshot with a single atomic pointer load — no lock, no
contention, on every request. Writers build a complete replacement and swap it in one
operation, so a request can never see a project that exists in the routing table but not
yet in the quota table.

### One port, if you want

A single listener can serve TLS, plain HTTP and the apt forward proxy together, deciding
which is which from the first byte of the connection. One firewall rule instead of three.
Explicit separate listeners remain available.

### Local filesystem only

Commits rely on `link` and `rename` being atomic within a directory, and cross-project
sharing relies on hard links. Neither holds on NFS. This is a real constraint, stated
plainly rather than discovered later.

---

## What it deliberately does not do

- **It is not a highly-available cluster.** One instance owns its storage. Peering
  shares content between caches; it does not replicate state or provide failover.
- **It is not a package registry you publish to,** except for the `files` role. It
  mirrors upstreams; it is not a home for your own npm packages.
- **It does not proxy arbitrary HTTPS.** apt and apk work through a plain forward proxy
  because that protocol has no TLS form. Everything else is a reverse proxy with a known
  shape.
- **It does not modify package contents.** URLs inside indexes are rewritten so clients
  come back to the cache; the artifacts themselves are served byte for byte, and their
  digests are verified.

---

## Observability

Structured JSON logs, one line per request, never containing a token or an
`Authorization` header. Prometheus metrics covering requests by outcome, bytes served,
upstream bytes and errors, fetch and query latency, in-flight fetches, store size and
object count, reclaimed and evicted bytes, job duration, and dropped events. `/healthz`
answers whether the process is alive; `/readyz` answers whether it can actually serve —
including proving the blob store is writable, because a read-only filesystem otherwise
shows up as every download mysteriously failing.

`pkgreg doctor` checks configuration, both databases, writable storage, hard-link
support, certificate validity and SAN coverage, `git`, the embedded console and the open
file limit, and reports everything wrong in one pass rather than one failed request at a
time.

### History without a second system

The console needs to show change over time, and requiring Prometheus to answer "was it
like this yesterday?" would contradict the whole point of a single binary. So the
catalog keeps its own coarse history:

- **Request outcomes over time**, bucketed and split by the five pipeline steps. The
  engine always knew which step served a request — it labels the Prometheus counter
  with it — but until now it collapsed that to hit-or-not before storing, so the
  console could report a hit rate and never say whether a hit came from a local entry,
  a coalesced in-flight fetch, or a peer.
- **Storage growth**, sampled hourly. Sampled rather than derived: summing blob
  creation times looks equivalent, but garbage collection deletes those rows, so a
  derived history would quietly rewrite its own past every time the collector ran.
- **Upstream health** — requests, errors, mean and max latency per upstream, hourly.
- **Cache age**, derived on demand: how much content has not been read in a day, a
  week, a month, a year. This is the same ordering eviction uses, so it is a preview of
  what the next pass would take.

Resolution degrades with age rather than history being deleted: five-minute buckets for
two days, folded to hourly for a month, then daily for two years. The fold and the
delete share a transaction, which is what makes it exactly-once after a crash.

**The boundary is deliberate.** This exists so the console works with nothing else
installed, at coarse resolution. Prometheus remains the answer for high resolution, long
retention and alerting; there are no percentiles or histograms here, and rebuilding them
in SQLite would cost far more than the questions this view asks.

One consequence is visible in the charts: a flush window that fails is dropped rather
than re-buffered, because retaining it would grow memory without bound while the
database is unavailable. That shows up as a **missing bucket**, and the console draws
missing buckets as gaps — never interpolated, never plotted as zero. A chart that draws
a confident zero for "nobody knows" is worse than one with a hole in it.

---

## Command surface

```
serve       run the cache and the control plane
init        create the data directory, mint TLS material, write a config
doctor      check configuration, storage and TLS, and report what is wrong
systemd     install and start the production service
audit       print the immutable control-plane audit log
checkpoint  create a content-addressed cache checkpoint
rollback    restore a project checkpoint
export      write a full or delta air-gap pack
import      verify and apply an air-gap pack
lockwarm    warm and rewrite a uv.lock
gc          collect unreferenced blobs
evict       apply LRU, TTL and free-space eviction policy
migrate     import a live Python cache into the Go store
version     print build identity
```

Plus two separate programs: `pkgreg-client`, which opens a fingerprint-verified
temporary package shell by default and performs machine setup only with `--persist`,
and `pkgreg-bridge`, the standalone form of the same loopback transport.

---

## How it is verified

Unit and integration tests against a synthetic upstream that can truncate, stall,
corrupt and demand authentication on command. A contract suite that runs against the
storage interface rather than its one implementation. Real `pip`, `uv`, `npm`, `docker`,
`apt-get`, `apk`, `git` and `wget` clients in CI. Fuzzers on the four parsers that read
untrusted input — path resolution, digests, manifests and PyPI HTML. A load test that
puts twenty concurrent readers on a 2 GiB stream and asserts the origin saw exactly one
request. A differential harness that replays a corpus against both the old and new
stacks and compares status, headers and body bytes.

Everything runs under the race detector, and that is a merge requirement.

See [running-and-testing.md](running-and-testing.md) for how to drive all of it.
