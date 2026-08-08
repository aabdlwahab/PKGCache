# pkgreg

The Go rearchitecture of package-registry: six pull-through package caches, an
operator console, and air-gap transfer, in **one static binary with no containers**.

Design: [go-architecture.md](../docs/go-architecture.md) ·
Build plan: [go-implementation-plan.md](../docs/go-implementation-plan.md) ·
Language rationale: [language-choice.md](../docs/language-choice.md)

## Status

**Phases 0–10 are implemented.** The Go binary now includes bounded-storage
maintenance and peer federation, the embedded API-v1 console, resumable Python-cache
migration, differential cutover tooling, and clean-host operations commands. The
live hard-offline Python/Go differential gate passed 46/46 cases over independent
warm cache snapshots. Client-onboarding Phases 1–4 are also implemented: generated
Linux/macOS/Windows setup scripts, build-resolution options, and the signed
`pkgreg-client` release pipeline.

The Phase 2 audit found and fixed one late-arrival race: a completed fetch used to
leave the in-flight registry just before its catalog entry became visible, which
could produce a second upstream transfer. Publication now happens before completion
is announced, with a permanent ordering test.

| Phase | Task | State |
|---|---|---|
| 0 | S1 — pure-Go SQLite under load | done ([report](../docs/spikes/s1-sqlite-driver.md)) |
| 0 | S2 — single-flight and progressive delivery | done ([report](../docs/spikes/s2-progressive-delivery.md)) |
| 0 | S4 — native CA + leaf minting | done (`internal/pki`, verified against a real TLS handshake) |
| 1 | P1-01 scaffold, Makefile, CI config, buildinfo | done |
| 1 | P1-02/03/04 logging, event bus, metrics | done |
| 1 | P1-05/06 config: sources, validation, atomic snapshot store | done |
| 1 | P1-07/08 blob store: inline-hash writer, atomic commit, crash recovery | done |
| 1 | P1-09/10/11 catalog: schema, migrations, SQLite store | done |
| 1 | P1-12 batched entry writes + LRU read cache | done |
| 1 | P1-13 PKI | done |
| 2 | P2-01/02 upstream pool, credentials, OCI anonymous bearer | done |
| 2 | P2-03 single-flight + progressive tail-follow delivery | done |
| 2 | P2-04/05 serve pipeline: hit / dedup / offline / miss, Range, HEAD | done |
| 2 | P2-06/07 refs and documents: TTL + ETag revalidation | done |
| 2 | M2 load qualification: 20 readers × 2 GiB, exactly one origin request | done ([report](../docs/load/m2-2gb.md)) |
| 3 | P3-01 ecosystem registry, descriptors, adapter context | done |
| 3 | P3-02 escape-safe router and permanent S3 corpus | done |
| 3 | P3-03 generic files store, write token, autoindex | done |
| 3 | P3-04 npm packuments, URL rewrite, tarball streaming | done |
| 3 | P3-05 PEP 503/691 PyPI, metadata sidecars, inventory | done |
| 3 | P3-06 real npm/uv/pip/wget client acceptance | done |
| 4 | P4-01 OCI ping, manifests, blobs, tags/list | done ([report](../docs/phase4-oci-apt.md)) |
| 4 | P4-02 multi-arch child back-fill and deduplicated image size | done |
| 4 | P4-03 offline tag refs and project-prefixed tags/list | done |
| 4 | P4-04/05 apt absolute proxy, allowlist, index revalidation | done |
| 4 | P4-06 apk indexes and apt/apk artifact inventory | done |
| 4 | P4-07 real multi-arch Docker, apt-get, and apk clients | done |
| 5 | P5 managed Git mirrors, smart HTTP, LFS, maintenance | done ([report](../docs/phase5-git.md)) |
| 6 | P6 project routing, listeners, TLS reload, graceful drain | done ([report](../docs/phase6-listeners-routing.md)) |
| 7 | P7 control DB, auth, projects/upstreams, jobs, API/SSE/audit | done ([report](../docs/phase7-control-plane.md)) |
| 8 | P8 snapshots, rollback, export/import, lockwarm, air-gap CLI | done ([report](../docs/phase8-air-gap.md)) |
| 9 | P9 GC, eviction, quotas, federation, scheduling, dashboard, rate limits | done ([report](../docs/phase9-scale-features.md)) |
| 10 | P10 API-v1 console, embedded web, migration, differential and operations | done; live gate 46/46 ([runbook](../docs/phase10-cutover.md)) |
| follow-on | Client onboarding Phases 1–4: scripts, build DNS, signed wrapper | implemented ([design/status](../docs/client-onboarding.md)) |

