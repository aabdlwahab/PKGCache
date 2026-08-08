# pkgreg Adoption and First-Use Friction Audit

Audit date: 2026-08-01  
Audited build: `pkgreg 3b39fbb-dirty`, Linux amd64, Go 1.26.3  
Audited scope: the complete `go/` tree plus the client release workflow it depends on  
Source size reviewed: approximately 53,000 lines excluding generated binaries and prior audit artifacts

## Executive verdict

pkgreg has a substantial and unusually cohesive implementation behind it. The cache engine, catalog, ecosystem routing, embedded console, temporary client bridge, snapshots, air-gap packs, peer fetches, control plane, tests, and operational commands are all real. The happy path for an unprotected project on a trusted Linux network works. The full unit suite and race suite pass, and live acceptance covered Docker, APK, APT, npm, uv/pip, uv sync, and generic downloads.

It is not ready for an unguarded public beta or a production trial where an evaluator is expected to follow the default setup without a security review. The default listener exposes the control plane over both TLS and cleartext, control-plane authentication is disabled, and the apt forward proxy can relay to any host. A live unauthenticated `POST /api/v1/projects` against a freshly initialized instance returned `201 Created`. The generated configuration and `systemd install` path preserve this posture, while `doctor` still reports the instance as healthy.

The second major adoption blocker is that the security story is not end-to-end. A token-gated project works for bridge-routed HTTP clients, but the generated apt proxy path cannot attach a data-plane bearer token, Docker trust intentionally stores no token, and persistent setup rejects token files. The tutorial currently presents `-token-file` as the protected-project answer without exposing those exceptions.

The third cluster is distribution and bootstrap. A fresh instance has no downloadable client until an operator separately publishes five cross-compiled clients. The signed macOS release workflow produces notarized `.zip` files that `pkgreg publish-client` rejects, while the locally publishable raw macOS files are not signed. There is also no equivalent server release workflow. This makes the advertised signed-client, one-binary experience difficult to reproduce from the actual artifacts.

Recommended launch posture today:

- Suitable for a guided internal preview on a private, explicitly allowlisted network.
- Suitable for technical evaluation when the operator builds and publishes clients first and uses public projects.
- Not suitable for exposure on an untrusted network with defaults.
- Not suitable yet as a protected multi-ecosystem registry promise, especially when apt, Docker, persistent CI hosts, or macOS client distribution are in scope.

### Readiness scorecard

| Area | Score | Reason |
| --- | ---: | --- |
| Core cache behavior | 4/5 | Broad implementation and passing tests; public-project acceptance is strong. |
| First-run path | 2/5 | Requires a Go build and a separate client publication step; browser trust is circular. |
| Secure defaults | 1/5 | Unauthenticated control plane, cleartext admin access, and unrestricted proxy by default. |
| Protected-project UX | 1/5 | Bearer-token model does not reach apt, Docker, or persistent setup. |
| Distribution | 2/5 | Client signing exists, but published artifact shapes conflict; no server release automation. |
| Documentation accuracy | 2.5/5 | Detailed tutorial, but critical exceptions and supported-operation boundaries are buried or absent. |
| Console UX | 4/5 | Responsive, accessible, and operationally useful; a few first-load and positioning issues remain. |
| Production operations | 3/5 | Health, metrics, audit, maintenance, snapshots, and systemd exist; HA, backup, identity, and lifecycle guidance are thin. |

## Severity model

- **P0 - Blocker:** Can expose the evaluator or invalidate the product's security boundary through the documented/default path.
- **P1 - High:** Breaks a central adoption journey, makes the secure configuration unusable for a supported ecosystem, or creates a major trust/distribution contradiction.
- **P2 - Medium:** Causes likely trial abandonment, support load, misleading expectations, or significant operational work.
- **P3 - Low:** Polish, discoverability, completeness, and later-scale concerns that still affect confidence.

## What was examined

The audit covered:

- All three commands: `pkgreg`, `pkgreg-client`, and `pkgreg-bridge`.
- Configuration precedence, defaults, validation, generated YAML, PKI, storage initialization, and systemd installation.
- The unified listener, explicit listeners, TLS/plaintext splitting, admin API, auth guard, cookies, CSRF/origin handling, health, readiness, metrics, and event delivery.
- Catalog, blob storage, upstream pool, streaming and deduplication engine, jobs, maintenance, migration, snapshots, packs, lock warming, and peer fetches.
- Files, Git, npm, OCI, PyPI, and apt/apk adapters and their setup descriptors.
- Client release collection/publication, trust bootstrap, temporary bridge, persistent setup, Docker trust, shell setup, and PowerShell setup.
- Landing page, tutorial, console, responsive CSS, current accessibility affordances, destructive actions, pagination, and error/loading behavior.
- All Go tests, race tests, vet, public-project OS acceptance tests, binary checksums, binary linkage, a fresh init/doctor/publish cycle, a live client shell, live TLS/plain HTTP requests, and desktop/mobile screenshots.

The `go/` implementation and the release workflow were untracked in the parent worktree at audit time. Findings therefore describe the exact working tree above, not necessarily a published release.

## Verified strengths

These are adoption assets worth preserving while fixing the blockers:

1. The default temporary `pkgreg-client` flow is genuinely low impact. It pins the CA fingerprint, downloads the CA, starts an ephemeral loopback bridge, configures a child shell, and removes the session state on exit. A live shell opened and exited successfully.
2. The server is self-contained. The UI is embedded, the SQLite driver is pure Go, and normal builds can be static with no external database or web runtime.
3. The core caching engine has careful streaming, digest verification, atomic publication, request coalescing, conditional metadata revalidation, peer fallback, and access/stat tracking.
4. Air-gap primitives are first-class rather than an afterthought: checkpoints, rollback, manifest packs, import/export, and lock warming are implemented.
5. The control plane includes project ownership, scoped and expiring tokens, rate limits, encrypted upstream credentials, audit events, jobs, and destructive-action confirmations.
6. The current console fixes several issues found in an older UI critique. It now coalesces live refreshes, differentiates loaded and empty state, paginates inventory, provides chart table alternatives and accessible names, moves focus on navigation, includes a skip link, handles reduced motion, and contains wide tables and commands on mobile.
7. Desktop and 390px-wide landing/tutorial screenshots rendered cleanly with no visible overlap or horizontal page overflow.
8. Public-project client acceptance passed for Docker, APK, APT, npm, uv/pip install, uv sync, and `wget` in this environment.
9. `go test -count=1 ./...`, `go test -count=1 -race ./...`, and `go vet ./...` all passed.

