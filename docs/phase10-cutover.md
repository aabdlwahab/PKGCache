# Phase 10 — console, migration, and cutover

Status: **COMPLETE — live differential cutover gate passed** (2026-07-28).

Phase 10 removes the last production dependencies on the Python control plane,
nginx, containers, and DVC. A production release is one static Go binary with the
console embedded.

## Delivered

| Plan item | Evidence |
|---|---|
| P10-01 API v1 console | The embedded ES-module console and `console/api.js` use only `/api/v1/*`; no framework or frontend build step remains |
| P10-02 descriptors | Ecosystem cards, filters, setup instructions, and upstream selectors render the `/api/v1/ecosystems` response without a frontend ecosystem enum |
| P10-03 SSE | One `EventSource` drives live downloads, recent activity, job state, and health; historical chart series refresh once per minute |
| P10-04 scale panels | Project byte/artifact quotas, offline/data-plane auth, project/token rate limits, GC/eviction jobs, tokens, origins, peers, snapshots, and jobs have working API v1 controls |
| P10-05 embedded web | `internal/web` always embeds the checked-in console; `/`, `/tutorial`, `/console`, and ETag-revalidated assets are served by Go with the former nginx CSP byte-for-byte, while `--headless` disables the browser surface |
| P10-06 migration | `pkgreg migrate from-python` imports projects, users, ledgers, refs, statistics, files, shared CAS content, and managed Git mirrors; progress is durable and idempotent |
| P10-07 parity | A live hard-offline Python/Go run passed all 46 request and command cases, including forward-proxy traffic and full/shallow/filtered Git clones |
| P10-08 operations | `init`, expanded `doctor`, and `systemd install` provide initialization, diagnostics, hardened service installation, and first-start enablement |

## Real 119 GB qualification

The importer was run against the repository's production-shaped `caches/` tree:

```text
source size              119 GB
source files             32,080
legacy CAS objects       8,788
projects                 global, gamma, gamma-2, proja

corrected first pass     52.717 s
CAS bytes linked         61,997,793,161
catalog entries          12,872
artifacts                7,664
refs                     130
managed Git files        111

immediate resume         0.913 s
durable skips            29,517
new links/hashes/entries 0 / 0 / 0
```

The validation destination was isolated and removed afterward. The source content
was not renamed, deleted, or rewritten. A migration-specific blob commit path avoids
touching timestamps on source CAS inodes while they remain hardlinked.

An earlier exploratory pass exposed that ordinary blob commits refreshed the mtime
of a destination hardlink and therefore also its legacy source inode. No bytes or
paths changed, but the resume pass correctly treated those mtimes as new work. That
issue produced `CommitImported` and its regression test. The live cutover run then
exposed that migrated OCI refs lacked immediately serveable mutable-tag entries; the
importer now materializes those aliases. The table above is a fresh run of the
corrected importer, followed immediately by a measured zero-work resume.

## Clean-host installation

Download or copy a release binary to a Linux host with `git` installed, then run:

```sh
sudo ./pkgreg systemd install \
  -hostnames cache.example.internal,10.20.30.40
```

This copies the current binary to `/usr/local/bin/pkgreg`, installs a hardened
`pkgreg.service`, initializes `/var/lib/pkgreg` on first start, mints the CA and
server certificate, writes the starter configuration, and runs
`systemctl enable --now pkgreg.service`.

The unit uses `DynamicUser`, `StateDirectory`, a high file-descriptor limit,
restart-on-failure, and systemd filesystem/kernel hardening. It listens on the
single-port default `:8443`; the landing page and console are on that same listener.

Before onboarding clients:

```sh
cd go
make client-release
sudo make client-install DATA_DIR=/var/lib/pkgreg
sudo /usr/local/bin/pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml
curl --fail --cacert /var/lib/pkgreg/certs/ca.crt \
  https://127.0.0.1:8443/readyz
```

The publish target builds and checksum-verifies all five client targets before
placing them in the data directory. The running server notices them immediately; no
restart is required.

`doctor` checks configuration, catalog/control databases, writable blob storage,
hardlink support, TLS validity/SAN coverage, `git`, embedded console presence, and
the open-file limit.

