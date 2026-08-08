# pkgreg Go release announcement

Paste-ready Teams copy for the Go release. Before sending:

1. Replace `<cache-host>` with the hostname users can resolve.
2. Replace `<ca-sha256>` with the CA fingerprint printed by `pkgreg init` or
   `pkgreg doctor`. Send that fingerprint through a second trusted channel too.
3. Confirm that `pkgreg-client` has been published and the URLs below open from a
   developer machine.
4. Attach the four GIFs from this folder at the marked points. Teams keeps them
   animated.

The short post is intentionally user-focused. The operator appendix after it is the
complete feature and architecture reference; include or link it for readers who want
the details.

---

## Paste-ready Teams post

### 🚀 pkgreg is live: download once, build many times

*[attach `00-pkgcache-launch.gif`]*

Our builds repeatedly download the same container layers, Python wheels, Node
packages, operating-system packages and Git objects. That costs time and bandwidth,
exposes CI to public-registry outages and rate limits, and becomes a hard stop on a
disconnected network.

**pkgreg is now live at `https://<cache-host>:8443`.**

It is one shared package cache for Docker/OCI, pip and uv, npm, apt and apk, Git and
generic build artifacts. The first request fetches and verifies an artifact; repeat
requests stay on our network. If several machines request the same miss together,
pkgreg opens one upstream stream and shares it with all of them as the bytes arrive.

Nothing new is installed for normal day-to-day use. `pkgreg-client` opens a temporary,
verified child shell with the package settings already applied. Type `exit` and the
bridge stops, the environment disappears, and your original shell is unchanged.

#### Start in about two minutes

1. Open **`https://<cache-host>:8443/tutorial`**.
2. Download the `pkgreg-client` binary for your operating system and architecture.
3. Check the download's SHA-256, compare the CA fingerprint with the value shared by
   the operator, then run:

Linux or macOS:

```bash
chmod +x pkgreg-client
./pkgreg-client \
  -server https://<cache-host>:8443 \
  -project global \
  -ca-sha256 "<ca-sha256>"
```

Windows PowerShell:

```powershell
.\pkgreg-client.exe `
  -server https://<cache-host>:8443 `
  -project global `
  -ca-sha256 "<ca-sha256>"
```

Then use the tools you already know:

```bash
python -m pip install numpy
uv sync --locked
npm install
git clone "$PKGREG_GIT_URL/github.com/pallets/click.git"
apt-get -o Acquire::http::Proxy="$PKGREG_APT_PROXY" update
wget "${PKGREG_FILES_URL}releases/app.tar.gz"
```

If your project protects package reads, Console → Connect adds
`-token-file ./pkgreg.token` to the exact command for you. Tokens are project- and
scope-limited; the client keeps a temporary read token in its local bridge instead of
copying it into every package manager.

Docker is the one exception because its daemon cannot see a child shell. Install trust
for that daemon only, then pull directly from pkgreg:

```bash
./pkgreg-client \
  -docker-trust \
  -server https://<cache-host>:8443 \
  -ca-sha256 "<ca-sha256>"

docker pull <cache-host>:8443/dockerhub/library/alpine:3.20
```

The Docker trust change is reversible with the same command plus `-uninstall`. On
Linux it needs `sudo`; Docker Desktop on macOS does not. Windows users should follow
the tutorial's persistent-trust path.

#### What we get

- **Faster, more predictable builds.** A warm artifact is served locally, and
  concurrent cold requests share one progressive upstream transfer.
- **One cache across the build stack.** OCI images, Python, Node, apt/apk, Git and
  generic files use one service, one project model and one console.
- **Integrity and deduplication by default.** Content is hashed while streaming,
  incomplete or corrupt transfers are never published, and identical bytes are stored
  once even when projects or ecosystems discover them through different names.
- **A real offline workflow.** Checkpoint a project while builds continue, export a
  full or delta pack, verify it on import, then serve the same package URLs with
  upstream access disabled. A missing dependency fails clearly instead of trying the
  internet.