## P0 findings

### F-001: The default deployment exposes an unauthenticated control plane, cleartext admin surface, and unrestricted forward proxy

**Evidence**

- `internal/config/sources.go:26-75` binds `:8443` on all interfaces, enables single-port mode, leaves auth unset, and leaves the proxy allowlist empty.
- `internal/config/types.go:89-92` explicitly states that an empty proxy allowlist relays anywhere and is only appropriate on a trusted network.
- `cmd/pkgreg/init.go:149-196` reproduces those choices in the starter configuration. Authentication is commented out and `proxy_allowlist` is not shown at all.
- `internal/control/auth/guard.go:45-119` bypasses every read, operate, create, and superuser check when accounts are disabled.
- `internal/app/listeners.go:74-105` splits TLS and plaintext on the same unified address. Plain origin-form requests are served by the same admin/data namespace rather than redirected.
- `internal/app/admin.go:12-21` mounts metrics, health, version, the API, and the console without an outer authentication boundary.
- `internal/control/api/v1.go:814-864` correctly avoids `Secure` on a cookie issued over HTTP, but that also means a user who signs in through the cleartext side sends a reusable session over cleartext.
- Live reproduction on a fresh instance: a plain unauthenticated `POST /api/v1/projects` with `{"name":"unauthenticated-audit"}` returned `201 Created` and created the project.

**Why this hurts adoption**

Following the documented `init` and `serve` path can expose project administration, user administration, credentials, maintenance actions, audit data, cache operations, and an outbound relay to the local network. The likely evaluator is precisely the person least likely to know which defaults require network containment. Security reviewers will stop a trial at this point, even if the intended deployment is internal.

The cleartext side is especially surprising because the same port is presented as an HTTPS origin. It is not limited to apt absolute-form proxy requests. An origin-form browser or API request over HTTP reaches the product surface.

**Required remediation**

1. Make the first-run default safe without assuming a perimeter. Good options are loopback-only binding until explicitly configured, mandatory control auth at init, or a generated random bootstrap credential printed once.
2. Route plain origin-form HTTP to HTTPS or reject it. Only accept absolute-form HTTP proxy requests on the plaintext branch.
3. Require a non-empty apt proxy allowlist, or disable the forward proxy until one is configured.
4. Make `doctor` fail, not warn, when a non-loopback listener combines disabled auth, cleartext admin access, or an empty proxy allowlist.
5. Show the effective security posture in init output and the console.
6. Add an automated test that a default non-loopback instance cannot mutate the control plane anonymously, cannot log in over plaintext, and cannot proxy to an unlisted host.

**Acceptance criterion**

A user can follow the shortest documented installation path on a machine reachable by other hosts without creating an unauthenticated administrative API or general-purpose HTTP relay.

## P1 findings

### F-002: Token-gated projects are not supported end-to-end by apt, Docker, or persistent setup

**Evidence**

- `internal/clientbridge/bridge.go:229-241` attaches the project bearer token to requests that pass through the local bridge.
- `internal/clientinstaller/client.go:93-105` gives the bridge a remote apt proxy URL rather than routing apt through the bridge.
- `internal/clientinstaller/client.go:280-305` encodes only the project name as the proxy username.
- `internal/router/project.go:107-148` says the proxy password is ignored and is not a credential.
- `internal/app/dataplane.go:235-257,284-290` accepts only a bearer `Authorization` header or `X-Auth-Token` for data-plane auth.
- `internal/clientinstaller/dockertrust.go:51-54` rejects `-token-file` because Docker trust stores no token.
- `internal/clientinstaller/client.go:111-113` rejects `-token-file` in persistent mode.
- The tutorial says to add `-token-file` for a protected project (`internal/web/dist/tutorial.html:195-201,718-726`) without stating that apt, Docker, and persistent setup remain unable to use it.

**Impact**

The recommended way to protect package data breaks three prominent adoption cases:

- apt/apk receives `401` because no bearer token can be transported through the generated proxy configuration.
- Docker receives a bearer challenge but has no pkgreg token service, credential helper, or documented `docker login` mapping that can satisfy it.
- CI runners and shared hosts cannot persist project credentials through the official client.

This is not merely missing documentation. It is a protocol and installer capability gap.

**Required remediation**

- Define one credential transport for the forward proxy, such as a proxy password mapped to the project token, and preserve the project label without ambiguity.
- Implement an OCI/Docker auth flow that Docker understands, or ship a credential helper and exact login workflow.
- Support persistent credentials through native permissioned tool stores, an external secret command, or a deliberately managed token file with rotation/uninstall behavior.
- Generate ecosystem-specific onboarding from project policy. Do not show a command as usable when that ecosystem cannot authenticate.
- Add real-client acceptance for a token-gated project for every advertised ecosystem and every onboarding mode.

### F-003: The signed macOS release artifacts cannot be published by the server

**Evidence**

- `internal/clientrelease/clientrelease.go:33-83` accepts only raw names such as `pkgreg-client-darwin-arm64`.
- `internal/clientrelease/clientrelease.go:175-210` ignores nonmatching files in a directory and rejects a nonmatching explicit file.
- `.github/workflows/client-release.yml:92-112` signs the raw macOS binary, packages it as `.zip`, notarizes the zip, and deletes the raw file.
- `.github/workflows/client-release.yml:179-183` publishes only `pkgreg-client-darwin-*.zip` for macOS.
- `Makefile:36-48` produces raw publishable macOS files, but does not sign or notarize them.

