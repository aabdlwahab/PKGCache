# pkgcache — a standalone, entirely client-side cache

Plan, 2026-08-08. Target: the Go implementation in [`go/`](../go/). For the shipped
server design see [system-overview.md](system-overview.md).

The name is reused deliberately: `pkgcache/` at the repository root is the retired
Python service, and this is what replaces the idea it stood for. Nothing in this plan
depends on that tree, which stays where it is until it is removed on its own schedule.

---

## What this is

Today pkgreg is a *host*: someone runs it on a machine, mints a CA, publishes a
client, and every developer points their tools at `https://cache:8443`. That is the
right shape for a team, and the wrong shape for one laptop. Everything that makes it
a server — TLS, a CA fingerprint, accounts, tokens, projects, an admin listener —
exists to make a shared network endpoint safe, and a developer who just wants
`pip install` to stop re-downloading the same wheel pays all of it.

**pkgreg local** is the same cache with no network surface. One binary, one
loopback port, no certificate, no account, no project, no privileged setup. A
developer installs it, runs a command, and their tools are cached. Nothing is
installed machine-wide and nothing is reachable from another machine.

The important claim is that this is **not a fork**. The engine, blob store, catalog,
ecosystems and console are used unchanged. Local mode is a locked configuration
profile plus a lifecycle wrapper, and the parts of the codebase it removes are
removed by not compiling them into a second `main`, not by editing them.

## Why it is nearly free

Four facts, verified in the current tree, are what make this a small project:

1. **A single port with no certificate already serves everything.** With
   `single_port: true` and no TLS pair, [listeners.go:80-87](../go/internal/app/listeners.go#L80-L87)
   binds one plain listener, and [dataplane.go:88-96](../go/internal/app/dataplane.go#L88-L96)
   routes the data plane, the apt/apk forward proxy, `/api/`, the console, `/metrics`
   and health off that one address. One port is already the whole product.
2. **With no accounts, the control plane is open.** [auth/guard.go](../go/internal/control/auth/guard.go)
   permits everything when `Accounts.Enabled()` is false. That is a critical finding
   on a routable address and exactly the desired behaviour on loopback — the console
   works, mutations work, nobody logs in.
3. **Loopback removes every trust problem.** [client-bridge.md](client-bridge.md)
   already measures this per client: over `http://127.0.0.1`, pip needs no
   `--trusted-host`, npm and uv need nothing, and native Linux Docker treats
   `127.0.0.0/8` as insecure by default. The bridge exists to *simulate* being local;
   local mode simply is.
4. **The client already knows how to configure tools.** `sessionEnvironment` in
   [clientbridge/session.go:186-244](../go/internal/clientbridge/session.go#L186-L244)
   builds `PIP_INDEX_URL`, `UV_DEFAULT_INDEX`, `NPM_CONFIG_REGISTRY`, `NO_PROXY` and
   friends. Local mode reuses it with a different base URL.

So the code that has to be written is a command surface, a lifecycle, and defaults
that suit a laptop instead of a server.

---

## The architecture decision: a per-user daemon, auto-started

Three shapes were possible.

| Shape | Verdict |
|---|---|
| Embed the engine in each command (`pkgcache run -- npm ci` opens the store itself) | **No.** SQLite and the blob store are single-writer. Two shells, or two CI jobs, would collide. It also loses in-flight coalescing between concurrent commands, which is the feature. |
| A user-level systemd/launchd service | **No, not as the requirement.** It reintroduces installation, and a laptop should not run a package cache during a flight it isn't building on. Offer it as an option, don't depend on it. |
| **An on-demand daemon, auto-started by the first command, idle-exiting** | **Yes.** One writer, shared coalescing, zero install, no residency cost when unused. |

`pkgcache run`/`shell` start the daemon if it is not up, wait for `/readyz`, and
proceed. The daemon exits after an idle interval with no in-flight fetches. This is
the `gpg-agent` model, and the failure modes are known: a stale lock, a stale port
file, a half-dead process. Handle them explicitly (below) rather than discovering
them.

### State, ports and discovery

- **Data directory** is per user, not `/var/lib/pkgreg` (the current default,
  [sources.go:21](../go/internal/config/sources.go#L21)):
  `$XDG_DATA_HOME/pkgreg` → `~/.local/share/pkgreg` on Linux,
  `~/Library/Application Support/pkgreg` on macOS, `%LOCALAPPDATA%\pkgreg` on Windows.
  Created `0700`.
- **Port**: a fixed default (`127.0.0.1:41780`) so that a `.npmrc` or a `pip.conf` a
  developer chooses to write by hand stays valid across restarts. If it is taken by
  something that is not our daemon, fall back to an ephemeral port.
- **Discovery**: `<data-dir>/local.json` records pid, port, version and start time,
  written after the listener binds and removed on clean exit. `run`/`shell`/`env`
  read it; a stale file whose pid is gone, or whose port answers something that is
  not our `/version`, is replaced rather than trusted.
- **Locking**: an `flock` on `<data-dir>/lock` is what actually prevents two daemons.
  The JSON file is discovery, not mutual exclusion — a pid check alone races.

### The safety property that pays for dropping authentication

Local mode **refuses to bind anything but loopback**. Not a warning — a startup
error. Every removal below (no TLS, no accounts, no tokens, no CA) is justified by
that one enforced invariant, so it must not be a default that a flag can quietly
undo. `config.Posture` gains a local-mode reading: the loopback findings
(`auth_disabled_loopback`, `open_proxy_loopback`, `cleartext_admin`) become
`SeverityInfo` "this is local mode", and any off-host address is refused outright.

**The honest limitation:** a loopback TCP socket is not authenticated per user. On a
multi-user machine, any local user can drive another's daemon and consume their
cache. This is the same caveat [client-bridge.md](client-bridge.md#when-to-fall-back)
already states for the bridge. Document it in the same words, keep the data
directory `0700` so the *contents* stay private, and provide `-token` for anyone who
wants the socket gated. Do not claim isolation the socket does not provide.

---

## Client-side coverage, honestly

"Everything client side" holds cleanly for four ecosystems and needs qualification
for two. Stating that up front is better than a matrix that quietly implies six.

| Ecosystem | How it is configured | Privileged setup? |
|---|---|---|
| **pypi** (pip, uv) | `PIP_INDEX_URL`, `UV_DEFAULT_INDEX` in the child environment | none |
| **npm** (npm, yarn, pnpm) | `NPM_CONFIG_REGISTRY` | none |
| **git** | `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n` injecting `url.<local>/global/git/<host>/.insteadOf` — the same trick the Dockerfile rewriter already uses ([dockerfile.go:233-241](../go/internal/dockerfile/dockerfile.go#L233-L241)), applied to the session environment, which it currently is not | none |
| **files** | `PKGREG_FILES_URL` | none |
| **oci** (docker, podman) | native Linux: works over loopback with rewritten names, because `127.0.0.0/8` is in the daemon's default insecure list. Docker Desktop and remote daemons: the daemon's loopback is not yours | **Desktop/remote: yes** — one `~/.docker/daemon.json` edit and a daemon restart |
| **apt / apk** | `http_proxy` pointed at the same port. Works for a user-invoked `apt-get` under `sudo -E`; a plain `sudo apt-get` drops the environment | **for machine-wide: yes** — an `/etc/apt/apt.conf.d` drop-in |

---

## The existing client surface, ported one for one

`pkgreg-client` is not a thin wrapper — it is `build`, `compose`, `--persist`,
`-docker-trust`, `-docker-build-trust`, `-dry-run`, `-uninstall` and `-print`, each of
which took real work to get right. **All of it carries over with the same commands,
the same flags and the same semantics.** In every case the local version is the same
code doing *less*, because there is no certificate to install and no fingerprint to
verify.

| `pkgreg-client` | `pkgcache` | What changes |
|---|---|---|
| default (temporary shell) | `shell` | Same `sessionEnvironment`; base URL is the local daemon instead of a bridge. No CA fetch, no fingerprint check. |
| — | `run -- <cmd>` | New. One command instead of a shell; same environment. |
| `build [docker flags] PATH` | `build [docker flags] PATH` | **Identical UX.** Same Dockerfile rewrite, same `-print`, same `-keep-images`, same pass-through of unknown docker flags. |
| `compose [flags] CMD` | `compose [flags] CMD` | **Identical UX.** Same `docker compose config` render, same per-service Dockerfile rewrite, same stdin feed. |
| `-cache-address` | `-host-address` | Same purpose — "the daemon cannot see my loopback" — with a local meaning: `host.docker.internal` instead of the cache's own hostname. |
| `-docker-trust` | `docker-setup` | Same shape (`-dry-run`, `-uninstall`, one file, no root on Desktop). Installs an `insecure-registries` entry for the local address instead of a CA, since there is no CA. |
| `-docker-build-trust` | `docker-build-setup` | Same mechanism verbatim: the `proxies` block in the Docker client's `config.json`, marked `pkgregManaged`, pointed at the local port. |
| `--persist` | `persist` | Same intent — settings that outlive the shell. Locally this is `~/.npmrc`, `~/.config/pip/pip.conf`, `~/.gitconfig` `insteadOf` and an apt drop-in, all user-level. No downloaded setup script, no OS trust store, no root. |
| `-print`, `-dry-run`, `-uninstall` | unchanged | Every mutating mode keeps all three. |
| `-server`, `-ca-sha256`, `-ca-file`, `-cookie-file`, `-host`, `-cache-ip` | dropped | There is no remote server, no CA and no hosts entry to manage. |
| `-token-file` | `-token` (optional) | Only if the user opts into gating the loopback socket. |

### The Dockerfile rewriter needs one new mode, and nothing else

[`internal/dockerfile`](../go/internal/dockerfile/dockerfile.go) is the valuable part
and it is entirely reusable: FROM mapping with the `library/` and registry-alias
rules, the ARG block repeated after every stage, `GIT_CONFIG_*` `insteadOf`, heredoc
and line-continuation parsing, stage-name detection, `Change` reporting, and the
Compose equivalent. None of that is address-specific.

Today it has two modes ([dockerfile.go:24-36](../go/internal/dockerfile/dockerfile.go#L24-L36)).
Local mode adds a third:

| Mode | Base | CA handling | Docker invocation |
|---|---|---|---|
| `Bridge` (exists) | loopback, plain HTTP | none | `--network=host` |
| `CacheAddress` (exists) | cache's HTTPS origin | `--mount=type=secret` + `PIP_CERT`/`SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`/`NPM_CONFIG_CAFILE`/`GIT_SSL_CAINFO`/`UV_NATIVE_TLS` | `--secret id=pkgreg-ca` |
| **`HostGateway` (new)** | `http://host.docker.internal:41780` | **none** | `--add-host=host.docker.internal:host-gateway` |

`HostGateway` is `CacheAddress` minus everything: plain HTTP means no secret, no
mount, no six CA arguments, and no `NeedsSecret` plumbing. `Bridge` is reused
unchanged for native Linux — `clientbuild` simply takes its `Base` from the running
local daemon rather than from `PKGREG_BRIDGE_URL`.

Two details that must not be missed:

- **`no_proxy` has to include `host.docker.internal`.** The existing ARG block emits
  `no_proxy=127.0.0.1,localhost` ([dockerfile.go:243-250](../go/internal/dockerfile/dockerfile.go#L243-L250)),
  which is correct for `Bridge` and wrong for `HostGateway`: pip and npm traffic to
  the cache would be sent through the cache's own apt proxy.
- **The `FROM` rewrite needs the daemon to accept a plain-HTTP registry.** Loopback is
  insecure by default; `host.docker.internal:41780` is not. That is exactly what
  `docker-setup` installs, so on Docker Desktop `build -host-address` depends on it —
  say so in the error message rather than letting the pull fail with a TLS error.

### `docker-setup` also unlocks unmodified image names

The same file that carries `insecure-registries` can carry `registry-mirrors`, and
`Server.RegistryMirror` ([types.go:89-101](../go/internal/config/types.go#L89-L101))
already makes a bare `/v2/library/alpine` resolve. With both set, `docker pull
python:3.12-slim` — untouched, no rewrite, no wrapper — is served from the local
cache. `build`'s FROM rewriting then becomes optional, which is what `-keep-images`
already expresses. Offer it as `docker-setup -mirror`, not as the default: it changes
where *every* pull on the machine goes.

### The consequence nobody would predict: persistence versus idle exit

`persist`, `docker-build-setup` and `docker-setup -mirror` all write settings that
outlive the session, pointing at a daemon designed to exit when idle. A `.npmrc` that
names a dead port is worse than no `.npmrc` — `npm install` fails with
`ECONNREFUSED` instead of working slowly.

Two ways out, and the plan takes both:

1. **Socket activation** on Linux (systemd user socket) and launchd on macOS. The
   port is always listenable; the daemon starts on the first connection and still
   idle-exits. This is the correct answer and it makes persistent settings safe by
   construction.
2. **Where socket activation is unavailable** (Windows, a machine without systemd),
   any persistent installation disables idle exit and installs a user-level autostart
   entry. `persist` says which of the two it did, and `-uninstall` reverses it.

A persistent mode that can leave the machine broken when a background process exits
is not a mode to ship on a promise; this is the one part of the plan that has to be
designed rather than ported.

---

## Disk policy: the user decides, and nothing is ever deleted silently

This is the sharpest divergence from server mode, and it is a deliberate reversal of
the plan's first draft. The server evicts; **pkgcache does not**. A developer's cache
holds the wheels their current work depends on, and a background process quietly
deleting them to hold a number nobody chose is the wrong default on a machine someone
is sitting in front of.

So: **the limit is mandatory and explicit, exhaustion is a loud stop rather than a
quiet delete, and reclaiming space is always something the user asked for.**

| Rule | Behaviour |
|---|---|
| **No limit, no service** | A fresh install refuses to run until `pkgcache limit 25G` (or `pkgcache limit none`, which means "disk floor only"). `PKGCACHE_LIMIT` covers CI, so this is one setup line, not an interactive prompt. |
| **Full means stop caching, not stop working** | Over the limit, the artifact is still fetched and served — the build completes. It is simply not stored. Existing cache hits keep being served from disk. |
| **Never evict, never GC on a timer** | `evict_*` stays off and `gc_interval` is 0. `pkgcache prune` and `pkgcache gc` exist and do exactly what they say, when asked. |
| **A disk floor underneath the limit** | Refuse to cache below a free-space reserve even when under the limit, so pkgcache is never the reason a disk fills. |
| **Say so, four ways** | stderr of `run`/`shell`; a persistent banner in the console and a non-zero `pkgcache status`; an OS desktop notification (`notify-send`/`osascript`/toast), rate-limited to once an hour and skipped with no desktop session; and a non-zero exit from pkgcache commands. |

### The exit-code rule, spelled out

Every pkgcache command exits non-zero while the cache is full — `run` included. That
was decided knowingly: a CI pipeline that has silently stopped caching should go red.
The one refinement is which non-zero:

- The child's own failure always wins. `npm ci` exiting 1 surfaces as 1, never masked.
- A child that **succeeded** while the cache was full surfaces as **75** (`EX_TEMPFAIL`),
  a code no package manager returns, so "the build failed" and "the build worked but
  nothing was cached" are never confused.

The remaining local defaults are ordinary:

| Setting | Server | pkgcache | Why |
|---|---|---|---|
| `catalog.read_pool_size` | 8 | 4 | one developer, not twenty CI hosts |
| `log.format` | json | **text** | it is going to a terminal |
| `log.access` | true | false | one line per request is noise on a laptop |
| idle exit | n/a | **15 min** | don't hold a process nobody is using |

`pkgcache status` prints size against the limit, free disk, hit rate, and the oldest
content — the catalog's cache-age query already computes the last, and it is what
makes `prune` an informed decision rather than a guess.

## Two upstreams, not one: local in front of team

The feature that makes this more than a private copy: a laptop cache whose upstreams
point at the **team's** pkgreg is a two-level hierarchy for free, with no new code —
it is ordinary upstream configuration. First developer on the team pays the internet
fetch; second pays the LAN; the same developer twice pays nothing.

```
pkgcache upstream --from https://cache.internal:8443 --project global
```

writes the pypi/npm/oci upstream URLs to point at the team cache and stores its CA.
The digest-addressed `peer` mechanism is *not* the right tool here (it needs a
`peer`-scoped token and only resolves by digest); plain upstreams re-resolve indexes,
which is what a cold laptop needs.

`--from` is also the natural place to accept an **air-gap pack**: `pkgreg import`
already verifies and applies one, so a developer can be seeded from a colleague's
export with no network at all.

---

## What the local binary does not include

Not deleted — not linked. `cmd/pkgcache` composes `app.Open` with a locked
snapshot and a smaller command table.

| Dropped | Because |
|---|---|
| PKI, CA minting, `init`, `publish-client`, `internal/onboarding`, `internal/pki` | no TLS, no trust to establish, nothing to publish |
| From `internal/clientinstaller`: the CA fetch, the fingerprint pin, the downloaded setup script, the OS trust store, the hosts entry | there is no certificate and no remote host. The *shapes* — dry-run, uninstall, print, managed-key marking — are kept and reused |
| Accounts, sessions, tokens, audit, sealed upstream credentials | no accounts exist; the guard is open by construction |
| Projects and multi-tenancy | one implicit `global`; URLs keep the `/global/` segment so paths stay identical to server mode and nothing in the router special-cases local |
| Quotas, rate limits, peers | one user, one machine |
| `systemd install`, `migrate` | not a service, not migrating from the Python stack |
| Headless mode | the console is loopback-only already; it is the whole UI |

**Kept, deliberately:** the console (a local cache with a size/hit-rate view is
better than one without), `offline`, `checkpoint`/`export`/`import`, `lockwarm`,
`gc`/`evict`, `doctor`, and the whole engine.

`control.db` is still opened. Making it optional means threading nil-ability through
the data plane and API for no user-visible gain; an empty database costs a few
kilobytes.

### Command surface

```
pkgcache run -- <command>      run one command with tools pointed at the cache
pkgcache shell                 child shell, same environment, exit to restore
pkgcache env                   print exports for a shell that wants them
pkgcache build [docker flags]  docker build through the cache, Dockerfile untouched
pkgcache compose [flags] CMD   the same for docker compose
pkgcache docker-setup          teach the daemon this address  [-mirror -dry-run -uninstall]
pkgcache docker-build-setup    proxy every build on this machine  [-dry-run -uninstall]
pkgcache persist               settings that outlive the session  [-print -dry-run -uninstall]
pkgcache limit 25G | none      set the cache budget; required before first use
pkgcache status                size against the limit, free disk, hit rate, oldest content
pkgcache console               open the local console in a browser
pkgcache offline [on|off]      serve from cache only
pkgcache upstream --from URL   point at a team cache, or import a pack
pkgcache prune | gc            reclaim space, only when asked
pkgcache stop | doctor | version
```

`run` is the primary verb, and the one to put in a README: `pkgcache run -- npm ci`
works in CI and on a laptop, needs no shell integration, and leaves nothing behind.

---

## Milestones

Each is independently useful and independently testable. Acceptance criteria are
tests, not descriptions.

**M1 — the local profile.** `config.LocalDefaults()`, loopback-bind enforcement,
per-user data directory resolution, local posture reading. `cmd/pkgcache` with
`serve` and `version` only.
*Accepts:* a table test asserting every non-loopback address is refused; a test that
the profile validates and that `Posture` reports no warning above Info.

**M2 — lifecycle.** Auto-start, `flock`, `local.json`, stale-state recovery, idle
shutdown, `stop`, `status`.
*Accepts:* twenty concurrent `run` invocations start exactly one daemon; a `SIGKILL`ed
daemon's leftover state is recovered without operator action; idle shutdown fires and
a subsequent `run` restarts cleanly.

**M3 — the session.** `run`, `shell`, `env` over the local base URL, reusing
`sessionEnvironment`; add the `GIT_CONFIG_*` injection it currently lacks.
*Accepts:* extend [test/acceptance/clients_test.go](../go/test/acceptance/clients_test.go)
with real `pip`, `uv`, `npm` and `git` against a local daemon, no CA anywhere on the
machine and no `--trusted-host`; a second run of each is served from cache with zero
upstream requests.

**M4 — disk policy.** The mandatory limit and `pkgcache limit`, the free-space floor,
serve-but-do-not-store when full, the four notification channels, the 75 exit code,
`status` and `prune`.
*Accepts:* a store driven past its limit still serves every artifact correctly —
byte-identical to upstream — and stores none of them; nothing is ever deleted without
`prune`; a fresh install refuses to serve until a limit is set, and `PKGCACHE_LIMIT`
satisfies that non-interactively; the desktop notification is attempted at most once
an hour and its absence never fails a command.

**M5 — `build` and `compose` at parity.** The `HostGateway` mode in
`internal/dockerfile`, `no_proxy` fix, `clientbuild` sourcing its base from the local
daemon, `-host-address` replacing `-cache-address`. Every existing flag keeps working.
*Accepts:* the existing [dockerfile](../go/internal/dockerfile/dockerfile_test.go) and
[clientbuild](../go/internal/clientbuild/clientbuild_test.go) suites run unchanged
against the new mode, plus golden-file tests asserting `HostGateway` emits **no**
secret mount and no CA arguments; `build -print` on the repo's own Dockerfiles is
byte-stable. A real `docker build` in CI on native Linux, from a cold cache and then a
warm one, with the origin seeing one request.

**M6 — the machine-touching modes.** `docker-setup` (insecure-registries, optional
`-mirror`), `docker-build-setup` (the `proxies` block), each with `-dry-run` and
`-uninstall`.
*Accepts:* the [test/onboardingos](../go/test/onboardingos) and
[dockertrust](../go/internal/clientinstaller/dockertrust_test.go) patterns — apply to a
scratch config, assert the file, assert `-uninstall` restores it byte for byte, assert
a hand-written entry is never removed (the `pkgregManaged` marker already does this).

**M7 — `persist`, and daemon availability.** Socket activation on Linux and macOS;
the no-idle-exit-plus-autostart fallback elsewhere; user-level `.npmrc`, `pip.conf`,
git `insteadOf` and apt drop-in, all reversible.
*Accepts:* with the daemon stopped, a socket-activated `npm install` starts it and
succeeds. `persist -uninstall` leaves every touched file byte-identical to its
pre-install state. On a platform without socket activation, `persist` refuses to leave
idle exit enabled.

**M8 — team upstream and packs.** `upstream --from`, CA handling for the team cache,
`import` of a pack.
*Accepts:* a two-instance test where the local daemon's miss is served by a second
pkgreg and the internet origin sees no request.

**M9 — distribution.** `go install`, release archives for the five targets already
cross-compiled by `make client-publish`, a README that is a `run` command and nothing
else.

M1–M4 give a working cache. M5–M7 are what "the same as before" means, and they are
the majority of the remaining work — the ports are cheap, the persistence question in
M7 is not.

---

## Risks, and what to do about them

- **The loopback socket is not per-user.** Stated above; documented, not papered over.
  `-token` for those who need it.
- **Docker Desktop is the one place the promise bends.** Say so on the first page of
  the README rather than in a troubleshooting section. One reversible file edit is a
  fair price; discovering it after installing is not.
- **A stale daemon serving an old binary** after an upgrade. `local.json` records the
  version; a client whose version differs stops the daemon and restarts it.
- **Persistent settings outliving the daemon** — the `ECONNREFUSED` failure above. The
  single biggest risk in the plan, and the reason M7 is its own milestone rather than a
  flag on M6. Socket activation first; refusing to install without a resident daemon
  second; a broken `npm install` never.
- **Three rewriter modes instead of two** is a combinatorial step in the one package
  where a subtle bug produces a build that succeeds and ships the wrong image. Keep the
  golden-file tests per mode, and keep `HostGateway` defined as "`CacheAddress` with
  the TLS parts removed" rather than as a third independent code path.
- **`sudo apt-get` drops the environment.** Recommend `sudo -E`; offer the
  `apt.conf.d` drop-in through `docker-setup`'s sibling only for people who ask.
- **Divergence between server and local defaults** turning into two products. Guard it
  with a test asserting the local profile is `Defaults()` plus an explicit, enumerated
  diff — so a new server tunable cannot silently acquire an unreviewed local value.

## Decisions

Settled before implementation started. Recorded here because several of them are
choices a reader would otherwise assume were accidents.

| Decision | Choice |
|---|---|
| Name and shape | **`pkgcache`, a separate binary** at `go/cmd/pkgcache`, same module, sharing `internal/`. Coexists with `pkgreg-client`; neither replaces the other. |
| Scope | **All of M1–M9.** |
| Platforms | **Linux implemented and verified.** macOS and Windows branches written following the existing `clientinstaller` patterns and marked unverified in code and docs until someone runs them. |
| Shared packages | **Refactored in place.** `internal/dockerfile`, `internal/clientbuild` and `internal/clientinstaller` gain the local paths; the existing suites are the guardrail and must stay green. |
| Cache limit | **Mandatory and explicit.** No eviction, ever, without being asked. |
| Full behaviour | **Serve, don't store, warn on four channels.** |
| Exit codes | **Non-zero from every command while full, `run` included** — 75 when the child itself succeeded. The tradeoff (a green build reported red) was raised and accepted deliberately: silently-degraded caching in CI is the worse failure. |
| Team cache | **Private CA, no token.** The upstream leg reuses the client's fingerprint-verified CA fetch; local clients still see plain loopback HTTP. |
| Docker mirror | **Opt-in** via `docker-setup -mirror`. Rerouting every pull on a machine is not a side effect of a setup command. |
| `/global/` path segment | **Kept.** Zero router changes, identical URLs to server mode, and no user ever types one — `run` and `env` generate them. |
| Delivery | **A feature branch, one commit per milestone**, tests green at each. No push, no PR, unless asked. |
