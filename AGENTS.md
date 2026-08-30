# AGENTS.md — orientation for an AI agent working in this repo

Read this before changing code. It explains the model the codebase is built on, the one
invariant that must not be broken, and — in full — how to add a new ecosystem, using
Maven as the worked example.

Deeper background lives in [docs/system-overview.md](docs/system-overview.md) (what it
does and why) and [docs/go-architecture.md](docs/go-architecture.md) (the design in
detail). This file is the operational version of both: what to touch, in what order.

Coding conventions for this project are a separate, enforced document —
[engineering-conventions/references/guidelines.md](engineering-conventions/references/guidelines.md).
Read it before any non-trivial code work. Nothing here overrides it.

---

## The model in sixty seconds

PKGCache is a pull-through cache in front of PyPI, npm, OCI registries, apt/apk, git and
plain files. One Go module builds two binaries — `pkgreg` (the shared server) and
`pkgcache` (the single-machine cache) — from the same code.

Four concepts carry the whole design:

| concept | what it is |
|---|---|
| **project** | a tenant, addressed as a **URL prefix** (`/<project>/<eco>/…`), never a port or a hostname |
| **ecosystem** | a protocol adapter: npm, pypi, oci, apt, git, files — the thing you add |
| **engine** | the one implementation of fetch → verify → hash → commit → serve, shared by every adapter |
| **catalog + blob store** | one SQLite catalog of `(project, eco, key)` entries pointing into one content-addressed blob store |

An ecosystem **describes** what it wants; the engine **executes** it. That split is the
whole extensibility story, and §"The one invariant" below is how it stays true.

---

## Repository map

```
go/                                    the implementation — one module, two binaries
  cmd/pkgreg, cmd/pkgcache             entry points (plus app/docker/bridge helpers)
  internal/app/app.go                  THE composition root: everything is wired here
  internal/app/dataplane.go            mounts each ecosystem's routes, resolves the tenant
  internal/eco/                        the extension point
    eco.go                             Ecosystem, Descriptor, Registry — read this first
    ctx.go                             Ctx: an adapter's entire view of the system
    npm/ pypi/ oci/ apt/ git/ files/   the six adapters
    ecotest/harness.go                 a live engine + blobs + catalog behind one adapter
  internal/engine/                     resolutions, documents, single-flight, progressive delivery
  internal/catalog/                    SQLite: entries, refs, artifacts, snapshots
  internal/blob/                       content-addressed store
  internal/router/                     mux.go (patterns) + project.go (tenant resolution)
  internal/config/                     the atomically-swapped snapshot, upstream chains
  internal/control/api/                the v1 HTTP API, descriptor-driven
  internal/session/, local/, onboarding/, clientbridge/   client-side wiring (see §Step 5)
docs/                                  the prose documentation
packaging/                             macOS, Ubuntu, Windows installers
examples/                              Dockerfiles that build through the cache
```

Rule of thumb for placement: **protocol logic goes in `internal/eco/<id>/`; everything
else belongs to a layer that already exists.**

---

## The one invariant

> An adapter contains protocol logic and nothing else.

No SQL, no filesystem access, no HTTP client, no single-flight, no hashing, no integrity
checking, no catalog bookkeeping. All of it is reachable through
[`eco.Ctx`](go/internal/eco/ctx.go), and only through it.

This is not style. The previous design reimplemented fetch-hash-commit six times with
small variations, and every variation was somewhere a bug could live alone. If you find
yourself opening a file or a `database/sql` handle inside `internal/eco/<id>/`, the
capability you need belongs on `Ctx` — add it there, once.

The matching rule for descriptors: **nothing outside a descriptor may hold a
per-ecosystem table.** Routing, the control API, the console's setup instructions, the
inventory exporter and snapshot inclusion all read `Descriptor`. An added `switch eco {
case "npm": … }` anywhere else is a defect, not an implementation.

---

## The request lifecycle

1. A listener (`UnifiedHandler`, or the forward proxy) receives the request.
2. [`internal/router/project.go`](go/internal/router/project.go) resolves the tenant in
   one of three shapes, chosen by the descriptor's `Listener`:
   - `ListenerPathPrefixed` — `/<project>/<eco>/…`, the uniform form (npm, pypi, git, files)
   - `ListenerProtocolRooted` — a fixed root the client cannot be talked out of; docker
     always starts at `/v2/`, so the project rides the image name
   - `ListenerForwardProxy` — absolute-form targets with no room for a project in the
     URL, so it rides the proxy username (apt, apk)