- **Bounded storage.** Online garbage collection, LRU/TTL eviction, free-space floors
  and per-project quotas keep the cache from growing without limit. Checkpointed
  content stays pinned.
- **Shared operations and visibility.** The embedded console shows live requests,
  cache/peer/origin outcomes, storage history, source health, jobs, quotas, tokens and
  audit history. Long operations keep running as cancellable jobs.
- **No application rewrite.** Start with the temporary shell; use the explicit
  persistent setup only for CI runners and shared build hosts.

*[attach `01-one-cache.gif`]*

#### Checkpoint it without stopping builds

pkgreg records the exact keys and content digests in each project. A checkpoint is a
fast catalog operation, not a filesystem walk, so downloads keep flowing while the
restore point is created. Roll back locally or export only what changed since an older
checkpoint.

*[attach `02-versioned.gif`]*

#### Carry it across the air gap

Transfer packs include the checkpoint manifest, verified content blobs and managed Git
state. Import checks every digest and the checkpoint lineage before publishing
anything. On the disconnected side, cached tags, refs and package indexes remain
resolvable in offline mode.

*[attach `03-offline.gif`]*

#### Useful links

- **Start here:** `https://<cache-host>:8443/tutorial`
- **Operator console:** `https://<cache-host>:8443/console`
- **Public CA:** `https://<cache-host>:8443/api/ca.crt` (copy the fingerprint from
  Console → Connect or `pkgreg doctor`)
- **Health:** `https://<cache-host>:8443/readyz`

Please try one development workflow or CI job this week and share what worked, what was
unclear and which dependency or client still needs attention.

---

## Operator and technical appendix

This section describes the shipped Go implementation. It replaces the retired
Python/nginx/container/DVC instructions.

### What changed in the Go release

| Retired deployment | Current Go release |
|---|---|
| Python cache service, Python control plane, nginx and multiple containers | One `CGO_ENABLED=0` `pkgreg` binary with the console embedded |
| Separate HTTPS, apt/apk and HTTP-admin surfaces by default | One `:8443` listener by default; it multiplexes TLS and the plain-HTTP apt/apk forward proxy |
| Console at `http://host:8088` | Landing, tutorial, console, API, metrics, health and package traffic share `https://host:8443` |
| Per-project cache trees with git + DVC snapshots | One immutable content-addressed blob store plus native catalog checkpoints and verified transfer packs |
| Manual CA download plus client-specific TLS bypass flags | Fingerprint-pinned `pkgreg-client`, temporary localhost bridge, Docker-only trust mode and auditable persistent scripts |
| Polling-era control surface | Versioned API v1, server-sent live events, durable jobs and embedded historical series |

The production binary needs no Python, Node, nginx, OpenSSL, container runtime or DVC.
The Git ecosystem is the only feature with an external runtime dependency: it uses the
host's `git` executable for smart HTTP, LFS and mirror maintenance. Every other role
continues to work if Git is absent, and `pkgreg doctor` reports the warning.

### Ecosystem coverage

| Role | Clients and behavior |
|---|---|
| **OCI** | Docker and Podman; Docker Hub, GHCR and Quay; anonymous bearer authentication; multi-architecture indexes and child manifests; tag/digest pulls and `tags/list` |
| **PyPI** | pip and uv; PEP 503 HTML and PEP 691 JSON; PEP 658 metadata sidecars; hashes and `requires-python`; multiple named indexes including PyTorch |
| **npm** | npm, yarn and pnpm protocol traffic; scoped and unscoped packuments; tarball URLs rewritten back through pkgreg while unknown metadata is preserved |
| **apt/apk** | Plain forward proxy, optional upstream allowlist, mutable index revalidation and immutable package caching |
| **Git** | Read-only smart HTTP mirrors; protocol v2; full, shallow and partial clones; Git LFS; pushes refused |
| **Files** | Browsable generic artifact store; anonymous or token-gated reads; scoped-token `PUT` and `DELETE`; the only publishable ecosystem |

`global` is the default project. Other projects are URL tenants:

```text
https://<cache-host>:8443/<project>/<ecosystem>/...
```