**Impact**

An operator must choose between the artifact shape pkgreg can publish and the artifact Apple users can trust without warnings. The README's claim that the signed client is the normal path is therefore not reproducible from the signed release output.

**Required remediation**

- Choose one canonical distributable format per platform and make the workflow, checksum file, publisher, download API, and tutorial agree.
- If the server should serve zip files, add safe extraction instructions and verify the signature/notarization metadata before publication.
- If the server should serve raw binaries, keep the signed/notarized raw binary as a workflow artifact and publish it with matching checksums/attestations.
- Test `pkgreg publish-client` directly against downloaded release artifacts in CI.

### F-004: A clean first run has a deliberate client-download dead end

**Evidence**

- `README.md:68-109` requires `make client-publish` and explicitly notes that the tutorial has nothing to offer until clients are published.
- `cmd/pkgreg/init.go:105-118` warns the operator to publish clients after initialization.
- `cmd/pkgreg/doctor.go:249-277` reports missing clients only as a warning.
- `go/.gitignore` excludes `bin/`, so a clean source checkout does not contain release clients.
- `Makefile:36-48` needs a Go cross-build for all five client platforms before the first developer can download one.

**Impact**

The product's main onboarding page depends on a second operator-only packaging workflow that is easy to omit. A server can be healthy and serving packages while every new developer sees an empty download panel. This is a high-cost failure because it occurs at the first promised interaction.

**Required remediation**

- Publish official client artifacts with every server release and give `pkgreg init` or `systemd install` an authenticated, checksum-verified way to install them.
- Alternatively ship matching client payloads with the server artifact if size and release policy permit it.
- Treat a missing client set as unhealthy for an installation that exposes `/tutorial`, or replace the tutorial with an operator-facing setup state until it is complete.
- Add a clean-machine end-to-end test that starts with only the server release artifact.

### F-005: `doctor` can mutate an empty data directory and still call a nonfunctional host healthy

**Evidence**

- `cmd/pkgreg/doctor.go:38-60` loads and evaluates the target instance.
- `cmd/pkgreg/doctor.go:98-130` calls `EnsureDirs` and `app.Open`, creating directories, SQLite databases, and the host credential key.
- Missing TLS and missing clients are warning paths (`cmd/pkgreg/doctor.go:153-158,249-277`).
- Live reproduction before `init`: `doctor` created `catalog.db`, `control.db`, `host.key`, and the directory layout, then exited zero with `healthy, with 2 warning(s)` even though `pkgreg-client` requires HTTPS and `/tutorial` had no download.

**Impact**

Operators expect a diagnostic to be observational and its exit status to be usable in automation. Here it changes the system and provides a green result for a state that cannot complete the primary onboarding journey. It also masks accidental use of the wrong data directory by populating it.

**Required remediation**

- Add a read-only doctor mode and make it the default.
- Separate `ready to serve cache traffic` from `ready to onboard a developer` and `secure for non-loopback exposure`.
- Return distinct nonzero statuses, or a machine-readable JSON result, for blocking readiness classes.
- Check listener bindability, public hostname/SAN coverage, auth posture, cleartext exposure, proxy allowlist, client completeness, available disk, filesystem type, upstream reachability, DNS, and time skew.

### F-006: Browser onboarding asks the user to trust the private CA before providing a trusted way to obtain it

**Evidence**

- `init` tells the operator to open an HTTPS tutorial using a newly minted private CA.
- A new browser has no reason to trust that CA, so the first visible interaction is a certificate warning.
- Single-port mode also serves the page over HTTP, but the documentation does not present that as a bootstrap mode and downloading executable/checksum material there is unauthenticated.
- The tutorial gets the binary digest and CA fingerprint from the same instance the user is trying to authenticate.
- `pkgreg-client` itself has a sound out-of-band fingerprint pinning implementation (`internal/clientinstaller/client.go:238-276,478-487`), but the evaluator still needs an independently trusted fingerprint and executable.

**Impact**

The user is asked to click through a warning or transfer trust material out of band before they have experienced product value. Security-conscious users will stop; less cautious users are trained to bypass the exact warning the product's trust design is meant to avoid.

**Required remediation**

- Provide an explicit out-of-band bootstrap channel: signed official client download plus an operator-distributed CA fingerprint.
- Print a short, copyable fingerprint and client command at installation, with a documented secure way to distribute it.
- Do not imply that a checksum fetched beside the binary establishes publisher authenticity.
- Consider a one-time bootstrap page bound only to loopback or protected by a generated one-time secret.

### F-007: The one-command systemd path installs an incomplete and insecure evaluation by default

**Evidence**

- `cmd/pkgreg/systemd.go:22-27` calls this the clean-host one-command installation path.
- Its unit runs `init` and `serve` with the generated default configuration (`cmd/pkgreg/systemd.go:90-127`).
- It does not publish client artifacts, enable auth, or populate a proxy allowlist.
- It writes to `/usr/local/bin` and `/etc/systemd/system`, invokes `systemctl`, and normally requires root, but does not perform an upfront privilege/preflight check.
- It only supports a safe directory directly under `/var/lib` (`cmd/pkgreg/systemd.go:44-54`).

**Impact**

The command succeeds at service installation but does not deliver the end-to-end outcome implied by its description. The new service is reachable with the P0 posture, while the tutorial remains unable to distribute clients. Failures can also occur after partially installing the binary or unit.

**Required remediation**

- Make `systemd install` transactional or explicitly resumable, with an upfront root, systemd, port, hostname, and path preflight.
- Require or generate secure auth and proxy settings.
- Fetch/publish verified client releases or print a hard failure that identifies the missing step.
- Add `systemd status`, `upgrade`, and `uninstall` paths, including clear data-retention behavior.

