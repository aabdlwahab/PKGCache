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

Your Dockerfile is not modified. It is rewritten in memory and handed to Docker on
standard input — `docker build -f -`, and `dockerfile_inline` for each service under
`pkgcache compose`. Nothing is generated on disk, so a `COPY .` cannot pick it up and a
Docker client that cannot see this process's files still builds: the snap has a private
`/tmp`, and a rootless daemon has its own mount namespace. (`dockerfile_inline` wants
Compose v2.17 or newer.)

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

`FROM` lines are repointed for any registry the reference names — Docker Hub, ghcr,
quay, `nvcr.io`, `gcr.io`, `public.ecr.aws` and the rest — because the cache discovers a
registry from the path rather than from a list somebody maintains. What is left alone is
a registry only this machine can reach: `localhost:5000`, an address, a `host:port` on
the build network. Those mean something different on the machine the cache runs on, so
they are not the cache's to redirect. Every substitution is printed, because a tool that
silently alters what gets built is a tool people stop trusting.

## Three ways to reach the cache

Which one applies depends entirely on whether the Docker daemon can see your loopback
interface, and that is worked out rather than asked: there is no native daemon on macOS or
Windows, so the gateway is always the answer there, and on Linux the only daemon that
needs it is Docker Desktop, which `docker info` names. The choice is printed when it is
made.

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
why this step exists at all.

Which address gets used is not a question you have to answer. `pkgcache build` and
`pkgcache pull` derive it: on macOS and Windows there is no native Docker daemon at all —
Docker Desktop, Colima, Rancher and OrbStack are virtual machines with their own
loopback — so the gateway is always right there, and on Linux the only daemon that needs
it is Docker Desktop, which `docker info` names. The choice is printed when it is made.
`-host-address` and `-host-address=false` force either one.

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
docker pull 127.0.0.1:41780/nvcr.io/nvidia/pytorch:24.01
```

The first path segment selects the registry, and any registry host works there without
being configured first: write `nvcr.io`, `gcr.io`, `mcr.microsoft.com` and the cache
fetches from it. `dockerhub`, `ghcr` and `quay` are aliases that predate that and keep
working, and `docker.io` is the same namespace as `dockerhub` rather than a second copy
of it. Official Docker Hub images live under `library/`.

A discovered registry is reached over HTTPS on the host the segment names. That is
bounded on purpose: with no configuration the cache reaches public registries only — a
dotted DNS name, no port — so a path segment cannot make it fetch from an address only
the server can route to. `server.registry_allowlist` narrows that to a list you name, and
is also how a private registry (`registry.internal:5000`) is admitted; `["*"]` means
anywhere. A registry needing credentials is still configured as an upstream, which also
takes precedence over discovery.

`pkgcache pull <image>` does the same thing from the command line and puts the name back,
so the image ends up called what you asked for rather than what it was fetched through:

```sh
pkgcache pull prom/prometheus          # named prom/prometheus afterwards
```

`pkgcache docker-setup -mirror` is the other approach: it registers the cache as a Docker
Hub mirror so an unmodified `docker pull python:3.12` comes from it with no wrapper. It is
off by default because it reroutes every pull on the machine — and it needs a cache that
answers unprefixed `/v2/` paths, which is `server.registry_mirror` on a pkgreg server. A
`pkgcache` on a laptop does not set that, so the daemon asks, gets a 404 and falls back to
Docker Hub. Use `pkgcache pull` there.

## Anything that shells out to docker

`pkgcache-docker` is docker with `build` and `pull` served from the cache. It exists for
tools that run a container command and let you choose which one:

```sh
crate prepare -c manifest.yaml --runtime pkgcache-docker
```

Three verbs, and only two are interesting. `build` rewrites the Dockerfile in memory —
exactly `pkgcache build`. `pull` fetches through the cache and tags the image back to the
name the manifest gives it — exactly `pkgcache pull`. Everything else (`run`, `save`,
`load`, `image inspect`, `compose`) is handed to docker untouched and is not slowed down
by this program existing.

Nothing in it knows about crate, so anything that runs `<command> build` and
`<command> pull` gets the same benefit. The alternative — teaching the orchestrator about
this cache — would be two products on two release cycles pretending to be one.

It picks the cache's address the same way `pkgcache build` does, which matters more here
because the caller is another program's `--runtime` setting and there is no flag to add.
`PKGCACHE_HOST_ADDRESS=1` or `=0` overrides that decision, and `PKGCACHE_DOCKER` puts the
shim in front of podman or nerdctl instead.

There is also `pkgcache crate -- prepare -c manifest.yaml`, which wraps the environment
rather than the runtime: it gives crate a Docker configuration that sends apt and apk
through the cache, and leaves image pulls alone. `--runtime pkgcache-docker` is the fuller
integration of the two — it covers pulls as well, and on native Linux it is the one that
works, because the wrapper's build proxy names an address a build container cannot resolve
without the `--add-host` that `pkgcache build` adds and crate's own `docker build` does
not.

Pulls chain to a team cache when one is configured — see
[pkgcache.md](pkgcache.md#three-tiers) for the path shapes, which are not like the other
ecosystems'.

## When a build still misses the cache

- `pkgcache status` — is the cache running, and is the team cache reachable
- the printed substitutions — a `FROM` that was not repointed names a registry the cache
  does not front
- `docker build --progress=plain` — the declared `ARG` lines are visible in the output
- an `ENV` in your own Dockerfile overrides the `ARG`, and yours wins