OCI carries the project in the image name because Docker cannot use a registry base
path. The apt/apk proxy carries it as the proxy username. In the default single-port
topology, both of these use `:8443`:

```text
<cache-host>:8443/<project>/dockerhub/library/python:3.13-alpine
http://<project>@<cache-host>:8443
```

An operator can still configure separate `:8443`, `:3142` and `:8088` listeners when
the network requires it. The tutorial and Console → Connect derive the actual public
coordinates instead of assuming the defaults.

### Request pipeline

Every non-Git content request follows the same order:

1. **Hit** — serve the project's existing entry.
2. **Dedup** — link identical bytes already held under another key, project or
   ecosystem without using the network.
3. **Peer** — ask an authenticated sibling cache for the known digest.
4. **Offline check** — if the project or instance is offline, fail without dialing an
   origin.
5. **Miss** — stream one upstream fetch to every concurrent reader while hashing and
   committing it once.

A leading client disconnect does not cancel a transfer that other readers or the cache
still need. Digest or size mismatches are rejected before the catalog entry becomes
visible. Range and HEAD requests are supported where the ecosystem allows them, and
mutable names use TTL plus ETag revalidation while retaining a last-known-good offline
ref.

### Projects, access and live configuration

- Projects have owners, per-project upstreams, online/offline state, byte and artifact
  quotas, request rates and either public or token-protected package reads.
- Users are `user`, `admin` or `superuser`. Reporting relationships determine which
  projects and accounts an operator can see and manage.
- Tokens can be limited by project, ecosystem, permission and request rate. The normal
  ladder is `admin` → `write` → `read`; a separate `peer` scope grants cache-to-cache
  digest access and no user access.
- Upstream credentials are sealed in `control.db` under a host-local key. Request logs
  never include tokens or `Authorization` headers.
- Project and upstream edits are published as an immutable configuration snapshot.
  New routing is visible on the next request; there is no restart or polling delay.
- Every mutation is added to the audit log with actor, source address and time.

### Storage, maintenance and federation

- Every distinct byte string is stored once by SHA-256. Entries map project,
  ecosystem and key to that immutable blob.
- Writes stream to staging, hash in one pass, sync, then link atomically before the
  catalog row is published. A crash can cost a re-fetch, not corrupt a published
  artifact.
- Online GC removes unreferenced blobs after a grace period and rechecks references at
  deletion time.
- Eviction can enforce a global or project-scoped size target, filesystem free-space
  floor or idle TTL. Reads update recency, so frequently used content is kept longest.
- Quotas are enforced atomically with publication; an over-limit write receives HTTP
  507 with current, attempted and allowed usage.
- A checkpoint pins everything it references, so GC and eviction cannot invalidate a
  restore point.
- Peers exchange content by digest with scoped authentication, batch presence checks,
  Range support, verified streaming and per-digest single-flight. This is federation,
  not clustering: peers share content but do not replicate control state or provide
  automatic failover.

pkgreg requires a local filesystem with atomic link and rename semantics. NFS is not a
supported blob-store backend.

### Checkpoint, air-gap and recovery commands

```bash
# capture the current project state without stopping traffic
pkgreg checkpoint -project global -message "release 2026.08"

# write a full pack, or only the change from an older checkpoint
pkgreg export -project global
pkgreg export -project global -base <older-checkpoint>

# after approved media transfer into shuttle/in
pkgreg import -project global

# restore a known state
pkgreg rollback -project global -snapshot <checkpoint-id>

# warm every file referenced by a uv.lock and produce an offline-safe rewrite
pkgreg lockwarm -project global -lock uv.lock -host <cache-host>
```

Manifests are deterministic and streamed. Imports verify each blob's digest and size,
then check project lineage and fast-forward ancestry again in the apply transaction.
Managed bare Git repositories travel as deterministic archive blobs. No service pause,
tree walk, DVC checkout or git bundle is involved.

### Console and observability

The console is checked-in HTML, CSS and ES modules embedded in every release. There is
no frontend framework, bundler, Node runtime, nginx or separate UI container.