### F-008: Authenticated persistent onboarding depends on manually extracting a raw browser cookie

**Evidence**

- Setup script endpoints require project view permission when control auth is enabled (`internal/control/api/setup.go:34-38`).
- `pkgreg-client` accepts only `-cookie-file`, described as one raw Cookie header (`cmd/pkgreg-client/main.go:36-39`).
- `internal/clientinstaller/client.go:490-502` validates that opaque raw value.
- The public tutorial's persistent commands do not include an authenticated acquisition flow.

**Impact**

The production-recommended auth posture makes unattended setup and CI bootstrap awkward. Users must learn browser developer tools or construct session-cookie files, and those in-memory sessions are not an appropriate long-lived machine credential. This creates pressure to disable control auth for convenience.

**Required remediation**

- Add a supported CLI login/device/bootstrap-token flow with scoped, short-lived setup authorization.
- Let the console generate a one-time command or enrollment token for persistent clients.
- Document noninteractive CI enrollment, expiration, revocation, and audit behavior.

### F-009: Direct initialization can leave a credential-bearing configuration world-readable

**Evidence**

- `internal/config/sources.go:408-433` creates a new data-directory tree with mode `0755`.
- `cmd/pkgreg/init.go:191-204` suggests putting `root_password` in the generated YAML and writes that YAML with mode `0644`.
- The systemd unit's `UMask=0027` reduces this exposure in that one path, but direct `pkgreg init` does not.

**Impact**

An evaluator following the direct path can uncomment the example credential and expose it to other local users. Upstream secrets live encrypted in the control database, so the config file becomes the weaker credential surface.

**Required remediation**

- Create the data directory as `0700` or an explicitly owned service directory.
- Write credential-bearing configuration as `0600`.
- Prefer `PKGREG_ROOT_PASSWORD`, a hashed password bootstrap, a secret file reference, or a one-time generated credential over a plaintext YAML password.
- Have `doctor` fail on unsafe ownership or modes.

### F-010: Product language overstates ecosystem breadth and operation coverage

**Evidence**

- The landing and README use broad language around one cache for every build environment/ecosystem.
- The implementation supports six adapter families: files, Git, npm, OCI, PyPI, and apt/apk.
- Git and OCI are read-only; apt/apk is HTTP-only; only generic files has an upload flow.
- Maven, Gradle, NuGet, Cargo, RubyGems, Go modules, Composer, Helm as a distinct protocol, and other common enterprise package flows are absent from the audited tree.

**Impact**

Evaluators bring an unsupported ecosystem or attempt publish/push behavior before learning the actual boundary. That turns a positioning problem into a product failure and makes the substantial supported surface look less trustworthy.

**Required remediation**

- Put a concise capability matrix before the first install command.
- Use literal language such as `cache pulls for six ecosystem families` and name write/read-only boundaries.
- Separate `supported now`, `possible via generic files`, and `not supported`.

### F-011: External adopters have no visible license or security-reporting contract

No project-level `LICENSE`, `NOTICE`, or `SECURITY.md` was found at the repository root or in `go/`. The Go dependency graph is pinned, but the audited release surface also has no SBOM or third-party notice artifact.

**Impact**

An external evaluator has no grant of rights to use, modify, or redistribute the software and no stated channel or disclosure policy for vulnerabilities. Legal, procurement, and security teams can block adoption before technical evaluation. If pkgreg is intended to remain proprietary, the same gap exists in a different form: the commercial evaluation terms are not part of the package.

**Required remediation**

- Publish the intended open-source or commercial license and required notices.
- Add a security policy with supported versions, reporting channel, response expectations, and disclosure rules.
- Attach a machine-readable SBOM, checksums, signatures/attestations, and third-party license inventory to releases.

### F-012: Default storage and traffic policies are unbounded

**Evidence**

- `internal/config/types.go:164-171` documents that zero disables the size target and TTL; the default minimum-free floor is also zero.
- `cmd/pkgreg/init.go:185-189` emits all three eviction policy values as zero.
- Project byte quota, artifact quota, and request rate also default to unlimited (`internal/config/types.go:192-207`).
- The scheduled evictor only runs when at least one policy is active (`internal/maintenance/service.go:329-334`).
- `/readyz` verifies that a temporary blob can be created but has no configured or emergency free-space floor (`internal/app/admin.go:31-73`).

**Impact**

A successful trial can fill the host filesystem indefinitely. Because the catalog, control database, CA, and blobs normally share the data filesystem, package growth can take down both package traffic and administration. Multi-tenant projects also have no default fairness boundary, and anonymous traffic has no default request limit.

**Required remediation**

- Make initialization ask for a cache budget or generate a conservative free-space floor.
- Require an explicit `unbounded: true` acknowledgement when all eviction bounds are disabled.
- Make readiness fail before the filesystem reaches the point where SQLite or blob commits fail.
- Have `doctor` report the effective capacity, current growth, projected exhaustion, and every unlimited project/rate policy.
- Provide human-sized configuration and CLI inputs such as `100GiB` and `20GiB`.

## P2 findings

### F-013: Subcommand help exits as an error and prints an error after valid help

`pkgreg serve -h` and `pkgreg init -h` print help, then print `pkgreg: flag: help requested`, and exit 1. `cmd/pkgreg/main.go:64-67` treats `flag.ErrHelp` like any other command error. `pkgreg-bridge -h` prints help and exits 2 because `internal/clientbridge/bridge.go:101-103` also lacks an `ErrHelp` branch. `pkgreg-client -h` already implements the correct behavior at `cmd/pkgreg-client/main.go:76-80` and can be used as the shared pattern.

Why it matters: shell probes, documentation tooling, packaging checks, and first-time users interpret ordinary help as failure.

Also align `pkgreg version` with the conventional `pkgreg --version`; the latter currently returns unknown command and exit 2.