3. `DataPlane` strips the prefix, builds an `eco.Ctx` with project, engine and config
   already resolved, and hands the request to that adapter's own mux.
4. The adapter's handler recovers the context with `eco.CtxFrom(w, r, p)`, works out
   *what* is wanted, and calls one of:
   - `c.Serve(engine.Resolution{…})` — the streaming artifact path: cache hit, dedup,
     offline, or a single-flight fetch with progressive delivery
   - `c.Document(engine.DocSpec{…})` — a small, revalidating upstream document (an
     index, a packument, a `Release` file), buffered so the adapter can rewrite it
   - `c.ServeBytes` / `c.JSON` / `c.Text` — generated responses
5. Errors go to `c.WriteError(err)`, which maps engine errors onto the right status —
   including the offline-miss message every ecosystem must answer identically.

---

## The Ecosystem contract

Two methods, in [go/internal/eco/eco.go](go/internal/eco/eco.go):

```go
type Ecosystem interface {
    Descriptor() Descriptor
    Routes() []Route
}
```

The `Descriptor` declares four axes that decide how the rest of the system treats you:

| field | choices | picked by |
|---|---|---|
| `Storage` | `StorageBlob` \| `StorageManagedDir` | can the content be content-addressed as a unit? Only git says no |
| `Listener` | `path` \| `protocol-rooted` \| `forward-proxy` | how much room the client protocol leaves for a project prefix |
| `Upstreams` | `single` \| `named-set` \| `none` | one origin, a map of alias→origin, or an origin derived from the request |
| `Freshness` | `Immutable` \| `Revalidate(ttl)` per key | whether a cache key may ever be re-fetched |

Plus `ParseArtifact` (cache key → inventory identity) and `Setup` (the copy-paste client
instructions the console renders — this is *why* adding an ecosystem needs no frontend
change).

One caveat to know before you rely on it: `Freshness` is the **declaration**. The
adapter still enacts it by choosing `Serve` (immutable) or `Document` (revalidating) per
request; the two must agree. Keep them in one place in your file so they cannot drift.

Route patterns come from [`internal/router`](go/internal/router/mux.go):

```
/literal/path      literal segments
/{name}            exactly one segment, captured RAW (percent-escaped)
/{name...}         zero or more segments, slash-joined; may be followed by more segments
/prefix/           a trailing slash is significant
```

First match wins, in registration order. Captures are raw by default — call
`p.Unescape("name")` when you want the decoded form, and *don't* when the escaping is
load-bearing (npm's `@scope%2Fname`, apt's relayed URL). Admin routes (`/+indexes`,
`/+maintain`) must set `Admin: true`; the data plane registers those first so a greedy
protocol catch-all cannot shadow them.

---

## Adding an ecosystem, end to end: Maven

Everything below is the real checklist. The server side is descriptor-driven and short;
Step 5 is the part that is easy to forget.

### Step 0 — decide the four axes

For Maven:

- **Storage** `StorageBlob`. A `.jar` is a file with a digest; nothing needs a live directory.
- **Listener** `ListenerPathPrefixed`. Maven takes a mirror `<url>` and appends its own
  path, so `/<project>/maven/…` works with no client tricks.
- **Upstreams** `UpstreamNamedSet`. Central is not the only repository anyone uses —
  Google's Maven, Gradle's plugin portal and a company Nexus are all normal. A named set
  makes each one an alias in the path, exactly as pypi's indexes and OCI's registries are.
- **Freshness** `maven-metadata.xml` (and its checksum sidecars) is mutable and must
  revalidate; a released coordinate is immutable forever. `-SNAPSHOT` artifacts are the
  exception and must revalidate too — decide this deliberately, and write the test.

### Step 1 — the package

Create `go/internal/eco/maven/maven.go`. Model it on
[npm.go](go/internal/eco/npm/npm.go) (single upstream, simplest) or
[pypi.go](go/internal/eco/pypi/pypi.go) (named set, closest to Maven). The skeleton:

```go
// Package maven implements a Maven repository pull-through cache.
package maven

const ID = "maven"

var defaultRepos = map[string]string{
    "central": "https://repo1.maven.org/maven2",
    "google":  "https://dl.google.com/dl/android/maven2",
}

type Repo struct {
    repos map[string]string
    ttl   time.Duration
}

func (r *Repo) Descriptor() eco.Descriptor {
    return eco.Descriptor{
        ID:               ID,
        Display:          "Maven",
        Summary:          "Maven artifacts, POMs and repository metadata.",
        Storage:          eco.StorageBlob,
        Listener:         eco.ListenerPathPrefixed,
        Upstreams:        eco.UpstreamNamedSet,
        DefaultUpstreams: cloneMap(r.repos),
        Freshness: func(key string) eco.Freshness {
            if mutable(key) {          // maven-metadata.xml, -SNAPSHOT
                return eco.Revalidate(r.ttl)
            }
            return eco.Immutable
        },
        ParseArtifact: parseArtifactKey,
        Setup:         setupSteps,
    }
}

func (r *Repo) Routes() []eco.Route {
    return []eco.Route{
        {Methods: []string{http.MethodGet}, Pattern: "/+repos",
            Handler: r.list, Admin: true},
        {Methods: []string{http.MethodGet, http.MethodHead},
            Pattern: "/{repo}/{path...}", Handler: r.artifact},
    }
}
```

The handler describes; it does not fetch:

```go
func (r *Repo) artifact(w http.ResponseWriter, req *http.Request, p router.Params) {
    c := eco.CtxFrom(w, req, p)
    repo, artifactPath := p.Unescape("repo"), p.Unescape("path")

    origin, ok := c.Upstream(repo)          // configured chain head, or the default
    if !ok {
        _ = c.NotFound("unknown repository " + repo)
        return
    }
    if !validPath(artifactPath) {           // validate at the boundary: no "..", no leading /
        _ = c.NotFound("invalid artifact path")
        return
    }

    key := repo + "/" + artifactPath
    upstreamURL := eco.JoinURL(origin, artifactPath)

    name, version, _, isArtifact := parseArtifactKey(key)
    resolution := engine.Resolution{
        Key:        key,
        Upstream:   c.UpstreamRequest(upstreamURL, nil),   // chains + credentials, derived
        MediaType:  mediaType(artifactPath),
        AccessName: name,
    }
    if isArtifact {
        resolution.Artifact = &catalog.Artifact{
            Name: name, Version: version, Origin: upstreamURL,
        }
    }
    if err := c.Serve(resolution); err != nil {
        c.WriteError(err)
    }
}
```

Mutable metadata takes the document path instead, so concurrent callers collapse into
one fetch and the TTL is honoured:

```go
doc, err := c.Document(engine.DocSpec{
    Name: "metadata/" + key, Key: "metadata/" + key,
    URL: upstreamURL, TTL: r.ttl,
})
```

Four things to get right, all of which the existing adapters demonstrate:

- **`UpstreamRequest` is not optional.** It attaches the credential for that origin and
  the fallback chain derived from it. Building your own `http.Request` loses both, and
  bypasses the outbound boundary that metrics and timeouts live on.
- **Never rewrite a URL from the listen address.** If you emit links (Maven mostly does
  not — it composes paths itself), build them from `c.ExternalBase()`, which includes the
  stripped `/<project>/<eco>` root. A link built any other way sends clients past the
  cache or points them at an address only this host can reach.
- **`ParseArtifact` is inventory identity, not parsing for its own sake.** For Maven,
  `com/example/lib/1.2.0/lib-1.2.0.jar` → name `com.example:lib`, version `1.2.0`.
  Return `ok=false` for keys that are cached content but not artifacts (checksums,
  metadata) — that is what keeps the inventory honest.
- **`Setup` renders what the console shows.** Use `eco.ClientAuthority(host, port)` for
  the address, and `SetupContext.IsGlobal` where the default project is addressed
  differently. For Maven this is a `settings.xml` `<mirror>` block.

### Step 2 — register it

One line, in the composition root [go/internal/app/app.go](go/internal/app/app.go),
alongside the other six:

```go
ecosystems.Register(maven.New())
```

That is the whole server-side wiring. Registration panics on a duplicate ID or an
invalid descriptor — deliberately, at startup, rather than shadowing silently. From this
line the router, `GET /api/v1/ecosystems`, the endpoints panel, the console's setup
instructions, snapshot inclusion and the inventory all pick it up with no further edits.

### Step 3 — test it against a real engine

Use [`ecotest`](go/internal/eco/ecotest/harness.go), which stands up a live blob store,
catalog, engine and fake upstream behind your adapter. Adapter bugs live at the seams —
a cache key that does not match what the next request asks for, an artifact row never
written — and a mocked engine hides exactly those.

