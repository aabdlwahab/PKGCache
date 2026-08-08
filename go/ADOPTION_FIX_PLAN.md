# pkgreg — merged adoption fix plan

Merges `go/ADOPTION_FRICTION_AUDIT.md` (F-001…F-045) with a second independent live audit
of the same tree and build (`pkgreg 3b39fbb-dirty`, Linux amd64, Go 1.26.3). Supersedes
`docs/go-adoption-friction.md`, which has been folded in and removed.

Every item is tagged:

- **`F-nnn`** — from the original audit. Where I could reproduce it, it is marked
  **[re-verified]**.
- **`N-nn`** — new in this merge; not present in the original audit. All marked
  **[verified]** unless stated.

Effort is a coarse estimate: **S** ≈ under a day, **M** ≈ a few days, **L** ≈ a week or
more / needs a design decision.

---

## Conflicts resolved during the merge

Three places where the two audits disagreed. Resolved by testing, not by averaging.

**1. Console severity — the original audit is right; my earlier console findings were stale.**
I had carried forward the three P0s from
`.impeccable/critique/2026-07-31T08-52-26Z__go-internal-web-dist.md` (unconfirmed
destructive actions, full DOM rebuild per 64 KiB, five control-plane requests per fetch).
All three have since been fixed, and I verified each in the current tree:

| Critique P0 | Status now |
|---|---|
| 5 irreversible actions, no confirm | Fixed — `confirm()` at `sources.js:77,214`, `admin.js:243,257,385,450`, `transfer.js:58`, `connect.js:294` |
| Rebuild per 64 KiB progress frame | Fixed — `requestAnimationFrame` coalescing at `store.js:215` |
| 5 control-plane requests per fetch | Fixed — debounced `statsTimer` at `store.js:236` |
| No pagination on inventory | Fixed — `cache.js:15-19,103-121,237-245` |
| No skip link / unnamed charts | Fixed — `chrome.js:134`, `charts.js:43-47` |

The original audit's **Console UX 4/5** stands. Do not re-open these. The critique's
remaining P1s (contrast, light-mode eco hues, `.night` token gaps) were not re-checked and
are left at P3 below.

**2. Default-posture severity — the original audit understated it, and I understated it too.**
F-001 claims cleartext admin exposure on the unified port. I initially only found
unauthenticated control-plane access. Re-testing with **auth enabled** confirms the
original audit and goes further: plain HTTP on the unified TLS port serves the console,
tutorial, metrics, and API, **and accepts a login, returning a 12-hour session cookie with
no `Secure` flag** — so an operator who enables auth and signs in over `http://` puts the
password and a reusable session on the wire.

```
http://host:8443/console          → 200
http://host:8443/metrics          → 200
http://host:8443/api/v1/projects  → 401   (guard works)
POST http://host:8443/api/v1/login → 200, Set-Cookie: pkgreg_session=…; HttpOnly; SameSite=Lax
                                          ^ no Secure
```
This makes F-001 the single most important item in the plan.

**3. `proxy_allowlist`** — the original audit reasons from the source comment; I confirmed
the behaviour end to end. A stock instance relayed `http://example.com/` and
`http://neverssl.com/` to completion. `CONNECT` returns `405`, which caps the blast radius
at plaintext HTTP but does not make it not an open relay.

---

## P0 — **all four fixed**, verified live

Status as of 2026-08-01. Every claim below was re-tested against a running instance
after the change, and the whole suite passes under `go test ./...`, `go test -race
./...`, `go vet` and `gofmt`.

| # | Item | Status |
|---|---|---|
| 1a | Control auth off by default | **Fixed** — `init` provisions a superuser with a generated password, printed once |
| 1b | Cleartext admin surface + cleartext login | **Fixed** — plain branch is proxy-only, 308 to https; login refused over cleartext |
| 1c | Empty `proxy_allowlist` invisible | **Fixed as directed** — see the residual risk note below |
| 1d | `doctor` reports unsafe hosts as healthy | **Fixed** — fails with exit 4, from a posture model shared with `serve` |
| 2 | No LICENSE | **Fixed** — Apache-2.0 + NOTICE + SECURITY.md |
| 3 | darwin `.zip` unpublishable | **Fixed** — release ships raw signed binaries; CI gate added |
| 4 | `doctor` mutates and green-lights dead hosts | **Fixed** — read-only, distinct exit codes 3/4 |