Use `-start=false` to stage the binary and unit without calling systemctl. An
existing different unit or binary is preserved unless `-force` is explicit.

## Live migration runbook

The Python source may keep serving during the long pass:

```sh
sudo /usr/local/bin/pkgreg migrate from-python \
  -source /srv/package-registry/caches \
  -registry-dir /srv/package-registry/config \
  -data-dir /var/lib/pkgreg \
  -checkpoint=false
```

The durable resume database is
`/var/lib/pkgreg/migration/from-python.db`. Repeating the same command imports only
new or atomically replaced source files. Shared `.cas/sha256` objects are linked by
their filename digest without a hashing pass; cross-filesystem migration copies
them once without re-hashing.

Cut over in this order:

1. Take and verify the final Python/DVC checkpoint. This is the rollback point.
2. Run the bulk migration above while Python remains live.
3. Run the differential corpus against Python and a Go instance using separate warm
   cache copies. The exact command and required fixture variables are in
   [`go/test/differential/README.md`](../go/test/differential/README.md).
4. Stop the compose stack.
5. Re-run `migrate from-python` without `-checkpoint=false`. The incremental pass
   captures the final writes and creates native checkpoint #1 for every project
   that has no native head.
6. Run `pkgreg doctor`.
7. Run `pkgreg systemd install`; point it at the already migrated
   `/var/lib/pkgreg`.
8. Verify `/readyz`, one cached pull per ecosystem, an offline miss, and the console.

Keep the stopped compose stack, original caches, and DVC history for one release
cycle. Rollback is stop `pkgreg`, restart compose on its untouched paths, and restore
the pre-cutover DVC checkpoint if needed.

## Differential gate

The checked-in corpus covers:

- PyPI simple HTML/PEP 691 JSON, wheel GET/HEAD/Range, and metadata;
- npm plain/scoped packuments and tarballs;
- OCI version, multi-arch/child manifests, blobs, and global/project tag lists;
- apt conditional `InRelease`, deb, and apk forward-proxy requests;
- files empty-root autoindex, missing reads, and hard-offline write/delete refusal;
- Git info/refs and full, shallow, and filtered clone;
- the legacy compatibility API. API v1 is covered by its Go integration suite
  because the retired Python server does not implement that namespace.

The live run used independent XFS copy-on-write snapshots of the same 119 GB logical
cache. Python ran on its real three-surface topology (TLS package origin, plain
apt/apk proxy, HTTP admin API); Go ran hard-offline on an isolated single port.
Neither deployment shared a writable tree, and the production Compose project and
ports were not modified.

```text
live corpus             PASS
request/command cases   46 / 46
Python                  https :38443, proxy :33142, admin :38088
Go                      single port :28080
Git modes               full, depth=1, filter=blob:none
```

The gate found and fixed real compatibility defects before passing:

- historical PyPI `data-yanked`, explicit JSON `requires-python: null`, and the
  retired snake_case cache-file format;
- OCI tag refs that migrated but were not serveable by tag;
- apt/apk media type parity;
- the empty files root, retired authorization ordering, and exact plain-text errors;
- the legacy proxy service/role contract, live-source list, endpoint instructions,
  shuttle empty-list shape, and packages defaults;
- split topology/CA trust in the harness and checkout-tree Git comparison.

Framing and deployment-specific values are treated deliberately. Hop-by-hop headers,
opaque validators, generated storage roots/listener ports, and rolling/historical
telemetry cannot be equal across two independent processes and are excluded per
case. Cached bodies, generated package metadata, statuses, protocol headers,
stable legacy fields, endpoint notes, and Git checkout content remain strict.
The exact invocation and rules are documented in
[`go/test/differential/README.md`](../go/test/differential/README.md).

## Verification

```sh
cd go
go test ./...
go test -race ./internal/blob ./internal/catalog ./internal/migrate/frompython \
  ./internal/web ./internal/app
go vet ./...
CGO_ENABLED=0 go build ./cmd/pkgreg
```

The console is checked-in HTML, CSS, and ES modules embedded by every build, so
verification has no Node step and no console build tag.