```go
h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
    origin.Serve("/com/example/lib/1.2.0/lib-1.2.0.jar", jarBytes)
    return maven.NewWithRepos(map[string]string{"central": origin.URL})
})
resp := h.Get("/central/com/example/lib/1.2.0/lib-1.2.0.jar")
// … request it twice, then assert h.Origin.Hits(path) == 1
```

Cover, at minimum: a cache hit costing one upstream request; the freshness split
(`d.FreshnessFor(key)` for both a released coordinate and `maven-metadata.xml`);
`ParseArtifact` on a real coordinate and on a checksum file; an offline miss; and a
path-traversal attempt. [`eco_test.go`](go/internal/eco/eco_test.go) has a minimal
end-to-end example in ~50 lines.

### Step 4 — the client-side tables that are *not* descriptor-driven

The server derives everything from the descriptor. The client-facing helpers do not, and
this is where a new ecosystem gets half-added. Grep for an existing ID before you
declare victory:

```bash
cd go && grep -rn '"npm"' --include='*.go' . | grep -v _test
```

The ones that matter:

| file | what it holds | needed for Maven? |
|---|---|---|
| [internal/session/session.go](go/internal/session/session.go) | the env vars `pkgcache shell` / `run` inject | yes, if Maven can be pointed by env (`MAVEN_OPTS`) or a generated `settings.xml` |
| [internal/local/persist.go](go/internal/local/persist.go) | the dotfiles `pkgcache setup` writes (`.npmrc`, `pip.conf`) | yes — `~/.m2/settings.xml` |
| [internal/local/team.go](go/internal/local/team.go) | `chainedEcosystems`: which ecosystems can front a team cache | yes — Maven composes origin + path, so it chains |
| [internal/clientbridge/bridge.go](go/internal/clientbridge/bridge.go) | reaching the cache from inside a container | yes, for Maven builds in Docker |
| [internal/onboarding/scripts.go](go/internal/onboarding/scripts.go) | the generated sh/PowerShell onboarding scripts | yes |
| [internal/obs/metrics.go](go/internal/obs/metrics.go) | `Ecosystems`, the label vocabulary pre-creating series | yes — one string; without it dashboards read "no data" |
| [internal/lockwarm/](go/internal/lockwarm/) | lock-file warming (uv.lock, package-lock.json) | optional — a separate feature, not required by the adapter |

`obs.Ecosystems` is a deliberate duplicate (importing the registry there would be a
dependency cycle), so it is the one list you must update by hand and the one most easily
missed.

### Step 5 — verify

```bash
cd go
make build
make test          # unit + integration
make race          # mandatory before merge
make lint
```

The acceptance suite drives real clients against a live instance and needs Docker and a
Python toolchain; see [docs/running-and-testing.md](docs/running-and-testing.md). To
drive the services by hand outside Docker, the repo ships a `verify` skill at
[.claude/skills/verify](.claude/skills/verify).

Then confirm the descriptor actually propagated — this is the payoff for keeping the
invariant:

```bash
curl -k https://localhost:8443/api/v1/ecosystems     # maven listed, with its setup steps
```

---

## Things that will bite you

- **Two ecosystems, one blob.** Content is deduplicated globally by digest. A cache key
  is `(project, eco, key)`; the blob underneath may be shared. Never delete a blob
  directly — `DeleteEntry` removes the key, and the garbage collector is the only thing
  that knows whether anything else still references the bytes.
- **Raw vs unescaped captures.** `p.Get` returns the segment exactly as the client wrote
  it; `p.Unescape` decodes. Choosing wrong is silently correct for ASCII names and wrong
  for scopes, `+`, and relayed proxy URLs.
- **Offline is a first-class mode.** Any new code path that fetches must degrade to a
  cache-only answer. `c.Offline()` reports it, and `c.WriteError` already says the right
  thing on a miss — route errors through it rather than writing your own 404.
- **Registering a route with a bad pattern panics at startup.** That is intended: a
  route that never matches is worse than a process that refuses to start.
- **The console is checked-in, embedded source.** No bundler, no Node in the build. A
  descriptor-driven ecosystem needs no console change at all; if you think you need one,
  re-read the descriptor.
- **Config is an atomically swapped snapshot.** Read it through the `Ctx` helpers
  (`Upstreams`, `UpstreamChain`, `Offline`) — never cache a snapshot across requests.

## Commits

`type(scope): what changed`, lowercase, describing the effect rather than the mechanism —
`fix(ci): the apt repository was never rebuilt after a release`. Types in use: `feat`,
`fix`, `docs`, `refactor`. Run `git log` before writing one.