- **Overview** — readiness, live requests and current project policy.
- **Cache** — inventory, package detail, request outcomes and cache age.
- **Connect** — exact project endpoints, client downloads, CA fingerprint, generated
  setup scripts and token controls.
- **Sources** — upstreams, credentials, peer configuration and health.
- **Transfer** — checkpoints, full/delta export, verified import, rollback, lock warming,
  GC and eviction jobs.
- **Admin** — projects, users, quotas, rate limits and audit history.

One SSE connection drives live downloads, activity, job state and health. Coarse local
history retains request outcomes, storage growth and upstream health without requiring
Prometheus; `/metrics` exposes the full Prometheus surface for alerts and long-term
retention. `/healthz` reports process liveness, while `/readyz` also checks whether the
blob store can actually accept writes.

`pkgreg serve --headless` removes the landing page, tutorial and console while leaving
the data plane, API, metrics and health endpoints available.

### Client setup modes

| Mode | Use it for | Machine changes |
|---|---|---|
| default | A developer terminal or one CI step | None. A verified localhost bridge and child shell disappear on `exit`. |
| `-docker-trust` | Docker or Podman daemon access | Installs trust only for this registry; supports `-dry-run` and `-uninstall`. |
| `--persist` | Managed CI runners and shared build hosts | Runs the downloadable, auditable OS script to install CA trust, optional name resolution and durable project settings; supports dry-run and uninstall. |

The client accepts either a trusted CA file or an out-of-band SHA-256 fingerprint and
refuses plain HTTP. The five published targets are Linux amd64/arm64, macOS amd64/arm64
and Windows amd64. The server publishes them atomically with a generated checksum file:

```bash
pkgreg publish-client -data-dir /var/lib/pkgreg /path/to/release
```

### Operator commands

```bash
# clean-host production installation
sudo ./pkgreg systemd install \
  -hostnames <cache-host>,<cache-ip>

# or initialize and run directly
pkgreg init -data-dir /var/lib/pkgreg -hostnames <cache-host>
pkgreg publish-client -data-dir /var/lib/pkgreg /path/to/release
pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml
pkgreg serve -config /var/lib/pkgreg/pkgreg.yaml

# preview maintenance before deleting anything
pkgreg gc -grace 1h -dry-run
pkgreg evict -target-size 536870912000 -min-free 10737418240 -ttl 720h -dry-run
```

`pkgreg doctor` checks configuration, both databases, writable storage, hard-link
support, TLS validity and SAN coverage, Git, the embedded console, published client
downloads and the open-file limit. It reports all detected issues in one run.

The resumable cutover tool imports a live retired deployment without changing its
source:

```bash
pkgreg migrate from-python \
  -source /srv/package-registry/caches \
  -registry-dir /srv/package-registry/config \
  -data-dir /var/lib/pkgreg
```

It migrates projects, users, inventories, refs, statistics, files, shared CAS content
and managed Git mirrors. A qualified 119 GB production-shaped tree completed its
corrected first pass in 52.7 seconds; the immediate resume pass did no new work.

### Verification evidence

- Live hard-offline Python/Go differential gate: **46/46** request and command cases,
  including forward-proxy traffic and full, shallow and filtered Git clones.
- Single-flight load gate: **20 readers × 2 GiB**, with every reader receiving the
  complete artifact and the origin seeing exactly one request.
- Native checkpoint qualification: **32,000 catalog rows / 119 GiB logical cache in
  126 ms** without pausing traffic.
- Real-client acceptance coverage for pip, uv, npm, Docker, apt-get, apk, Git and wget.
- Full tests, targeted race detector, vet, fuzz coverage and a static build are merge
  gates.

### Deliberate boundaries

1. pkgreg is a pull-through cache, not a private publishing registry. Package
   ecosystems are read-only; the generic files role is the intentional write path.
2. Peering is digest federation, not a highly available cluster or state replication.
3. The apt/apk listener is a plain forward proxy; pkgreg does not tunnel arbitrary
   HTTPS traffic.
4. The store requires a local filesystem; NFS is unsupported.
5. The default data plane is public to the trusted network unless a project is switched
   to token authentication. Network reachability remains part of the security boundary.