**Residual risk, accepted by decision:** an empty `proxy_allowlist` still relays
anywhere. The alternative — refusing until configured — would have broken every
existing apt deployment on upgrade, and that trade was decided in favour of
compatibility. What changed is that the condition is now impossible to miss: `serve`
logs it at ERROR on every start, `doctor` **fails** with exit 4 on a non-loopback
listener, the generated config carries the setting with the SSRF explanation inline,
and `proxy_allowlist: ["*"]` exists so a deliberate choice is distinguishable from an
unconsidered default. A default install is still an open relay until someone acts on
the warning. `SECURITY.md` states this explicitly so a reporter is not surprised.

### What changed, by file

- `internal/config/posture.go` *(new)* — one posture model, so `serve` and `doctor`
  cannot form different opinions about the same fields. That disagreement was the
  root of F-001/1d.
- `internal/control/auth/bootstrap.go` *(new)* — idempotent superuser provisioning.
- `internal/app/dataplane.go` — `SinglePortPlainHandler`: proxy-only, 308 for
  origin-form, `/healthz` carve-out for probes.
- `internal/app/listeners.go` — the TLS-enabled split now binds that handler.
- `internal/control/api/v1.go` — `refuseCleartextLogin`, the exact negation of
  `cookieSecure`.
- `internal/eco/ctx.go` — `"*"` as the explicit relay-anywhere opt-in.
- `cmd/pkgreg/init.go` — provisioning, credential banner, rewritten `auth` and
  `proxy_allowlist` config blocks; creates the catalog so "initialized" is checkable.
- `cmd/pkgreg/doctor.go` — read-only, posture check, documented exit statuses.
- `cmd/pkgreg/main.go` — `flag.ErrHelp` exits 0; `exitCode` maps readiness classes.
- `cmd/pkgreg/publishclient.go`, `internal/clientrelease/clientrelease.go` —
  `NearMiss` diagnosis for archived artifacts.
- `.github/workflows/client-release.yml` — raw signed darwin binaries; a
  `verify-publishable` job that runs the operator's own command and blocks the release
  below 5/5.
- Tests: `internal/config/posture_test.go`, `internal/control/auth/bootstrap_test.go`,
  `internal/app/posture_test.go`, `internal/clientrelease/nearmiss_test.go`,
  `cmd/pkgreg/doctor_test.go`.

### Original findings, for reference

### 1. `F-001` + `N-01` — Default deployment is an unauthenticated, cleartext-capable admin plane and an open HTTP relay — [re-verified, expanded]
**Effort: L** (several independent sub-fixes; do them as separate PRs)

Four distinct defects behind one posture. Split them:

| Sub | Defect | Fix | Effort |
|---|---|---|---|
| 1a | Control auth off by default (`init.go:194` comments out `root_user`; `guard.go:47-119` bypasses every check when accounts are disabled). Anonymous `POST /api/v1/projects` → `201`. | `init` generates a random root password and prints it once, or binds loopback-only until auth is configured. | M |
| 1b | Origin-form plain HTTP on the unified port reaches console, tutorial, metrics and API; login over cleartext issues a non-`Secure` session. **[N-01, new]** | Accept only absolute-form proxy requests on the plaintext branch; 308 or reject origin-form. Refuse `/api/v1/login` over cleartext outright. | M |
| 1c | Empty `proxy_allowlist` = relay anywhere (`ctx.go:225-228`), and `init` never writes the key at all (`F-026`). | Emit `proxy_allowlist: []` in the starter config with the warning inline; disable the proxy until it is set. | S |
| 1d | `doctor` reports `healthy` for all of the above. | Make it **fail** when a non-loopback listener combines disabled auth, cleartext admin, or an empty allowlist. | S |

Add the regression test the original audit asks for: a default non-loopback instance must
not permit anonymous control mutation, cleartext login, or proxying to an unlisted host.

### 2. `F-011` — No LICENSE, NOTICE, or SECURITY.md — [re-verified]
**Effort: S.** Cheapest blocker in the plan. Legal and procurement stop here before any
technical evaluation. Add the license, a security-reporting policy, and third-party notices.