### F-014: The tutorial is not actually complete without JavaScript

`internal/web/dist/tutorial.js:1-3` claims the page is complete without JavaScript, but client downloads, checksums, and the CA fingerprint command are populated dynamically. The HTML fallback retains `PASTE_FINGERPRINT`, and copy controls are script-driven (`internal/web/dist/tutorial.html:120-201`).

Why it matters: locked-down, script-blocked, partially loaded, or air-gapped browsers see a polished page whose central command cannot succeed.

Recommendation: server-render coordinates, fingerprint, and published downloads into the embedded page, or provide a complete noscript block with CLI commands and explicit links.

### F-015: Coordinate discovery permanently gives up after 1.2 seconds

`internal/web/dist/coords.js:156-190` resolves a shared promise to `null` after a fixed 1.2 second budget. A later successful fetch cannot replace the resolved fallback. Location-based reconstruction also assumes the current listener layout, which can be wrong with explicit admin/proxy ports.

Why it matters: a busy first start, slow storage, browser scheduling pause, or reverse-proxy delay can leave the tutorial with a placeholder fingerprint for the whole page session.

Recommendation: keep the late response, show an explicit retry/error state, and render coordinates on the server when possible.

### F-016: apt/apk support excludes the normal HTTPS proxy path

`internal/app/dataplane.go:205-211` rejects `CONNECT` and tells the client to configure an HTTP repository. `internal/eco/apt/apt.go:1-6` describes the same limitation.

Why it matters: many modern Debian and Alpine repositories are HTTPS-only or redirect to HTTPS. Teams must downgrade the configured origin URL or cannot use this adapter, and the limitation is easy to miss until package-manager failure.

Recommendation: state `HTTP repositories only` in every capability matrix and setup block. Longer term, either support controlled CONNECT/MITM with a defensible trust model or fetch HTTPS origins through a non-CONNECT package-manager integration.

### F-017: Docker is technically supported but has the highest onboarding cognitive load

The tutorial devotes roughly 300 lines to temporary Linux daemon behavior, BuildKit networking, Docker Desktop, remote builders, Dockerfiles, multistage builds, daemon trust, persistent setup, and failure cases. This detail is useful, but it proves that `open one shell` is not the Docker experience.

Why it matters: Docker is a headline ecosystem. First testers often start with `docker pull`, while the correct command varies by daemon location and networking mode. Protected projects add the unresolved token issue from F-002.

Recommendation: give Docker a short decision tree generated from OS/mode, a single copyable command for each supported case, and an automated `pkgreg-client -check docker` that tests DNS, CA trust, registry reachability, auth, and one small manifest request.

### F-018: Git and OCI operation boundaries are discoverable too late

- Git is explicitly read-only, smart HTTP only, with dumb HTTP, push, and LFS upload rejected (`internal/eco/git/git.go:52-60,274-276,362-368,490-498`).
- OCI implements pull-oriented routes and no push workflow.

Why it matters: developers naturally test `git push`, image push, CI publication, or a tool using an older Git transport. The landing-level promise does not prepare them for a caching-only surface.

Recommendation: label adapters `pull/cache only` in navigation, console cards, and docs. Return errors that link to the supported operation matrix.

### F-019: Server delivery is still source-first and Linux-only

`Makefile:24-31` builds the host binary and Linux amd64/arm64 release binaries. There is no server release workflow parallel to the client workflow, no package repository, and no installer that fetches a verified server artifact. The declared module requires Go 1.25, which a clean evaluator must already have if no binary release is provided.

Why it matters: `one static binary` describes runtime topology, not acquisition. The highest-friction step currently happens before the user can see the product.

Recommendation: publish versioned server binaries, checksums, signatures/attestations, an SBOM, supported OS/architecture table, and a minimal verified install/upgrade path.

### F-020: Single-host storage assumptions constrain enterprise trials

The catalog and control plane use local SQLite, blobs rely on atomic filesystem operations and hardlinks, and `internal/blob/store.go:44` notes that relevant assumptions do not hold on NFS. Peers fetch digest-addressed content but do not provide replicated control state, leader election, or failover.

Why it matters: common production evaluations ask for Kubernetes, object storage, network volumes, active-active service, or a documented standby. The current architecture is a strong single-node appliance, but the limits are not positioned early.

Recommendation: publish a deployment envelope: supported filesystems, local-disk requirements, backup consistency rules, maximum tested catalog size, vertical scaling guidance, and explicit HA/non-HA behavior.

### F-021: Backup, restore, and disaster-recovery guidance is missing from the main journey

Snapshots and transfer packs preserve project content state, but they are not a clearly documented whole-instance backup for the control database, users, upstream credentials, host key, CA private key, client publications, and audit history.

Why it matters: operators cannot decide what must be backed up together or test restore time/objectives. Copying SQLite and blob files independently while the service runs can create an inconsistent recovery set.

Recommendation: define supported online/offline backup procedures, consistency boundaries, restore validation, secret/key handling, and upgrade rollback.

### F-022: CA and host-key lifecycle is initialization-oriented rather than operational

`init` mints a long-lived private CA and server certificate; `doctor` checks expiry. The audited operator surface does not provide a clear rotate CA, rotate leaf, overlap trust, revoke compromised CA, or rotate the credential-sealing host key workflow.

Why it matters: security reviews will ask how a ten-year deployment rotates trust without breaking every persistent host and Docker daemon.

Recommendation: document the lifecycle now and add staged rotation commands before production adoption.

### F-023: Sessions are process-memory state

`internal/control/auth/sessions.go` stores console sessions and login-failure counters in in-memory maps. A restart invalidates every login and clears rate-limit history; multiple server processes would not share sessions.

Why it matters: restarts during evaluation look like unexplained sign-outs, and the design blocks horizontal admin-plane scaling.