M2 passed on 2026-07-27: 20 concurrent readers each verified the complete 2 GiB
artifact while the origin saw exactly one request. The opt-in permanent load test
and measured result are in the [M2 report](../docs/load/m2-2gb.md). Peer retrieval
landed in Phase 9 (`P9-04`). Credential application and the OCI bearer flow are present;
upstream credentials are sealed in `control.db` under the host-local key.

## Try it

```bash
make build

./bin/pkgreg init -data-dir /tmp/pkgreg -hostnames cache.example.com
make client-publish DATA_DIR=/tmp/pkgreg
./bin/pkgreg doctor -config /tmp/pkgreg/pkgreg.yaml
./bin/pkgreg serve  -config /tmp/pkgreg/pkgreg.yaml

curl --cacert /tmp/pkgreg/certs/ca.crt https://localhost:8443/healthz
curl --cacert /tmp/pkgreg/certs/ca.crt https://localhost:8443/readyz
curl --cacert /tmp/pkgreg/certs/ca.crt https://localhost:8443/metrics
```

`init` mints a CA and a server certificate with `crypto/x509` — there is no
`openssl` anywhere. An existing `ca.crt`/`ca.key` pair is always reused, so trust
already distributed to build hosts stays valid.

It also provisions the `admin` superuser and prints its generated password once, which
is what turns control-plane authentication on. Save it, or delete the account and
re-run `init` to get another.

`doctor` exits **4** on the configuration above, because `:8443` answers on every
interface with an empty `server.proxy_allowlist` — a working open HTTP relay. That is
the intended report, not a failure of the trial. Keep a local run on loopback and it
exits 0:

```bash
./bin/pkgreg serve -config /tmp/pkgreg/pkgreg.yaml -unified-addr 127.0.0.1:8443
```

### Publishing the client

The client is served from disk, not embedded — five platforms at ~7 MB each would
nearly double a server whose whole point is being one lean static binary. So an
operator publishes it explicitly, and until they do, `/tutorial` has nothing to hand
out and a new developer's first page is a dead end. `pkgreg doctor` reports it.

`make client-publish DATA_DIR=…` cross-compiles all five and publishes them, which
is right on a machine with the Go toolchain. On a production host there is no
toolchain and no Makefile — just the binary — so publishing is a subcommand of the
server itself:

```bash
# Copy bin/pkgreg-client-* and bin/pkgreg-client-SHA256SUMS onto the cache host, then:
pkgreg publish-client -data-dir /var/lib/pkgreg /path/to/release
pkgreg publish-client -data-dir /var/lib/pkgreg   # no path: scans the dir holding pkgreg
```

It installs each binary atomically, records SHA-256 digests in `sha256sum` format, and
verifies any sums file that travelled with the release — so a damaged copy onto an
air-gapped host is refused rather than served. `internal/clientrelease` owns that
filename grammar and digest format for the publisher, the download API and `doctor`
alike.

Compare the CA fingerprint through a trusted channel, then open a temporary shell:

```bash
./pkgreg-client -server https://localhost:8443 -ca-file /tmp/pkgreg/certs/ca.crt
# run package commands, then:
exit
```

The default uses a verified localhost bridge and changes nothing outside the child
shell.

Docker is the exception, because its daemon never sees that shell — and on macOS and
Windows it runs in a virtual machine whose loopback is not the developer's, so the
bridge address is unreachable and a pull fails with `connection refused`. Installing
the CA for the daemon alone is its own mode: one file, no administrator access on
Docker Desktop, and `-uninstall` to reverse it.

```bash
./pkgreg-client -docker-trust -server https://localhost:8443 \
  -ca-file /tmp/pkgreg/certs/ca.crt -dry-run
./pkgreg-client -docker-trust -server https://localhost:8443 \
  -ca-file /tmp/pkgreg/certs/ca.crt
docker pull localhost:8443/dockerhub/library/alpine:3.20
```