### 3. `F-003` — Signed macOS release artifacts cannot be published by the server — [re-verified, reproduced]
**Effort: M.** `client-release.yml:92-112` ships `pkgreg-client-darwin-*.zip`;
`clientrelease.go:36-37` accepts only bare names. Fed a faithful copy of the real release
payload, `publish-client` reports:

```
Still missing 2 of 5 client platforms:
  pkgreg-client-darwin-amd64
  pkgreg-client-darwin-arm64
```

macOS developers — the exact audience the tutorial's Docker case C is written for — get no
download. Unzipping by hand does not help: the recorded digests are the zips'. Pick one
canonical shape, make workflow/sums/publisher/API/tutorial agree, and add a CI step that
runs `publish-client` against the release artifacts and fails below 5 of 5.

### 4. `F-005` — `doctor` mutates a pristine directory and calls a dead host healthy — [re-verified]
**Effort: M.** Against a never-initialized path, `doctor` created `blobs/`, `managed/`,
`shuttle/`, `db/catalog.db`, `db/control.db` and `db/host.key`, then exited **0** with
`healthy, with 2 warning(s)` — for a host with no CA, no TLS, and nothing to download.
A typo'd `-data-dir` is silently populated. Add a read-only mode and make it the default;
separate "serves cache traffic" from "can onboard a developer" from "safe off loopback",
with distinct exit codes.

---

## P1 — fix before a pilot deployment

### 5. `F-002` — Token-gated projects are not supported end-to-end
**Effort: L.** apt cannot carry a bearer token through the generated proxy config
(`clientinstaller/client.go:280-305`, `router/project.go:107-148`), Docker trust rejects
`-token-file` (`dockertrust.go:51-54`), persistent setup rejects it too
(`client.go:111-113`) — while the tutorial presents `-token-file` as *the* answer for a
protected project. Three of six ecosystems silently lose authentication. This is a
protocol/installer gap, not a docs gap. Highest-value P1: it is the difference between
"caches packages" and "is a registry we can put private artifacts in".

### 6. `N-02` — `log.access` is dead configuration — [verified, new]
**Effort: M** (implement) / **S** (delete).
`Log.Access` is declared at `config/types.go:110` with the comment *"one structured line
per data-plane request"*, defaults to `true`, is settable via `PKGREG_LOG_ACCESS`, and is
written into every starter config as `access: true`. It is read in exactly one place —
`config/sources.go:269`, where the env var is parsed into it. **Nothing consumes it. There
is no HTTP access logging anywhere in the codebase.**

Verified: a server that served npm packuments and tarballs, OCI manifests, a full
`git clone`, and proxied apt traffic logged exactly two lines total — `starting` and
`listeners up`.

This is the finding most likely to lose an evaluating SRE: they turn a documented knob, see
nothing, and write off the observability story. Implement it — a cache with no request log
is not operable — or delete the field and its config comment.

### 7. `N-03` — Blob-store metrics report zero for six hours — [verified, new]
**Effort: S.** `pkgreg_blob_count` / `pkgreg_blob_store_bytes` are set at startup
(`app.go:206,288-297`) and after a GC or eviction pass
(`maintenance/service.go:264-271`). With the default `gc_interval: 6h`, a fresh instance
reports an empty store for six hours no matter what it caches. Verified: 6 blobs / 803 KiB
on disk, `doctor` counting them correctly, `/metrics` reporting `pkgreg_blob_count 0`.
Anyone who wires Grafana on day one sees a permanently empty cache. Refresh on the existing
30 s `stats_flush_interval` tick.

### 8. `F-004` + `F-019` + `N-04` — There is no server release; acquisition is the highest-friction step
**Effort: M.** No server release workflow, no tags (`git tag` is empty, so
`pkgreg version` reports `3b39fbb-dirty`), no checksums, no SBOM. **New: `go.mod:3`
requires `go 1.25.0` with no `toolchain` directive** — Go's auto-download rescue needs
`proxy.golang.org`, exactly what the air-gapped target site does not have, so the failure
on Debian 12 / Ubuntu 22.04 / RHEL 9 is obscure. Ship tagged, attested
`pkgreg-linux-{amd64,arm64}` + `pkgreg-SHA256SUMS`, add `toolchain go1.25.0`, and let
`init` / `systemd install` fetch and publish verified clients (closes the `F-004` dead end).