Recommendation: document restart behavior. For production/HA ambitions, persist signed sessions or shared session state and retain bounded login-throttle state.

### F-024: Enterprise identity integration is absent

The audited tree implements local username/password accounts and roles. No OIDC, OAuth, SAML, LDAP, external reverse-proxy identity contract, or mTLS user identity was found.

Why it matters: many organizations will not approve a separate local password database for an administrative service, especially one storing upstream credentials.

Recommendation: state local-auth scope honestly and define an identity roadmap or a supported, security-reviewed proxy-auth contract.

### F-025: Operational endpoints may disclose more than expected

`/metrics`, `/healthz`, `/readyz`, and `/version` are mounted outside the control-plane authorization guard (`internal/app/admin.go:12-21`). Health being public is common, but metrics and detailed readiness can reveal ecosystem usage, project labels, versions, storage state, and failure modes depending on emitted labels.

Why it matters: the default all-interface listener makes this an exposure rather than a cluster-local convention.

Recommendation: split liveness from operator diagnostics, protect metrics/detailed readiness or bind them separately, and review all metric labels for project/package cardinality and disclosure.

### F-026: The generated YAML omits a critical proxy security setting

`proxy_allowlist` exists and its empty behavior is security-sensitive (`internal/config/types.go:89-92`), but `cmd/pkgreg/init.go:143-197` does not include it in the starter configuration.

Why it matters: even an operator reading every generated comment cannot discover the setting that prevents open relay/SSRF behavior.

Recommendation: include it prominently, disable proxying when it is absent, and generate safe starter entries only from explicit operator choices.

### F-027: YAML path emission is fragile for unusual but legal paths

`cmd/pkgreg/init.go:143-203` writes `data_dir` and TLS paths without YAML quoting. Paths containing comment-sensitive or scalar-sensitive characters can produce a malformed or changed configuration.

Why it matters: uncommon service paths, mounted volumes, or directories containing spaces and `#` can turn initialization into a confusing parse error.

Recommendation: construct a typed config value and serialize it through `yaml.v3` instead of formatting YAML text.

### F-028: The default `/` route prioritizes a marketing experience over the evaluator's next task

The landing page is visually polished and responsive, but it is a long, animated product narrative. The first actionable evaluator surfaces are `/tutorial` and `/console/`.

Why it matters: on a self-hosted operational appliance, the root URL is commonly used as the post-install check. Operators want status, setup completion, and the next command before a product pitch.

Recommendation: after initialization, make `/` an operator-aware start page or redirect authenticated users to the console. Keep the landing experience for a public demo mode if desired.

### F-029: The intended font is referenced but not shipped

`internal/web/dist/tokens.css:8-20` requests `/fonts/IBMPlexMono.woff2`; `internal/web/dist/fonts/README.md` confirms that the file must be manually added. The UI falls back correctly, so this is not a functional bug.

Why it matters: screenshots, alignment, perceived polish, and text density vary across operating systems, including the air-gapped case the design emphasizes.

Recommendation: either ship the licensed font in release assets or design/test against the actual system-font stack and remove the missing network request.

### F-030: A control-plane outage is presented as a sign-in screen

`internal/web/dist/console/boot.js:27-34` renders the login screen for every `/me` failure. Only 401 is the normal auth case; a 500, malformed response, proxy error, or transient network failure gets the same form plus a message.

Why it matters: users retry credentials against an unavailable service and may interpret an infrastructure problem as an authentication failure.

Recommendation: render a distinct connection/startup failure state with retry, endpoint/version details, and no credential fields.

### F-031: The server accepts both HTTPS and HTTP without an HSTS/redirect story

The TLS listener and plaintext origin listener coexist on one address. Even after F-001 adds auth, users can bookmark or follow an HTTP URL, and an HTTP login results in a non-Secure cookie by design.

Recommendation: reject or redirect origin-form HTTP, set HSTS on a confirmed HTTPS deployment, and make proxy traffic structurally distinguishable from admin traffic.

### F-032: CLI ergonomics are optimized for implementation, not packaging/operator conventions

Observed friction includes:

- No top-level `--version` alias.
- Nonzero help for most commands.
- No shell completions or man pages.
- No `status` or `config show` command that prints effective configuration and listener/security posture.
- Byte-size inputs are generally raw integers rather than human units.
- `audit` has a limit but little filtering/export surface for actor, action, project, or time.
- Air-gap commands use managed shuttle directories and basenames, which is safe but needs stronger path/result discoverability.

Recommendation: prioritize `pkgreg status --json`, `pkgreg config show`, conventional help/version behavior, human byte parsing, and audit filters before adding more commands.

### F-033: The tutorial's integrity guidance can be mistaken for authenticity

Each download exposes a SHA-256, and publisher verification is careful. However, when the binary and checksum come from the same newly installed, privately trusted server, the checksum detects corruption but does not independently establish who built the executable.

Recommendation: surface platform signature/notarization state, release version/commit, attestation links or offline verification commands, and the distinction between transport integrity and publisher authenticity.

### F-034: The `publish-client` command also publishes bridge binaries but reports client completeness

The filename grammar accepts both `client` and `bridge`, and a live publication of `go/bin` copied both families. The success message still reports `All 5 client platforms`. This is not harmful, but the command name and report hide the second artifact family.

Recommendation: rename to `publish-tools`, or explicitly list separate client and bridge completeness. Ensure the tutorial/API filters remain intentional.

## P3 findings and watch items

### F-035: Real-client protected-mode acceptance is missing

The acceptance harness proved public project flows. No end-to-end run covered a token-gated project through pip/uv, npm, Git, apt, Docker, and files. This is why F-002 can coexist with a green acceptance suite.

Add a matrix over public/token projects, temporary/persistent mode, and supported OSes.

### F-036: Privileged, cross-platform, and maximum-size tests remain opt-in or environment-dependent