`~/.docker/certs.d/<host>:<port>/ca.crt` is what Docker Desktop reads, verified against
Docker Desktop 29.6.2 on darwin/arm64. A Linux daemon reads `/etc/docker/certs.d`
instead, so the same command needs `sudo` there. Windows has no such directory — a path
named `<host>:<port>` is not expressible — so Docker Desktop for Windows takes registry
trust from the certificate store, which `--persist` populates.

For a managed build host that needs machine-wide trust and reusable settings, preview
and apply the explicit persistent path:

```bash
./pkgreg-client --persist -server https://localhost:8443 \
  -ca-file /tmp/pkgreg/certs/ca.crt -dry-run
sudo ./pkgreg-client --persist -server https://localhost:8443 \
  -ca-file /tmp/pkgreg/certs/ca.crt
```

The generated `.sh` and `.ps1` files remain available beside the client for auditing
that persistent setup. Prefer a hostname published in site or builder DNS.

## Deploy to a server

For a real host, starting from release binaries — no Go toolchain, no Makefile. Run as
root. Upgrading an existing instance is [UPGRADING.md](UPGRADING.md); this is a clean
install.

```bash
CACHE_HOST=cache.example.com          # the DNS name clients will use
RELEASE=/root/pkgreg-release          # dir holding the binaries you copied over
```

**1. Install the unit and the binary.** `systemd install` copies the binary it is run
from into `/usr/local/bin` and writes a hardened unit; `-start=false` holds off so the
first start is deliberate.

```bash
cd "$RELEASE"
chmod +x pkgreg-linux-amd64
./pkgreg-linux-amd64 systemd install -hostnames "$CACHE_HOST" -start=false
```

**2. First start.** The unit's `ExecStartPre` runs `pkgreg init`, which mints the CA,
writes the configuration, and provisions the `admin` superuser.

```bash
systemctl daemon-reload
systemctl start pkgreg
```

**3. Capture the admin password.** Printed once, to the journal, and only its scrypt
digest is stored.

```bash
journalctl -u pkgreg --since "5 minutes ago" | grep -A6 "CONTROL-PLANE LOGIN"
```

**4. Publish the clients**, or `/tutorial` has nothing to hand a developer.

```bash
pkgreg publish-client -data-dir /var/lib/pkgreg "$RELEASE"
```

**5. Close the forward proxy.** An empty `server.proxy_allowlist` relays plaintext HTTP
to any host for any caller that can reach the port — including addresses only this host
can route to. List the repositories you actually mirror, or `["*"]` to accept relaying
anywhere as a deliberate, greppable choice.

```bash
$EDITOR /var/lib/pkgreg/pkgreg.yaml
systemctl restart pkgreg
```

```yaml
server:
  proxy_allowlist:
    - archive.ubuntu.com
    - deb.debian.org
    - "*.alpinelinux.org"
```

**6. Verify.** `doctor` is read-only and exits non-zero on an unsafe posture, so it is
usable as the gate in a provisioning script.

```bash
pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml; echo "exit=$?"     # want 0
curl -fsS --cacert /var/lib/pkgreg/certs/ca.crt "https://$CACHE_HOST:8443/readyz"
systemctl status pkgreg --no-pager
```

**7. Hand out the fingerprint** over a channel that is not the cache itself — that
comparison is what makes a developer's first download safe.

```bash
pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml | grep '  ca '
```

Developers then open `https://$CACHE_HOST:8443/tutorial`. The console also offers
**Browse as guest**: read-only access to the global project with no account, which is
`auth.guest_read` and is on by default.

### Four things that bite

**Step 3 is one-shot.** To reissue the credential, delete the account and re-run init:

```bash
sqlite3 /var/lib/private/pkgreg/db/control.db "DELETE FROM users WHERE username='admin';"
pkgreg init -data-dir /var/lib/pkgreg
```

**Steps 4 and 5 must follow step 2.** The unit uses `DynamicUser=yes`, so
`/var/lib/pkgreg` is a symlink into `/var/lib/private/pkgreg` that does not exist until
the service has started once. Running `init` by hand beforehand puts a real directory
where systemd expects that symlink; the order above avoids the question rather than
depending on how a given systemd version resolves it.

**Between steps 2 and 5 the instance is an open relay.** On an untrusted network,
firewall the port before step 2 and open it after step 6.

**`-data-dir` must be a single directory directly under `/var/lib`** — `systemd install`
rejects anything else. A package cache usually wants a dedicated mount, so bind-mount
it before step 2 and add the matching fstab entry:

```bash
mount --bind /data/pkgreg /var/lib/pkgreg
```

## Layout

```
cmd/pkgreg/          serve/init/doctor/audit/publish-client plus air-gap commands
internal/
  app/               composition root — the only place construction happens
  blob/              content-addressed store; one copy of every distinct byte-string
  buildinfo/         link-time build identity
  catalog/           metadata: blobs, entries, refs, artifacts, snapshots, stats
  clientrelease/     what a published client release looks like on disk
  config/            every tunable; immutable snapshots swapped atomically
  control/           database, auth, sealed credentials, projects, jobs, API/SSE
  engine/            the cache pipeline: single-flight, progressive delivery,
                     freshness, refs, documents
  eco/               adapter contract plus files, npm, PyPI, OCI, apt/apk and Git
  listener/          single-port TLS/plain split and atomic certificate reload
  lockwarm/          uv.lock parse, bounded warm and URL-preserving rewrite
  obs/               slog, event bus, Prometheus metrics
  onboarding/        generated Linux/macOS and Windows client setup scripts
  ops/               checkpoint, rollback, export/import and managed-tree apply
  pki/               CA and leaf certificate minting
  race/              build-tag constant so perf tests skip loudly under -race
  snapshot/          streamed manifests, diffs and verified transfer packs
  testutil/upstream/ synthetic origin with failure injection
  upstream/          outbound HTTP, credentials, OCI bearer tokens
```

Each package's doc comment explains *why* it is shaped the way it is; start there.

## The four concepts

Everything is built on these, and they are what let garbage collection, eviction,
quotas and air-gap snapshots exist at all:

| | |
|---|---|
| **Blob** | Immutable bytes, addressed by sha256. Never rewritten in place. |
| **Entry** | `(project, eco, key) → blob`. The byte cache; what a GET resolves. |
| **Ref** | `(project, eco, name) → target + freshness`. OCI tags, git refs, apt `Release`, npm dist-tags — one table instead of four bespoke mechanisms. |
| **Artifact** | The semantic inventory the console lists. |

Two properties do most of the work:

- **Every write is hashed as it streams**, so deduplication is universal rather than
  limited to content whose digest was known in advance. Three projects caching the
  same 2.5 GB wheel hold one blob.
- **A blob is immutable once linked**, which is why concurrent GC, live snapshots and
  cross-project hardlinking need no locking.

The cache pipeline resolves every request the same way, whatever the ecosystem:

```
1. entry hit          the catalog already maps this key to a blob
2. dedup              the expected digest is already held, from any project — link it
3. offline            serve nothing rather than reach upstream
4. single-flight      one fetch, N readers tail-following it as it lands
```

## Development

```bash
make test     # go test ./... (real-client tests run when their binaries are present)
make race     # mandatory before merge
make lint     # golangci-lint
make release  # static linux/amd64 + linux/arm64 with the console embedded
```

Ground rules are in [go-implementation-plan.md §1](../docs/go-implementation-plan.md).
The ones that bite most often:

- Nothing outside `internal/config` reads an environment variable.
- Every goroutine has a documented owner and a stop path.
- Performance assertions skip with `if race.Enabled { t.Skip(race.SkipReason) }` —
  never a `//go:build !race` tag, which would make them vanish silently from a
  `-race`-only CI job. The detector costs 5–20×, so thresholds are meaningless under
  it. Correctness tests always run instrumented.
- A Prometheus `*Vec` contributes nothing to a scrape until a label combination
  exists. Pre-create the bounded ones (`obs.InitProjectSeries`) so dashboards read 0
  rather than "no data", and assert on full exposition lines, never bare names.

## Dependencies

Deliberately small; this project exists to make dependency trees someone else's
problem.

| Module | Why stdlib is insufficient |
|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite. No CGO, so the binary is genuinely static. |
| `gopkg.in/yaml.v3` | Config file parsing with unknown-key rejection. |
| `github.com/prometheus/client_golang` | Correct histograms, exposition format, runtime collectors. |
| `golang.org/x/sync` | `singleflight` for document fetches; `errgroup`. |

No web framework, no ORM, no logging library, no go-git. The only external *runtime*
dependency is `git`, needed solely by the git mirror ecosystem; `pkgreg doctor`
reports its absence as a warning and everything else still serves.