### 9. `F-012` — Storage, quota and rate policy are all unbounded by default — [re-verified]
**Effort: M.** `evict_target_bytes`, `evict_min_free_bytes`, `evict_ttl` all default to `0`
= disabled; GC only reclaims *unreferenced* blobs, and everything served is referenced. Per-
project byte quota, artifact quota and rate limit are also unlimited. A successful trial
fills the host. **New detail:** `doctor` never checks free space despite `internal/diskusage`
already existing and being used by the evictor — add it, plus an "eviction fully disabled"
warning and a `/readyz` free-space floor.

### 10. `F-009` — Enabling auth means a plaintext password in a 0644 file — [re-verified]
**Effort: S.** `init` writes `pkgreg.yaml` **0644** and suggests putting `root_password` in
it; the data tree is created `0755`; `control.db` (scrypt password hashes, token hashes) is
`0644`. Only `ca.key`, `server.key`, `host.key` get `0600`. Create the data dir `0700`,
write credential-bearing config `0600`, prefer `root_password_file` or a one-time generated
credential.

### 11. `F-007` + `N-05` + `N-06` — The one-command systemd path is not production-shaped
**Effort: M.** Beyond the original audit's transactional/preflight/uninstall points:

- **`N-05` [verified]: `-data-dir` must be directly under `/var/lib`** (`systemd.go:51-54`).
  `pkgreg systemd install -data-dir /srv/pkgreg` → `data-dir must be one safe directory
  directly under /var/lib`. A package cache is a storage appliance; its storage is almost
  always a dedicated mount. `-unit-dir` and `-bin-dir` are *not* similarly constrained, so
  the rule reads as arbitrary. The bind-mount workaround is undocumented.
- **`N-06` [verified]: `DynamicUser=yes` silently relocates state.** Real state lands in
  `/var/lib/private/pkgreg` (mode 0700, root-only) with `/var/lib/pkgreg` as a symlink. So
  every documented follow-up — `doctor -config /var/lib/pkgreg/pkgreg.yaml`,
  `publish-client -data-dir /var/lib/pkgreg`, `export` — needs root, and root-created files
  are root-owned inside the dynamic user's tree. Nothing says so.
- Upgrading errors by default: replacing the binary requires `-force` (`systemd.go:143`).
- No `AmbientCapabilities=CAP_NET_BIND_SERVICE`, so the service can never move to `:443`.
- Unit prints `console: https://<host>:8443/` — a literal placeholder.

### 12. `F-006` + `F-008` — Trust bootstrap is circular, and authenticated persistent setup needs a raw browser cookie
**Effort: M.** The evaluator is asked to click through a certificate warning for a CA they
have no independent way to obtain, and CI enrollment requires hand-extracting a session
cookie (`cmd/pkgreg-client/main.go:36-39`) that is process-memory state anyway (`F-023`).
Add a proper CLI login / enrollment-token flow and an out-of-band fingerprint channel.

### 13. `F-010` + `F-016` + `F-018` — Advertised breadth exceeds actual capability
**Effort: S** (matrix) / **L** (HTTPS apt). Six adapter families, not "every ecosystem".
Git and OCI are **pull-only**; only generic files accepts uploads; **apt/apk rejects
`CONNECT` (verified: `405`), so HTTPS-only Debian/Alpine repos — now the norm — cannot be
used at all.** Publish a capability matrix above the first install command and label each
adapter's boundary in the console, the tutorial, and its error messages.

---

## P2 — removes most day-one confusion

### 14. `N-07` — `npm ping` fails — [verified, new]
**Effort: S.** `npm.go:74-85` registers packument and tarball routes only.
`/-/ping`, `/-/whoami`, `/-/v1/search` all `404`. `npm ping` is *the* command a Node
developer runs to check a registry: it fails loudly while everything else works perfectly,
which reads as "the cache is broken". A handler returning `{}` closes it.

### 15. `N-08` — The PyPI URL grammar is unguessable, and the natural guess gets a bare stdlib 404 — [verified, new]
**Effort: S.** The real path is `/<project>/pypi/<index>/+simple/<pkg>/` — the `root/pypi`
index name and the `+simple` segment are unlike anything a pip user has seen.

```
/global/pypi/simple/requests/   → 404  "404 page not found"    ← bare Go default
/global/pypi/pypi/+simple/…     → 404  "unknown index pypi"    ← good
/nosuchproject/npm/left-pad     → 404  "unknown project …"     ← good
/global/npmm/left-pad           → 404  generic hint, does not name the ecosystem
```

