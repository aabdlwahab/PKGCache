# Docker builds through the cache

A `docker build` is the hardest thing to point at a cache, because the build does not
run in your shell. It runs in the daemon, which never sees your environment, and on
Docker Desktop it runs inside a virtual machine whose loopback is not yours. Every layer
of this page exists because of that one fact.

The short version:

```sh
pkgcache build -t myapp .
pkgcache compose up --build
```

Your Dockerfile is not modified. It is rewritten in memory, and the generated file is
written outside the build context so a `COPY .` cannot pick it up.

## What the rewrite does

Deliberately little: **declare build arguments, and repoint `FROM`.** It never adds a
`RUN`, never reorders, never removes. Anything larger would produce a file a reader
could not predict from the original, and a build nobody can predict is worse than a flag
somebody has to remember.

The arguments it declares are the ones the tools already read:

| tool | argument |
|---|---|
| pip | `PIP_INDEX_URL` |
| uv | `UV_DEFAULT_INDEX` |
| npm | `NPM_CONFIG_REGISTRY` |
| git | `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n` |
| apt, apk | `http_proxy`, `no_proxy` |

They are `ARG`, never `ENV`. An `ENV` is written into the shipped image, and an image
whose `PIP_INDEX_URL` points at somebody's laptop is a broken image the moment it leaves
that laptop.

`FROM` lines are repointed only for registries this cache fronts — Docker Hub, ghcr and
quay. A registry it does not front is not its to redirect, and is left alone. Every
substitution is printed, because a tool that silently alters what gets built is a tool
people stop trusting.

## Three ways to reach the cache, and how to pick

The right one depends entirely on whether the daemon can see your loopback interface.

**Bridge** — tools point at the client's loopback bridge over plain HTTP. Nothing needs
a certificate. Requires a Linux daemon sharing this machine's network namespace. This is
the default when it applies.

**Host gateway** — tools reach the cache at `host.docker.internal`. This is what
`pkgcache` uses on Docker Desktop, where loopback inside the build is the build's own.
The cache serves plain HTTP, so there is no CA to mount, but the daemon must be told
that plain-HTTP registry is acceptable:

```sh
pkgcache docker-setup            # once per machine
```

Loopback is exempt from that rule by default; `host.docker.internal` is not, which is
why this step exists at all. Add `-host-address` to `pkgcache build` on Docker Desktop,
a remote daemon, or CI.

**Cache address** — tools point at a `pkgreg` server's own HTTPS address, and its CA is
mounted into each `RUN` that needs it as a BuildKit secret under `/run/secrets`. This is
what `pkgreg-client` uses against a shared server; `pkgcache` serves plain HTTP and has
no HTTPS address of its own, so it does not offer this. (`pkgcache build -cache-address`
is a deprecated alias for `-host-address`, kept so old scripts keep working.)

## The plain-HTTP trap

pip refuses a plain-HTTP index unless it is loopback, and reports the refusal as
`No matching distribution found` — which reads as a missing package, not a rejected
index. When the base is `http://` and not loopback, the rewrite also declares:

```
ARG PIP_TRUSTED_HOST=<host>
ARG UV_INSECURE_HOST=<host>
```

Without them a `host.docker.internal` build fails with a message that sends you looking
for the wrong problem entirely.

## Caching apt in a build somebody else wrote

`pkgcache build` rewrites the Dockerfile it is given. It cannot reach a build invoked by
a colleague's Makefile or a CI script that calls `docker build` directly. For those:

```sh
pkgcache docker-build-setup      # once per machine
```

That sets the build proxy in your Docker **client** configuration. `HTTP_PROXY` and its
relatives are predefined build arguments, injected into every `RUN` with no `ARG` line
to declare them, so this is the only channel into a build whose file you do not control.

It covers apt and apk and nothing else: pip, uv and npm speak HTTPS to their upstreams,
which through a proxy means a `CONNECT` tunnel this proxy does not offer. Because it
applies to every build on the machine, the cache has to be running for those builds to
work — `pkgcache persist` keeps it up.

## Pulling images

Images are pulled from the cache by name:

```sh
docker pull 127.0.0.1:41780/dockerhub/library/alpine:3.20
docker pull 127.0.0.1:41780/ghcr/some-org/some-image:v1
```

The first path segment selects the registry — `dockerhub`, `ghcr` or `quay`. Official
Docker Hub images live under `library/`.

With `pkgcache docker-setup -mirror`, an unmodified `docker pull python:3.12` is served
from the cache with no rewrite at all. That is off by default: it reroutes every pull on
the machine, which is not something a setup command should do to you quietly.

Pulls chain to a team cache when one is configured — see
[pkgcache.md](pkgcache.md#three-tiers) for the path shapes, which are not like the other
ecosystems'.

## When a build still misses the cache

- `pkgcache status` — is the cache running, and is the team cache reachable
- the printed substitutions — a `FROM` that was not repointed names a registry the cache
  does not front
- `docker build --progress=plain` — the declared `ARG` lines are visible in the output
- an `ENV` in your own Dockerfile overrides the `ARG`, and yours wins