- Native `pip` acceptance skipped because the test host's Python lacked the pip module; uv-based pip behavior passed.
- Privileged onboarding OS acceptance requires `PKGREG_ONBOARDING_OS_ACCEPTANCE=1`.
- The 2 GiB streaming/load path is opt-in.
- macOS, Windows, Docker Desktop, remote BuildKit, and Windows ARM behavior were not executed locally.

These are reasonable CI separations, but release gates should run the relevant matrix on native runners.

### F-037: Lint is declared but unavailable in the audited environment

`go vet ./...` passed. `make lint` could not be run because `golangci-lint` was not installed. Pin the lint version in CI or a tool manifest so contributors and releases run the same analyzer set.

### F-038: The console has no explicit first-run setup state

The console assumes a project/control-plane view, while missing client publications, auth posture, proxy allowlist, and CA distribution are operator setup concerns. A compact setup-readiness page would turn several silent configuration gaps into guided work.

### F-039: The tutorial is comprehensive but difficult to scan for one narrow task

The page cleanly handles mobile and gives a three-step path, but Docker, persistent hosts, troubleshooting, and multiple platform variants make it long. A user coming only for npm or pip must still understand the shared shell before getting the exact success check.

Recommendation: preserve the full guide, but add OS/ecosystem selectors that generate the shortest valid path and hide inapplicable branches.

### F-040: First success is configuration-focused rather than outcome-focused

The flow proves that a shell was configured, but the user still chooses a package and interprets whether it was a cache miss or success. Offer a tiny, deterministic read-only smoke test per ecosystem and show the corresponding request/event in the console.

### F-041: Capability and compatibility contracts are distributed across source, tutorial, and errors

There is no single versioned document covering supported clients, methods, protocols, authentication modes, upstream credential kinds, OSes, filesystems, proxy behavior, and known incompatibilities.

Recommendation: maintain a compatibility matrix as a release artifact and link every adapter/card/error back to it.

### F-042: API consumers have no clearly published contract

The control API is substantial, but the audited `go/` tree does not include OpenAPI, generated client types, version lifecycle, deprecation policy, or examples for automation.

Why it matters: teams will script project/token/upstream creation during evaluation. Reverse-engineering browser requests increases brittle integrations and support load.

### F-043: Local account recovery and bootstrap rotation need a documented path

The root account can be configured and users can be managed in the console, but operators need explicit procedures for a forgotten root password, credential rotation, a lost config secret, and recovery when the console cannot be reached.

### F-044: Upgrade and schema compatibility are not part of the first production story

The code contains migrations and careful databases, but the operator-facing flow lacks a release upgrade checklist, downgrade limits, preflight, backup point, schema compatibility promise, and rollback procedure.

### F-045: A stale local `pkgreg-bridge` build can differ from release properties

The inspected release-target binaries were static, while the current host `bin/pkgreg-bridge` was dynamically linked and appeared to be a local/stale build. Because `bin/` is ignored, local artifacts can silently differ from release outputs.

Recommendation: put version/linkage metadata in every binary, clean or separate host and release output directories, and make publisher provenance visible.

## Ecosystem adoption matrix

| Ecosystem | What works now | Important boundary | First-test risk | Protected project status |
| --- | --- | --- | --- | --- |
| Generic files | Anonymous GET/HEAD and write-token upload | Write-once artifact-store semantics; not a general mutable file server | User must create/upload a known test object first | Bridge can attach bearer token; direct upload uses token |
| Git | Smart HTTP clone/fetch and LFS object reads | Read-only; no push, dumb HTTP, or LFS upload | Repository URL rewriting and external `git` dependency | Expected to work through bridge bearer path; needs real-client acceptance |
| npm | Metadata/tarball cache with upstream override/credentials | Cache/read path, not package publishing | Scoped-package URL and auth configuration | Expected through bridge; persistent token gap remains |
| PyPI | Simple index, artifacts, uv/pip workflows, PyTorch index aliases | Cache/read path, not package publishing | Native pip availability and index/trusted-host configuration | Expected through bridge; persistent token gap remains |
| OCI/Docker | Pull-oriented registry cache | No push; daemon is outside shell; builder/desktop networking varies | Highest setup complexity and CA trust burden | Not end-to-end: no Docker-compatible token flow |
| apt/apk | HTTP forward proxy with cached indexes/packages | No CONNECT; repository must use HTTP; proxy can relay anywhere if unallowlisted | Modern HTTPS repos fail; proxy config is host-wide in many environments | Not end-to-end: generated proxy carries project label, not bearer token |

## First-use journey analysis

### Journey A: Evaluator from a clean source checkout

1. Install Go 1.25+ and build `pkgreg`.
2. Run `init` into a writable nondefault directory or use root for `/var/lib/pkgreg`.
3. Cross-build five client binaries and publish them.
4. Run `doctor`, which can say healthy while treating missing onboarding assets as warnings.
5. Start the server on all interfaces with no auth and unrestricted proxy by default.
6. Open an HTTPS page backed by an untrusted private CA.
7. Transfer or trust an out-of-band fingerprint, download a client, chmod it, and open a shell.
8. Choose an ecosystem-specific smoke test and understand whether it succeeded.

Likely exits: Go/toolchain acquisition, omitted client publication, certificate warning, security review, or Docker/apt exception.

### Journey B: Operator using `systemd install`

1. Run a command that requires root and systemd.
2. The binary and unit are written before all runtime steps have succeeded.
3. The service initializes a private CA and starts with auth off.
4. No clients are published.
5. The printed console URL leads to a certificate warning and a tutorial without downloads.

Likely exits: permissions/partial install, empty download panel, or default-security discovery.

### Journey C: Developer on a public project

1. Obtain the correct signed client and an independently trusted CA fingerprint.
2. Run one command and enter a temporary shell.
3. pip/uv/npm/Git/files traffic uses the loopback bridge cleanly.
4. apt uses the remote forward proxy; Docker needs a separate decision path.

