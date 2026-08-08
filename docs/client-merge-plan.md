# One client, three tiers

Plan, 2026-08-08. Merges `pkgreg-client` and `pkgcache` into a single program, and
gives it the resolution chain: **this machine → the team's cache → the registries**.

Prerequisite reading: [pkgcache.md](pkgcache.md) for what exists,
[local-cache-plan.md](local-cache-plan.md) for why it is shaped that way.

---

## Why merge

The two programs have converged. `pkgcache` already carries the entire client surface —
`run`, `shell`, `env`, `build`, `compose`, `persist`, `docker-setup`,
`docker-build-setup` — and `pkgreg-client` carries one thing `pkgcache` lacks: the
fingerprint-pinned CA fetch that lets a laptop trust a team cache without installing
anything.

Keeping them apart forces a choice nobody should have to make. A developer with both a
laptop cache and a team server runs two daemons on two loopback ports and has to know
which one their `.npmrc` names. That is the whole argument; everything else is detail.

**The absorption goes one way.** `pkgcache` gains ~200 lines of trust code from
`internal/clientinstaller`. The reverse — teaching `pkgreg-client` the engine — means
moving the daemon lifecycle, the budget, the store guard and two databases.

**What it costs, stated plainly.** The client goes from a small loopback proxy to ~26 MB
carrying SQLite, the console and the control plane, on every laptop and in every CI
image. It also overturns a decision this repository wrote down — the Makefile says of
the bridge, *"a developer laptop should not have to run a copy of the server to talk to
one."* That sentence should be deleted rather than left to rot: after this, a laptop
does run a copy, deliberately, because a copy is what a local cache is.

Local caching stays **opt-in at runtime**. Without it no store is opened, no database
touched, and the program behaves exactly as the bridge does today. Size is the only cost
an existing user pays.

---

## What a client runs

One-time, per machine:

```sh
pkgcache setup -server https://cache.internal:8443 -ca-sha256 AB:CD:… -limit 25G
```

That is the whole installation. It verifies the team cache's CA against the fingerprint
you were given out of band, records it, sets the disk budget, and writes nothing outside
the cache directory.

Then, forever after:

```sh
pkgcache run -- npm ci          # or uv sync, pip install, cargo, go mod download…
pkgcache shell                  # the same, as a shell you stay in
pkgcache build -t app .         # docker build, Dockerfile untouched
```

Each of those starts the daemon if it is not running. Nothing else is required.

For a machine where settings must outlive the session — CI, an IDE, a colleague's
Makefile:

```sh
pkgcache persist                # ~/.npmrc, pip.conf, uv.toml, .gitconfig + socket activation
```

### The variants

| Situation | Command |
|---|---|
| Laptop, team cache, local caching | `pkgcache setup -server … -ca-sha256 … -limit 25G` |
| Laptop, no team cache | `pkgcache setup -limit 25G` |
| Just a verified shell, no local store — today's `pkgreg-client` | `pkgcache setup -server … -ca-sha256 … -no-cache` |
| CI | `PKGCACHE_LIMIT=25G PKGCACHE_SERVER=… PKGCACHE_CA_SHA256=… pkgcache run -- make` |
| On a plane | `pkgcache run -offline -- uv sync` |

### What `status` shows

Tiering is only useful if you can see it, so `status` reports all three:

```
cache      ~/.local/share/pkgcache
daemon     running, pid 4127, up 2h13m
local      3.2 GiB of 25 GiB          1,847 hits today
team       https://cache.internal:8443  reachable, 12 ms   214 hits today
direct     allowed                     31 fetches today
```

When the team cache is down it says so, rather than a build simply getting slower for a
week before anyone notices:

```
team       https://cache.internal:8443  UNREACHABLE since 09:14 (connection refused)
direct     allowed — 4,102 fetches went straight to the internet
```

---

## The engine work, which is the real cost

The chain does not exist today and is **independent of the merge**. Two facts:

- `Ctx.Upstreams()` ([ctx.go:267-278](../go/internal/eco/ctx.go#L267-L278)) returns an
  unordered `map[string]string`. `Priority` is carried in `control.Upstream` and
  `config.Peer` and then discarded at the point of use. `SingleUpstream()` returns the
  first entry of a map iteration, which is not deterministic.
- Peering cannot be the middle tier. It is digest-addressed, so a peer can serve bytes
  whose digest you already know but cannot answer "what is the latest numpy". A cold
  cache has to resolve an index, and index resolution has no fallback.

So the work is: **ordered upstream lists, with failover.**

### Failure semantics, which is where this goes wrong if rushed

| The team cache | Fall through to the internet |
|---|---|
| connection refused, DNS failure | **yes** — the case this exists for |
| timeout | yes, but on a *short* connect budget. The request timeout is 20 minutes so a 2.5 GB wheel can finish; inheriting that means a dead team cache stalls every build for 20 minutes before anything else is tried |
| 5xx | yes, after one retry |
| 404 | **no.** A cache in deliberate offline mode answers exactly this, and falling through would quietly defeat the air gap somebody switched on |
| 401, 403 | **no.** That is a misconfiguration, and going direct hides it |

### The policy, and its default

Silently reaching the internet when the team cache is down is right for a laptop and
wrong for a controlled environment. So it is a per-project setting, `fallthrough`, and
the two profiles disagree on purpose:

- **`pkgcache`**: `direct`. A developer who cannot reach the office should keep working.
- **`pkgreg serve`**: `fail`. A mirror that silently stops mirroring is worse than one
  that reports an outage.

`pkgcache setup -no-direct` opts a laptop into the strict behaviour.

---

## Milestones

**N1 — ordered upstreams.** `config.ProjectUpstreams` becomes
`project → eco → name → []Endpoint` ordered by priority; `Ctx` keeps `Upstreams()`
returning the highest-priority entry per name for compatibility and gains
`UpstreamChain(name)`. No behaviour change: every existing configuration is a chain of
one.
*Accepts:* the differential harness passes unchanged — this touches the server, and a
chain of one must be byte-identical to today. A test pins that `SingleUpstream` is now
deterministic.

**N2 — failover.** The chain is tried in order with the table above. A per-endpoint
connect budget, separate from the request timeout. `fallthrough` policy, defaulting to
`fail` for servers and `direct` for the local profile.
*Accepts:* a synthetic upstream that refuses connections, times out, 404s, and 403s in
turn, asserting fall-through happens for exactly the first two; a dead middle tier costs
seconds rather than the request timeout.

**N3 — shared trust.** Lift the CA fetch and fingerprint pin out of
`internal/clientinstaller` into a package both callers use. `pkgcache` gains `-server`
and `-ca-sha256`, and stores the verified CA in its own directory.
*Accepts:* `pkgreg-client`'s existing tests pass against the lifted code; a wrong
fingerprint is refused with the same message it is today.

**N4 — `setup`, and tiering made visible.** One idempotent command writing
`<data-dir>/config.json`; the tier-aware `status` above; per-endpoint outcome metrics so
the counts are real rather than estimated.
*Accepts:* `setup` twice changes nothing the second time; `status` reports a team cache
that is down as down within one probe interval.

**N5 — absorb the client.** `-no-cache` for bridge-only behaviour identical to today.
Every `pkgreg-client` flag keeps working, `-cache-address` as a deprecated alias for
`-host-address`. `pkgreg-client`'s `main` becomes a thin shim that execs the merged
binary with a deprecation notice.
*Accepts:* the existing `clientinstaller`, `clientbridge` and `clientbuild` suites pass
untouched; a `-no-cache` run opens no database, which a test asserts by checking the
data directory stays empty.

**N6 — one binary, two names.** `pkgreg publish-client` publishes the merged program
under both names for one release cycle. The tutorial and
[client-onboarding.md](client-onboarding.md) are updated in the same commit as the code,
not after it.

**N7 — retire the shim** a release later.

---

## Risks

- **N1 and N2 change the server.** This is the one part of the merge that is not
  laptop-only, and a fallthrough bug there means a cache silently bypassing the mirror it
  exists to be. Opt-in per project, chain-of-one identical to today, differential harness
  as the gate.
- **Two loopback endpoints during the transition.** Anyone running both programs today
  gets two daemons. N5's shim should detect a running `pkgcache` and use it rather than
  starting a bridge beside it.
- **The 26 MB.** Real, and not worth solving with build tags — that would recreate the
  two binaries this plan exists to remove.
- **`-no-cache` is a promise.** If it ever opens a database, the merge has cost existing
  users something they did not ask for. Worth a test rather than a comment.

## Open questions

1. **Which name ships?** `pkgcache` is better and is what the local product is called;
   `pkgreg-client` is what every existing instruction, the tutorial and the signed
   release pipeline say. N6 assumes both for a cycle, but the survivor should be chosen
   before N5 rather than after.
2. **Should the team tier also be tried for `docker`?** OCI resolves through `/v2/`
   against a named registry alias rather than a single upstream, so the chain shape is
   the same but the configuration surface is per alias. Cheap to include, worth stating.
3. **Does `persist` need to know about tiers?** The files it writes name only the local
   endpoint, which is correct — the chain is the daemon's business. Confirm that stays
   true when the daemon is not running and socket activation starts it.