The first line is the **only** place in the data plane that leaks Go's default 404 body, and
it is the path users are most likely to type. Add a catch-all under `/{project}/pypi/` that
explains the grammar and lists indexes (`/+indexes` already returns exactly that map), and
name the known ecosystems in the router's 404.

### 16. `F-013` + `N-09` — CLI help conventions
**Effort: S.**
- `pkgreg <cmd> -h` prints help, then `pkgreg: flag: help requested`, then exits **1**
  (`main.go:64-67`); `pkgreg-bridge -h` exits 2. `cmd/pkgreg-client/main.go:76-80` already
  has the correct `flag.ErrHelp` branch — reuse it.
- `pkgreg --version` returns unknown command, exit 2.
- **`N-09` [verified, new]: every subcommand advertises the whole listener flag block.**
  `bindConfigFlags` is attached to all commands, so `pkgreg gc -h` offers `-headless`,
  `-single-port`, `-unified-addr`, `-proxy-addr`, `-admin-addr`, `-offline`. Alphabetical
  ordering puts `-admin-addr` first for *every* command and buries `-project`, `-file`,
  `-snapshot`, `-dry-run`. `pkgreg export -headless` is accepted and meaningless. Split into
  "config resolution" (all commands) and "listener" (`serve`/`init`/`doctor` only).
- `checkpoint` exposes both `-m` and `-message`, both listed.

### 17. `F-014` + `F-015` — Tutorial is not actually complete without JavaScript, and coordinate discovery gives up permanently
**Effort: M.** `tutorial.js:1-3` claims noscript completeness, but downloads, checksums and
the fingerprint are script-populated and the HTML fallback keeps `PASTE_FINGERPRINT`.
`coords.js:156-190` resolves to `null` after a fixed 1.2 s and can never be replaced by a
later success, so one slow first start leaves a placeholder fingerprint for the whole page
session. Server-render coordinates and fingerprint into the embedded page.

### 18. `N-10` — `serve` gives the operator nothing to act on — [verified, new]
**Effort: S.** Default log format is JSON, so the entire reward for starting the server is
`{"msg":"listeners up","addresses":{"single":"[::]:8443"},…}`. No URL to open, no CA
fingerprint reminder, nothing clickable. Print a human start banner (URL, fingerprint,
posture summary) on a TTY, keep pure JSON when not.

### 19. `N-11` — Artifact inventory under-reports two of six ecosystems — [verified, new]
**Effort: M.** After exercising every ecosystem, `/api/v1/stats` and
`/api/v1/projects/global/artifacts` listed `oci` and `npm` only. The apt proxy fetch and the
git clone produced no inventory rows, so the console's "what is cached" view is blind to
them.

### 20. `F-025` + `F-031` — Operational endpoints and scheme handling
**Effort: M.** `/metrics`, `/healthz`, `/readyz`, `/version` sit outside the auth guard
(`admin.go:12-21`) — re-verified: `/metrics` returns 200 unauthenticated with auth enabled,
exposing project labels and ecosystem usage on an all-interfaces listener. Split liveness
from operator diagnostics; add HSTS once 1b lands.

### 21. `N-12` — Certificate SANs include every Docker bridge — [verified, new]
**Effort: S.** `pki.DiscoverSANs` produced 18 SANs on an ordinary dev box, 13 of them
`172.x.0.1` bridge gateways. Noisy in `init` and `doctor` output — and because `doctor`
re-runs discovery and warns about names the cert lacks, creating or removing a Docker
network later triggers a spurious *"certificate does not cover …; re-run `pkgreg init
-force`"*. Exclude container-bridge and link-local ranges unless explicitly listed.

### 22. `N-13` — Default ports collide with the stack pkgreg replaces — [verified, new]
**Effort: S.** `:8443`, `:3142`, `:8088` are exactly what the retired Python Compose stack
binds, so anyone doing an A/B comparison hits `bind: address already in use` on first
`serve`. `3142` is also apt-cacher-ng's conventional port. The error text is good; the
collision is avoidable.

### 23. `F-028` + `F-030` + `F-038` — Console and root-route first-run behaviour
**Effort: M.** `/` is a long animated product narrative where an operator expects post-install
status; `boot.js:27-34` renders the login form for *every* `/me` failure, so a 500 or a proxy
error looks like bad credentials; there is no setup-readiness view surfacing missing clients,
auth posture, or allowlist state.

### 24. `F-027` — Unquoted YAML path emission
**Effort: S.** `init.go:143-203` formats `data_dir` and TLS paths as raw YAML text. A path
with a space or `#` produces a malformed or silently altered config. Serialize a typed value
through `yaml.v3`.