This is the strongest journey once the operator has completed distribution.

### Journey D: Developer on a token-gated project

1. Create and securely transfer a project token file.
2. Start the temporary bridge; common HTTP clients can use the bearer token.
3. apt bypasses the bridge and cannot present the token.
4. Docker cannot consume the token through Docker trust.
5. Persistent setup refuses the token entirely.

This journey currently contradicts the secure-project promise.

### Journey E: CI runner or shared build host

1. Needs persistent CA/tool configuration and often noninteractive control-plane access.
2. Authenticated setup requires a raw browser session cookie.
3. Project token cannot be persisted by the official client.
4. Docker and apt remain special cases.
5. Upgrade, token rotation, and uninstall automation are not fully productized.

Likely exit: bespoke scripting or control/data auth disabled for expediency.

### Journey F: Air-gapped operator

1. Core checkpoint, export, import, and lockwarm primitives are present.
2. Operator must separately move client releases, checksums/signatures, CA trust, control configuration, packs, and potentially upstream credentials.
3. Whole-instance backup/restore and compatibility across pkgreg versions are not yet a single documented runbook.

The primitives are strong; the adoption gap is orchestration and operational contract.

## Validation results

### Passed

- `go test -count=1 ./...`
- `go test -count=1 -race ./...`
- `go vet ./...`
- Public-project acceptance: Docker, APK, APT, npm, uv pip install, uv sync, and wget
- Published client/bridge checksum verification for the current `go/bin` release files
- Fresh `init`, `publish-client`, TLS and plaintext serving, control API, and temporary client-shell smoke test
- Desktop and 390px mobile rendering for landing and tutorial with Firefox headless

### Skipped or not available

- Native pip acceptance: the host Python did not have the pip module; uv paths passed.
- Privileged OS onboarding: opt-in environment flag not enabled.
- 2 GiB test: opt-in load flag not enabled.
- macOS/Windows native client execution, Docker Desktop, and remote builder paths.
- `golangci-lint`: command not installed.
- Console screenshot after asynchronous module/API boot: Firefox's one-shot screenshot completed before the module-driven view replaced `Loading control plane...`; source/API behavior was inspected instead, so this is not reported as a console hang.

### Important coverage gaps

- Token-gated real-client matrix.
- Default-deployment security invariants.
- Clean-host test using only published release artifacts.
- Release-artifact-to-`publish-client` compatibility.
- systemd install failure rollback and uninstall.
- Backup/restore and version upgrade/downgrade drills.
- Native OS trust-store behavior on every supported platform.

## Recommended adoption plan

### Gate 1: Make a trial safe

1. Close F-001 and F-026: safe binds, mandatory/bootstrapped auth, plaintext origin rejection, and proxy allowlisting.
2. Make `doctor` fail on unsafe or non-onboardable states and stop mutating by default.
3. Add regression tests for anonymous control mutation, HTTP login, and unrestricted proxying.

Exit criterion: a default evaluation can be placed on an ordinary corporate network without relying on undocumented perimeter controls.

### Gate 2: Make the downloadable path real

1. Resolve the signed macOS zip/raw artifact mismatch.
2. Publish signed/attested server and client artifacts together.
3. Let the clean-host installation obtain and publish verified clients.
4. Test the whole path from release download to developer shell.

Exit criterion: an operator starts with one official server artifact, and a developer obtains a platform-trusted client from the resulting instance without a source build.

### Gate 3: Make protected projects honest

1. Implement apt proxy token transport.
2. Implement Docker-compatible auth.
3. Support managed persistent credentials and noninteractive enrollment.
4. Run token-gated acceptance for every ecosystem/mode.

Exit criterion: enabling project token auth does not silently remove any ecosystem advertised for that project.

### Gate 4: Reduce trial abandonment

1. Publish a concise capability/deployment matrix.
2. Fix CLI help/version conventions and add `status`/effective-config output.
3. Make tutorial coordinates and noscript behavior reliable.
4. Add per-ecosystem deterministic smoke tests and a setup-readiness console view.

Exit criterion: a first evaluator can identify support, install, verify one cache hit, and diagnose a failure without reading source or a long exception guide.

### Gate 5: Prepare production review

1. Document backup/restore, upgrade/rollback, CA/key rotation, and supported filesystems.
2. Decide the identity/SSO and HA posture.
3. Protect or separate operational endpoints.
4. Run privileged, large-object, native OS, and failure-recovery release gates.

Exit criterion: security and platform teams can evaluate pkgreg from published contracts and reproducible tests rather than implementation inference.

## Suggested launch checklist

Until the P0/P1 items are fixed, a guided preview should require all of the following:

- Bind only to a private interface or firewall the host before starting it.
- Enable control-plane auth and set a strong root credential through a protected secret source.
- Set a strict `server.proxy_allowlist`, or disable access to the proxy port/path.
- Use only public data-plane projects for apt and Docker trials.
- Build and publish all client platforms before sharing `/tutorial`.
- Do not use the signed macOS zip as `publish-client` input until the artifact mismatch is resolved.
- Distribute the CA fingerprint through a channel independent of the pkgreg page/download.
- Tell users that Git and OCI are pull/read-only and apt/apk requires HTTP origins.
- Keep the data directory on a supported local filesystem with hardlinks.
- Back up the control DB, host key, CA/key, catalog, blob store, downloads, and configuration as one tested recovery set.
- Treat restart as console-session invalidation.
- Run `doctor`, then independently verify auth, plaintext rejection, proxy destination restriction, downloads, and a package smoke test.

## Bottom line

The hard engineering core is much further along than the adoption surface suggests. The main risk is not that pkgreg fails to cache packages. It is that the fastest path to seeing it work creates an unsafe service, while the path to securing it breaks several headline client modes. Fixing the default boundary, release/bootstrap chain, and protected-project protocol gaps would materially change the product from a guided technical preview into a credible self-serve trial.
