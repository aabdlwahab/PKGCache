# Client onboarding: a name, a cert endpoint, and a setup script

> **Setting up a developer machine?** Use the shorter
> [`pkgreg-client` tutorial](../go/internal/web/dist/tutorial.html). This page records
> the design, security reasoning, and platform verification for maintainers.

**Status:** Phases 1–4 are implemented (2026-07-28). The Go control plane serves
the public CA and project-scoped Linux/macOS/Windows setup scripts; build resolution
has a qualified per-build path and an optional forwarding resolver; and
`pkgreg-client` has a five-target signed-release pipeline. Claims marked
*(verified)* were tested against a live stack. Ubuntu, Alpine, Red Hat UBI, and Arch
privileged install/trust/idempotency/uninstall runs passed locally; the macOS and
Windows privileged jobs are encoded in CI and require their native hosted runners.

## The proposal

> Serve the CA from an endpoint, and ship an OS-agnostic executable that installs it
> on the client machine, adds the cache's name to that machine's resolution (and to
> the Docker builder's), so everything is reachable as one stable hostname.

Three separable parts, three different answers.

| Part | Verdict | Why |
|---|---|---|
| Endpoint serving the CA | **Do it** | Small, replaces an `openssl s_client` scrape, extends something that already exists |
| A stable hostname | **Do it — but not `.dev`** | Real benefits (fixes a pip bug, decouples cert from IP), wrong TLD |
| A compiled cross-OS installer | **Ship both** | The script remains the auditable primitive; a signed wrapper adds fingerprint verification and managed rollout |

## Why not `pkgcache.dev`

`.dev` is a real ICANN gTLD operated by Google, and the **entire TLD is on the HSTS
preload list** compiled into Chrome, Edge and Firefox. Browsers force HTTPS under it
and refuse click-through on certificate warnings — which is precisely the situation a
private CA creates. Separately, it is someone else's namespace: a machine that leaves
the network, or a query that escapes the local resolver, reaches whoever owns the
name rather than failing closed.

Use **`pkgcache.internal`**. ICANN reserved `.internal` in 2024 for exactly this
purpose and guarantees it will never be delegated. Avoid `.local` as well — that is
mDNS/Bonjour territory and causes multicast resolution surprises.

The naming idea is worth doing on its own merits:

- **It fixes a real failure.** pip < 26 cannot use an IP-literal HTTPS index at all:
  its vendored urllib3 omits SNI for IP addresses and pip's trust backend rejects the
  socket with `ValueError: check_hostname requires server_hostname`. A hostname makes
  this disappear. *(verified — pip 25.0.1 fails against the IP, succeeds against a
  name; pip 26.1.2 succeeds against both.)*
- The cert stops being tied to an IP, so re-addressing the host does not invalidate
  every client.
- Published on `443` instead of `8443`, image references lose the port:
  `docker pull pkgcache.internal/dockerhub/library/python:3.12-slim`.

## What a machine-local name reaches — and what it does not

This is the part to get right before building anything, because "add it to the
machine's DNS" covers less than it appears to.

| Consumer | Resolves via | Covered by a hosts-file entry? |
|---|---|---|
| `docker pull` / `FROM` | the **host's** resolver (the daemon runs on the host) | ✅ yes |
| `RUN` steps inside a build | the **build container's** own `/etc/hosts` | ❌ **no** |
| `docker run` containers | the container's own `/etc/hosts` | ❌ no |
| pip / npm / git run on the host | host resolver | ✅ yes |

*(verified: a build whose `RUN` does `getent hosts pkgcache.internal` fails without
`--add-host` and succeeds with it; build containers do not inherit the host's hosts
file.)*

So a hosts entry gets you the `FROM` line but not the `RUN` lines underneath it.
Three ways to close that gap, in increasing order of how well they scale:

**A — per-build flags.** `docker build --add-host pkgcache.internal:<ip>`, or
`extra_hosts:` in compose. Works today, no infrastructure, but it has to appear in
every build invocation, which is exactly the kind of thing that gets forgotten.

**B — a resolver on the cache host.** Run the optional forwarding dnsmasq service,
which answers for `pkgcache.internal` and forwards everything else upstream.
Client machines can point at it, and Docker/BuildKit can be configured explicitly
with the resolver address. The daemon-level setting applies to all containers, so
forwarding non-cache queries is mandatory. This path is now verified with an
isolated dnsmasq network and a `docker-container` BuildKit builder configured through
`buildkitd.toml`; the checked-in example does not rely on undocumented inheritance
from the host's `/etc/hosts`.

**C — a record in site DNS.** One A record covers hosts and containers everywhere with
no client software at all. Out of scope per the current constraint (no site DNS
control), but worth revisiting: it deletes most of this document.

## Why "install the CA once" is not enough

An installer that only touches the OS trust store leaves half the tools broken,
because several ship their own root store and ignore the system one.

| Trust store | Read by |
|---|---|
| OS store (`update-ca-certificates`, keychain, Windows Root) | docker daemon, git, curl, wget, uv |
| `certifi` bundle | **pip < 24.2 — ignores the OS store** |
| Node's bundled roots | **npm — ignores the OS store** |
| Java `cacerts`, .NET, Firefox NSS | their own, separately |

The installer therefore has to configure tools individually — the same matrix as the
per-client flags in the README's [Route 1](../README.md#route-1-default--skip-verification-for-this-one-host-no-cert-files),
just executed for the user. That is fine; it just means the CA install is not the
interesting part, and "one cert install fixes everything" should not be promised.

It also **cannot fix builds**: `RUN` steps run in containers that have neither the
host's trust store nor its hosts file. [`examples/porting/`](../examples/porting/)
remains the answer there. The installer's scope is developer workstations and CI
hosts, not images.

## Why the script remains the persistent primitive

- **There is no single OS-agnostic executable.** The release is five Go binaries:
  Linux amd64/arm64, macOS amd64/arm64, and Windows amd64.
- **Signing is a recurring operational dependency.** macOS needs Developer ID
  signing and notarization; Windows needs Authenticode signing and a timestamp.
  The pipeline is implemented, but releases intentionally fail closed when those
  credentials are absent.
- **A binary that silently installs a root CA is malware-shaped.** Endpoint security
  tooling treats it accordingly. `pkgreg-client` therefore defaults to a temporary
  localhost-backed child shell and writes nothing to the machine. Only explicit
  `--persist` downloads the readable platform script, verifies its embedded CA, and
  executes or prints it.
- The script remains directly downloadable for audit and environments that do not
  want a release binary.

## Build plan

### Phase 0 — decide the name (½ day, decision only)

Settle on `pkgcache.internal` (or a subdomain of an owned domain, which would switch
the whole plan to a publicly-trusted cert and delete phases 1–2). Add it to `SANS` in
[`scripts/gen-certs.sh`](../scripts/gen-certs.sh) and re-mint the server cert; the CA
is reused, so trust already distributed stays valid.

### Phase 1 — CA endpoint — implemented

- `GET /api/ca.crt` is preserved by the Go control plane with
  `Content-Type: application/x-pem-file`. It serves only the configured public CA;
  no private key enters a response.
- The client bootstraps only this public CA over an otherwise unverified HTTPS
  connection and compares it byte-for-byte with the out-of-band fingerprint. It then
  builds a normally verifying TLS client from that CA and uses the verified
  connection for the setup script. The executable script is never accepted over the
  bootstrap connection.
- The API-v1 endpoint response and console surface a direct download link and the
  SHA-256 fingerprint for out-of-band comparison.

### Phase 2 — generated setup script — implemented

The Go control plane renders:

- `GET /api/v1/projects/{project}/setup.sh`
- `GET /api/v1/projects/{project}/setup.ps1`
- compatibility aliases `GET /api/setup.sh?project=...` and
  `GET /api/setup.ps1?project=...`

The routes use the request host and configured public listener ports, or the pinned
`PUBLIC_ORIGIN` when one is configured. They require visibility of the selected
project. The console links all three downloads and displays the CA's colon-separated
SHA-256 fingerprint; `/api/ca.crt` publishes the same value in
`X-Pkgreg-CA-SHA256`.

The scripts embed the public CA, project endpoints, and no credentials. This is a
deliberate change from the early design: the Go control plane stores token hashes,
not recoverable token secrets, and a read/setup route must never mint or disclose a
write credential. Operators supply a write token separately when a write operation
needs one.

What it does, per platform:

| Step | Linux | macOS | Windows |
|---|---|---|---|
| CA into OS store | `/usr/local/share/ca-certificates` + `update-ca-certificates` (Debian/Alpine); `/etc/pki/ca-trust/source/anchors` + `update-ca-trust` (RHEL); `/etc/ca-certificates/trust-source/anchors` + `trust extract-compat` (Arch) | `security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain` | `Import-Certificate` into `LocalMachine\Root` |
| Docker | `/etc/docker/certs.d/<host>/ca.crt` | system keychain plus the same daemon path | inherited from `LocalMachine\Root` by Docker Desktop/Windows daemon |
| Name | append to `/etc/hosts` | `/etc/hosts` | `%SystemRoot%\System32\drivers\etc\hosts` |
| pip / npm / git / uv | managed project environment loaded through `/etc/profile.d` | managed project environment loaded through `/etc/zprofile` | reversible machine environment values, with previous values saved in `state.json` |

Both variants are idempotent, support dry-run and uninstall modes, manage an optional
hosts entry (`--cache-ip` / `-CacheIP`), and fail with a clear message unless the
operator explicitly started them as root/Administrator. They never invoke `sudo` or
request elevation themselves. CA and Docker trust are reference-aware across
multiple installed projects using the same instance CA. On Linux/macOS the installer
prints the exact `source /etc/pkgreg/projects/<project>/env.sh` command for activating
settings immediately; otherwise start a login shell. Windows environment changes
appear in newly opened processes.

### Phase 3 — resolution for builds — implemented

The baseline is explicit and portable:

- `docker build --add-host pkgcache.internal:<cache-ip> ...`
- Compose `build.extra_hosts`

[`examples/build-resolution/`](../examples/build-resolution/) contains both forms,
a BuildKit `[dns]` configuration example, and daemon-level DNS guidance. The optional
`dns` Compose profile builds [`dnsmasq/`](../dnsmasq/) and requires explicit cache
address/upstream resolver configuration instead of guessing host networking.

[`scripts/verify-build-resolution.sh`](../scripts/verify-build-resolution.sh)
qualified four paths on 2026-07-28: Docker `--add-host`, Compose
`build.extra_hosts`, isolated dnsmasq authoritative plus forwarded lookup, and a
`docker-container` BuildKit builder using the configured resolver. All passed.

### Phase 4 — signed client wrapper — implemented

[`go/cmd/pkgreg-client`](../go/cmd/pkgreg-client/) is a small cross-platform wrapper
that requires either `-ca-sha256` or a trusted `-ca-file`. Its default mode downloads
and verifies `/api/ca.crt`, starts the shared loopback bridge on an ephemeral port, and
opens a child shell with project-scoped environment values. Exiting that shell stops
the bridge and restores the parent environment without an uninstall step.
For a token-gated project, `-token-file` keeps the read token in the bridge process
and adds it to package requests without writing credentials into each tool's config.

`-docker-trust` is a third mode, between the temporary session and `--persist`. The
Docker daemon is a separate process that never sees the session's environment, and under
Docker Desktop it runs in a VM whose loopback is not the developer's — so the bridge
address the session exports is unreachable and `docker pull` fails with
`connection refused`. This mode verifies the CA against the pinned fingerprint and then
installs it for the daemon only:

| Platform | Location | Privilege |
|---|---|---|
| macOS (Docker Desktop) | `~/.docker/certs.d/<host>:<port>/ca.crt` | none |
| Linux (dockerd) | `/etc/docker/certs.d/<host>:<port>/ca.crt` | root |
| Windows (Docker Desktop) | not applicable — a `<host>:<port>` path is not expressible; use `--persist`, which imports into `LocalMachine\Root` | Administrator |

It honours `DOCKER_CONFIG` where certs.d is genuinely client-side, and deliberately
ignores it on Linux, where writing under a client-side override would produce a file
`dockerd` never opens — a success report with a still-failing pull. `-dry-run` and
`-uninstall` behave as they do for `--persist`, and removal touches only this registry's
directory. The macOS location was verified against Docker Desktop 29.6.2 on darwin/arm64.

The same split applies to the generated `setup.sh`, which previously wrote the Linux
path on every platform: on macOS it installed the certificate where Docker Desktop does
not look, reported success, and left the pull failing.

`--persist` selects the Phase 2 script path. In that mode the client downloads the
platform-specific setup script over TLS verified by the pinned CA, checks that the
script embeds the same CA, and supports `-dry-run`, `-uninstall`, `-print`,
`-cache-ip`, and an authenticated `-cookie-file`. Because persistent tool
configuration cannot safely share one bearer token across every package client,
`-token-file` is temporary-mode only. Plain HTTP is rejected in both modes.

`make client-release` produces the five target binaries and a SHA-256 checksum file.
Publishing them is `pkgreg publish-client`, a subcommand of the server rather than a
Makefile target, because the host that serves the downloads generally has only the
binary — no Go toolchain, no checkout, no `make`. It copies each file into
`<data-dir>/downloads` atomically, regenerates the digests from what the directory then
holds, and verifies any `pkgreg-client-SHA256SUMS` that travelled with the release,
which is what catches a damaged transfer onto an air-gapped host. Publishing is what
makes the files appear in the tutorial and Console → Connect; a binary left in `go/bin`
is built but not published.

```sh
pkgreg publish-client -data-dir /var/lib/pkgreg /media/transfer/pkgreg-release
sudo pkgreg publish-client -data-dir /var/lib/pkgreg bin   # root-owned data dir
make client-publish DATA_DIR=/var/lib/pkgreg               # build then publish, in a checkout
```

`internal/clientrelease` owns the filename grammar, the platform set and the checksum
format. The download API, `pkgreg doctor` and this command all read it from there,
because the same three facts previously lived in three places that had to be edited
together.
The `client-v*` release workflow:

- builds and attests static Linux amd64/arm64 binaries;
- Developer-ID signs and notarizes macOS amd64/arm64 zip artifacts;
- Authenticode signs and timestamps the Windows amd64 executable;
- generates release checksums and publishes only after all platform jobs pass.

Cross-compilation and checksum generation passed locally. Native signing was not
claimed locally: the workflow requires the Apple and Windows signing secrets and
their hosted runners, and fails if they are missing.

## Testing

The renderer rejects unsafe project/host values and non-CA input. Tests verify that
the default client session uses an ephemeral loopback bridge, removes inherited
persistent CA variables from the child environment, and restores the parent on exit.
Persistent-mode tests parse the
generated shell with `bash -n`, execute its non-mutating `--dry-run`, verify the
PowerShell state/restore contract, exercise v1 and compatibility download routes,
check project authorization, reject a hostile `Host` header, and verify the
out-of-band fingerprint header. `internal/clientinstaller` separately covers
fingerprint mismatch, embedded-CA mismatch, authentication, size limits, platform
command selection, printing, execution, and cleanup.

The opt-in privileged OS acceptance suite mints an ephemeral CA/server certificate,
proves curl rejects it, installs twice, proves system trust plus name resolution,
uninstalls, and proves trust is removed. Ubuntu 24.04, Alpine 3.22, Red Hat UBI 9.6,
and current Arch passed locally. The Arch run found and fixed a real detection issue:
Arch now also ships `update-ca-trust`, but reads
`/etc/ca-certificates/trust-source/anchors` rather than RHEL's `/etc/pki` tree.

GitHub Actions repeats the four pinned Linux distro runs and executes the native
privileged suite on Ubuntu 24.04, macOS 14, and Windows 2025. macOS/Windows results
must come from those native runners.

## Open decisions

1. `pkgcache.internal` vs. a subdomain of an owned domain. The second one obsoletes
   phases 1–2 entirely — no CA to distribute, nothing to install, Docker included.
2. Whether each deployment should use explicit per-build mappings or enable the
   optional dnsmasq resolver for all builders.
3. Whether this replaces or complements the README's Route 1. They are complementary:
   Route 1 (no certs, per-client flags) for images and CI; this path for developer
   workstations where verification should stay on.

## What already exists to build on

- [`urls.endpoints()`](../webui/app/urls.py) — per-project, per-ecosystem client
  endpoints as data, already rendered by the console.
- [`views/connect.js`](../go/internal/web/dist/console/views/connect.js) — where the
  download link and fingerprint are rendered.
- [`scripts/gen-certs.sh`](../scripts/gen-certs.sh) — SAN list is already a parameter.
- [`examples/porting/`](../examples/porting/) — the in-image half, which the installer
  does not replace.