### 25. `F-034` — `publish-client` also publishes bridges but reports client completeness — [re-verified]
**Effort: S.** Live output listed five `pkgreg-bridge-*` files and then said
`All 5 client platforms are published`. Rename to `publish-tools` or report the two families
separately.

### 26. `N-14` — One-shot commands emit a JSON log line before their human output — [verified, new]
**Effort: S.** `doctor` suppresses it (`snap.Log.Level = "error"`); `checkpoint`, `gc`,
`export`, `import`, `migrate` do not, so a JSON `starting` object is the first thing on
screen for every operation. `-json` stdout is clean (verified), so this is cosmetic — and
one line to fix.

---

## P3 — polish, contracts, and scale posture

| ID | Issue | Effort |
|---|---|---|
| `N-15` | **Server does not build for Windows** — `doctor.go:282-283` uses `syscall.Rlimit`/`Getrlimit`; `GOOS=windows go build ./cmd/pkgreg` fails. darwin and linux/arm64 build fine. A Windows contributor cannot run `go build ./...` or `go test ./...` at all, even though the *client* is cross-platform. Ten-line `_unix.go`/`_other.go` split — the pattern `internal/diskusage` already uses. **[verified, new]** | S |
| `F-020`,`F-021`,`F-022`,`F-044` | Deployment envelope: supported filesystems (SQLite + hardlinks, not NFS), whole-instance backup/restore consistency set, CA/host-key rotation, upgrade/downgrade and schema-compatibility runbook. | L |
| `F-024`,`F-023` | Local-accounts-only identity; sessions are process memory, so every restart signs everyone out. Document the scope; decide the SSO/HA posture. | L |
| `F-042` | No OpenAPI, generated types, versioning or deprecation policy for a control API teams will script during evaluation. | M |
| `F-035`,`F-036` | Coverage: no token-gated real-client matrix; privileged/2 GiB/native-OS runs are opt-in. **Re-verified:** `make test` is green while `TestPipInstall`, `TestProductionCorpus`, `TestM2TwentyClientsSingle2GiB` and `TestPrivilegedInstallIdempotencyTrustAndUninstall` all skip — an evaluator reads "ok" across 46 packages and believes the 46/46 differential gate and the 2 GiB load test just ran. Print a skip summary at the end of `make test`. | M |
| `F-037` | `make lint` fails — `golangci-lint` is neither vendored nor installed by any target; CI pins v2.6.2. **[re-verified]** | S |
| `F-029` | `tokens.css:15` requests `/fonts/IBMPlexMono.woff2`; `dist/fonts/` holds only a README pointing at the deleted Vite console's build path. Falls back cleanly, but the product never renders as designed and still emits the request. | S |
| `F-033` | Checksum beside the binary proves integrity, not authenticity. Surface signature/notarization state and attestation links. | S |
| `F-039`,`F-040` | Tutorial is excellent but is one long scroll; first success proves a shell was configured, not that a cache hit happened. Add OS/ecosystem selectors and a per-ecosystem deterministic smoke test. | M |
| `F-041`,`F-043` | No single versioned compatibility matrix; no root-password recovery / bootstrap-rotation procedure. | M |
| `F-045` | `bin/` is gitignored, so a stale locally-built `pkgreg-bridge` can differ in linkage from release output. Put version/linkage metadata in every binary. | S |
| — | Remaining `.impeccable` P1s not re-checked in this pass: control/button contrast below WCAG 1.4.11, light-mode eco hues, `.night` unoverridden tokens, `--series-6`. | M |
| `N-16` | Files upload to a public project returns a bare `403` that does not point at Console → Connect the way the endpoints payload does. **[verified, new]** | S |
| `N-17` | `/api/v1/coordinates` is unauthenticated and returns the CA fingerprint. Correct by design — pinning is only meaningful out-of-band — but it needs one explicit sentence in the docs, because a reviewer will ask. **[verified, new]** | S |
| `F-017` | Docker has the highest onboarding cognitive load (~300 tutorial lines). Add a generated decision tree and a `pkgreg-client -check docker` self-test. | M |
| `N-18` | Repo root is the retired product: a 1151-line README mostly documenting the Python/Compose stack, beside `pkgcache/`, `webui/`, `docker-compose.yml`, `dnsmasq/`, `caches/`, `shuttle/`, `handoff/`, `handoffs/`, `design_handoff_checkpoint_transfer/`. The Go banner is correct but the surrounding evidence says "Python Compose project". Also `go/README.md` leads with a 10-phase P-numbered task table rather than what the product does. **[verified, new]** | M |

---

## What to fix next

The original top-ten is now seven of ten done — items 1, 2, 5, 6, 7, 9, 10 shipped with
the P0 pass, and `F-013`'s `-h` half came along with the exit-code work. What remains,
in order:

| # | Item | Why it is next | Effort |
|---:|---|---|---|
| 1 | `N-02` — implement or delete `log.access` | A documented, defaulted-on knob that does nothing. There is still no HTTP access log at all, which is the finding most likely to cost an evaluating SRE's confidence | M / S |
| 2 | `N-03` — refresh blob gauges on the stats tick | Nearly free, and it removes a permanently-empty Grafana on day one | S |
| 3 | `N-07` + `N-08` — `npm ping`, and a helpful 404 under `/{project}/pypi/` | Two tiny handlers; the first two commands a Node and a Python developer type | S |
| 4 | `N-09` — per-command flag sets | `pkgreg gc -headless` is still offered; `-admin-addr` still leads every command's help | S |
| 5 | `F-002` — token-gated projects end to end | The largest remaining correctness gap: apt, Docker and persistent setup silently cannot authenticate | L |
| 6 | `F-019` + `N-04` — ship a signed server release; add `toolchain go1.25.0` | Acquisition is still the highest-friction step, and the client release pipeline now has a working pattern to copy | M |
| 7 | `F-009` — data dir `0700`, config `0600`, DBs not world-readable | Partly defused (the generated config no longer contains a credential) but still worth closing | S |
| 8 | `F-012` — free-space check in `doctor`, eviction-disabled warning | The posture model added in this pass is the natural place to hang it | M |

Items 2, 3, 4 and 7 are all **S** and together are about a day.

---

## What the merged evidence says about readiness

The original audit's scorecard holds, with two adjustments from this merge:

| Area | Original | Merged | After the P0 pass | Why changed |
|---|---:|---:|---:|---|
| Core cache behavior | 4/5 | 4/5 | 4/5 | Confirmed independently — air-gap round trip, `git clone`, single-flight, dedup all verified, and unchanged by this pass |
| First-run path | 2/5 | 2/5 | **3/5** | `init` now yields a closed, checkable instance and `doctor` tells the truth about it; still a source build and a separate client-publish step |
| Secure defaults | 1/5 | 1/5 | **3.5/5** | Auth on, cleartext admin gone, unsafe posture fails the gate. Not 5: the empty allowlist still relays by decision, and metrics remain unauthenticated (`F-025`) |
| Protected-project UX | 1/5 | 1/5 | 1/5 | Untouched — `F-002` is the largest remaining gap |
| Distribution | 2/5 | 2/5 | **3/5** | The client release is publishable and CI-gated; there is still no server release |
| Documentation accuracy | 2.5/5 | 2/5 | **2.5/5** | The `auth` and `proxy_allowlist` config blocks now describe reality; `log.access` still documents behaviour that does not exist |
| Console UX | 4/5 | 4/5 | 4/5 | Confirmed — the `.impeccable` P0s are genuinely fixed |
| Production operations | 3/5 | 2.5/5 | 2.5/5 | Untouched: blob metrics still frozen for 6 h, no access log, no free-space check, systemd still cannot use a dedicated storage mount |

The engineering core was always well ahead of the adoption surface. After this pass the
first half of that sentence is less true as a criticism: the fastest documented path now
produces a closed instance, and the diagnostic that reports on it can be trusted and
scripted against. What has not changed is the protected-project protocol gap (`F-002`),
acquisition (`F-019`), and the telemetry an operator would use to build confidence —
still partly absent (`N-02`) and partly wrong (`N-03`).
