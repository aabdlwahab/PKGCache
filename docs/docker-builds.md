# Using the Docker cache in builds

The `docker-cache` service is **[zot](https://zotregistry.dev)**, a multi-upstream
pull-through cache on port **5000**:

- **Online** (`--profile online`) it syncs **on demand** from `docker.io`,
  `ghcr.io` and `quay.io`, storing each image as an OCI layout under
  `caches/docker/`.
- **Air-gapped** (`--profile offline`) the same layouts are served by a
  sync-disabled zot — a plain registry with **no upstream**.

Unlike the old Docker-Hub-only registry mirror, zot caches **any** registry — the
trade-off is that you reference images **through** zot by prefixing a per-upstream
destination, so there's no transparent `daemon.json` `registry-mirrors` mode.

All client URLs are **HTTPS**: a Caddy reverse proxy (the `tls-proxy` service)
terminates TLS on `:5000` / `:4873` / `:3141` using the cert from
`scripts/gen-certs.sh`, then forwards to the backends internally. So clients get a
trusted connection with no per-tool insecure flags — after a one-time CA install,
see [Trusting the cache](#trusting-the-cache). (apt-cacher-ng on `:3142` stays
plain HTTP — it's a proxy, not a server; details in its section below.)

Throughout, `CACHE_HOST` = the machine running the cache (`localhost` if it's the
same box, otherwise its IP/hostname).

## How to reference an image

Pull `CACHE_HOST:5000/<destination>/<upstream-repo>:<tag>`, where `<destination>`
selects the upstream:

| Upstream registry | Pull through zot as                          |
| ----------------- | -------------------------------------------- |
| `docker.io`       | `CACHE_HOST:5000/dockerhub/<repo>`           |
| `ghcr.io`         | `CACHE_HOST:5000/ghcr/<repo>`                |
| `quay.io`         | `CACHE_HOST:5000/quay/<repo>`                |

```dockerfile
# docker.io/library/ubuntu:22.04
FROM CACHE_HOST:5000/dockerhub/library/ubuntu:22.04

# docker.io/verdaccio/verdaccio:6   (user/org images keep their namespace)
FROM CACHE_HOST:5000/dockerhub/verdaccio/verdaccio:6

# ghcr.io/astral-sh/uv:python3.12-bookworm-slim
FROM CACHE_HOST:5000/ghcr/astral-sh/uv:python3.12-bookworm-slim
```

Official Docker Hub images live under `library/`, so Hub's `ubuntu` is
`dockerhub/library/ubuntu`. To cache another registry (e.g. `gcr.io`,
`registry.k8s.io`), add a `sync` entry with a new `destination` in
[`config/zot-config.json`](../config/zot-config.json).

The first pull of an image populates the cache from upstream (online); every later
pull — and every pull on the air-gapped side — is served from `caches/docker/`.

## Trusting the cache

The cache serves HTTPS with a cert from a **private CA** (`scripts/gen-certs.sh`
mints `certs/ca.crt` + `certs/server.crt`). A client trusts it once it trusts that
CA — so **copy `certs/ca.crt` to each build host once** and install it. After that
there are no insecure flags anywhere. The CA cert is safe to share and must travel
across the air gap; the `.key` files never leave the cache host.

- **System trust store → docker, apt, apk** trust the cache with nothing further:

  ```bash
  sudo cp ca.crt /usr/local/share/ca-certificates/package-cache.crt  # Debian/Ubuntu
  sudo update-ca-certificates
  # RHEL: /etc/pki/ca-trust/source/anchors/ + sudo update-ca-trust
  ```

  Docker reads the system store; restart the daemon after installing. (No-restart
  alternative: drop the CA at `/etc/docker/certs.d/CACHE_HOST:5000/ca.crt`.)

- **`docker buildx` with a `docker-container` driver** (its own BuildKit, ignores
  the host store) — point it at the CA in `buildkitd.toml`:

  ```toml
  [registry."CACHE_HOST:5000"]
    ca = ["/etc/buildkit/ca.crt"]
  ```

  ```bash
  docker buildx create --use --name cached --config buildkitd.toml
  ```

- **pip and npm do not use the system store** — they're pointed at the CA in their
  own sections below (`PIP_CERT` / npm `cafile`).

> Self-signed-CA, not zero-config: "automatic trust" means automatic *after* the
> one-time CA install per host. A publicly-trusted cert would need a real domain +
> reachability, which an air-gapped/`localhost`/bare-IP cache can't have.

## One Dockerfile for both online and air-gapped

The refs are identical on both sides (zot serves the same path whether syncing or
serving from storage), so nothing changes across the gap. To keep upstream refs
swappable, parameterize the prefix:

```dockerfile
ARG REGISTRY=CACHE_HOST:5000/dockerhub
FROM ${REGISTRY}/library/ubuntu:22.04
```

```bash
# through the cache:
docker build --build-arg REGISTRY=CACHE_HOST:5000/dockerhub -t myapp .
# straight from Docker Hub (no cache):
docker build --build-arg REGISTRY=docker.io -t myapp .
```

## Verify it's actually using the cache

```bash
# Repos zot has stored (namespaced by destination):
curl -s --cacert certs/ca.crt https://CACHE_HOST:5000/v2/_catalog

# Tags cached for one repo:
curl -s --cacert certs/ca.crt https://CACHE_HOST:5000/v2/dockerhub/library/ubuntu/tags/list
```

On the online side, pull an image once, then re-run with upstream blocked — it
should still serve from `caches/docker/`. After new images land, snapshot them
with `python3 scripts/pkgops.py checkpoint "added <image>"`.

---

# Using the pip / npm / apt caches in builds

The base image comes through the Docker mirror above; the **packages installed
inside** the build (`pip install`, `npm install`, `apt-get install`) come from
the other three proxies:

| Ecosystem      | Proxy endpoint                                          |
| -------------- | ------------------------------------------------------- |
| pip (PyPI)     | `https://CACHE_HOST:3141/root/pypi/+simple/`            |
| pip (PyTorch)  | `https://CACHE_HOST:3141/root/pytorch-cu124/+simple/`   |
|                | `https://CACHE_HOST:3141/root/pytorch-cpu/+simple/`     |
| npm            | `https://CACHE_HOST:4873/`                              |
| apt            | `http://CACHE_HOST:3142/`                               |
| apk            | `http://CACHE_HOST:3142/` (via apt-cacher-ng)           |

The two `pytorch-*` indexes are extra devpi **mirror** indexes (alongside the
default `root/pypi`) that mirror `download.pytorch.org/whl/{cu124,cpu}`, where
`torch` and the `nvidia-*` CUDA wheels actually live — see
[Extra indexes (PyTorch)](#extra-indexes-pytorch) below. They're created
automatically by the devpi container on the online side and carried across the gap
in the cache.

Unlike the Docker mirror (daemon config), these are pointed at **per-build**, via
build args / env / config files inside the Dockerfile. **pip and npm are HTTPS**
(behind the same `tls-proxy`) and need the CA — see [Trusting the cache](#trusting-the-cache);
in a build that means copying `ca.crt` in and pointing the tool at it (shown
below). **apt/apk stay HTTP** (apt-cacher-ng is a proxy, not a TLS server).

## Networking: the build must reach the proxy

A build runs in its own network namespace, so `localhost` inside the build is
**not** the host. The proxy has to be reachable from *inside* the build:

- **Same host as the proxies** — build with host networking so `localhost:PORT`
  resolves to the proxies:

  ```bash
  docker build --network=host -t myapp .          # classic builder
  docker buildx build --network=host -t myapp .   # buildx (docker-container driver)
  ```

  Then use `CACHE_HOST=localhost` (or `127.0.0.1`) below.

- **Proxy on another machine** — use its IP/hostname as `CACHE_HOST` and skip
  `--network=host` (the default bridge network can route to it).

pip and npm reach the cache over **HTTPS**, so each `COPY`s the CA in and points
the tool at it (no `--trusted-host` / `strict-ssl=false` — those disabled TLS;
the CA *trusts* it instead). apt/apk talk plain HTTP to the proxy.

---

## pip / PyPI (devpi)

Point pip at the devpi simple index over HTTPS. pip ignores the system trust
store, so `COPY` the CA in and set `PIP_CERT` at it; `PIP_INDEX_URL` + `PIP_CERT`
as env vars apply to every `pip install` in the build:

```dockerfile
ARG CACHE_HOST=localhost
COPY ca.crt /usr/local/share/ca-certificates/package-cache.crt
ENV PIP_INDEX_URL=https://${CACHE_HOST}:3141/root/pypi/+simple/ \
    PIP_CERT=/usr/local/share/ca-certificates/package-cache.crt

RUN pip install --no-cache-dir requests numpy
```

```bash
# run from a context that has ca.crt (e.g. cp certs/ca.crt next to the Dockerfile)
docker build --network=host --build-arg CACHE_HOST=localhost -t myapp .
```

`PIP_CERT` points pip at the private CA so it *trusts* the HTTPS index (replaces
the old `--trusted-host`, which instead disabled the check). Per-command form:
`pip install --index-url https://CACHE_HOST:3141/root/pypi/+simple/ --cert ca.crt ...`.

### uv (lockfile-aware)

`uv` hits the same devpi index, but **ignores pip's env vars** — it has its own,
and a `uv.lock` adds a sharp edge worth knowing.

```dockerfile
FROM CACHE_HOST:5000/ghcr/astral-sh/uv:python3.12-bookworm-slim
ARG CACHE_HOST=localhost
COPY ca.crt /usr/local/share/ca-certificates/package-cache.crt
ENV UV_INDEX_URL=https://${CACHE_HOST}:3141/root/pypi/+simple/ \
    SSL_CERT_FILE=/usr/local/share/ca-certificates/package-cache.crt
COPY pyproject.toml uv.lock ./
RUN uv sync --locked          # NOT --frozen (see below)
```

- **Index:** `UV_INDEX_URL` (or `UV_DEFAULT_INDEX`) — pip's `PIP_INDEX_URL` does
  nothing for uv.
- **CA:** `SSL_CERT_FILE` pointed at the CA (uv uses bundled Mozilla roots, not
  the system store, so `PIP_CERT` doesn't apply). `UV_NATIVE_TLS=true` is the
  alternative — it makes uv use the system store, where an installed `ca.crt`
  already lives.
- **Use `uv sync --locked`, not `--frozen`.** `--frozen` installs from the URLs
  hardcoded in `uv.lock` and [ignores `UV_INDEX_URL`](https://github.com/astral-sh/uv/issues/19625),
  so it bypasses the cache and reaches `pythonhosted.org` directly — which defeats
  caching online and **fails on the air-gapped side**. Plain `uv sync` / `--locked`
  respect the configured index. `--locked` also asserts the lock matches
  `pyproject.toml` (errors on drift) — reproducible *and* cached.

The lock's sha256 hashes still validate: devpi serves byte-identical PyPI files,
so the cache is integrity-transparent. Lock against **plain PyPI** (keeps `uv.lock`
portable — uv otherwise tends to rewrite each package's recorded `source.registry`
to your mirror) and install *through* the cache via `UV_INDEX_URL`; the same lock
resolves identically whether the bytes come from PyPI or devpi.

#### Keeping uv.lock clean

A tarnished lock (devpi URLs baked into `source.registry`) commits silently,
travels across the gap, and then breaks for anyone without your devpi. uv writes
the **resolve-time** index into the lock, so the discipline is to separate
lock-time from install-time:

| Step                      | Index                      | Where it runs              |
| ------------------------- | -------------------------- | -------------------------- |
| `uv lock` / `uv add`      | **PyPI** (the default)     | online / dev host          |
| `uv sync` (install)       | **devpi**, via `UV_INDEX_URL` env only | build / air-gapped host |

Three rules keep it clean:

1. **Generate the lock with nothing pointing at devpi** — don't set `UV_INDEX_URL`,
   and never add devpi as a `[[tool.uv.index]]` in `pyproject.toml` (that's a lock
   input and *does* get baked in). Plain `uv lock` records `pypi.org`.
2. **Point at devpi only via the environment at install time** (as in the
   Dockerfile above). The env override is not written into the lock.
3. **Never `uv lock` / `uv add` / re-resolving `uv sync` while `UV_INDEX_URL`
   points at devpi.** To add a dependency, unset the override first.

`uv sync --locked` is the seatbelt: it refuses to modify the lock, erroring out
instead of silently rewriting sources to the mirror. Two cheap CI guards catch a
tarnish either way:

```bash
# the lock must never reference the mirror:
! grep -qE ':3141|CACHE_HOST|/root/pypi/' uv.lock
# a sync must not have modified the lock:
uv sync --locked && git diff --exit-code uv.lock
```

Already tarnished? Regenerate cleanly (online, no override):

```bash
unset UV_INDEX_URL
uv lock --upgrade        # or: rm uv.lock && uv lock — rewrites source.registry back to pypi.org
grep -c pypi.org uv.lock # sanity: sources point at PyPI again
```

> Whether the env-level `UV_INDEX_URL` trips the `--locked` freshness check (vs.
> only `pyproject.toml` index config doing so) has shifted across uv versions. If
> `uv sync --locked` errors purely from the env override on your version, use plain
> `uv sync` plus the `git diff --exit-code uv.lock` guard for the same protection.

### Extra indexes (PyTorch)

`UV_INDEX_URL` only redirects the **default** index. A project that pulls `torch`
from PyTorch's wheel channel does it through a **named, explicit** index pinned in
`pyproject.toml` — the default-index override doesn't touch it:

```toml
[tool.uv.sources]
torch = [
    { index = "pytorch-cpu",  extra = "cpu" },
    { index = "pytorch-cuda", extra = "gpu" },
]
[[tool.uv.index]]
name = "pytorch-cuda"
url = "https://download.pytorch.org/whl/cu124"
explicit = true          # torch resolves ONLY from here, never PyPI
```

So `torch` (and the `nvidia-*` CUDA wheels it drags in) is locked to
`download.pytorch.org` — and its files actually download from a second host,
`download-r2.pytorch.org`. Neither is `root/pypi`, so the default devpi mirror
never sees them. The devpi container mirrors both wheel channels as their own
indexes (`root/pytorch-cu124`, `root/pytorch-cpu`); on a fetch, **devpi follows
the R2 redirect and re-serves the file from its own host**, so the air-gapped side
needs only devpi, not those two upstream hosts.

Point every index at the mirror the same way — at install time only, via env, so
the lock stays pinned to the portable upstream URLs. uv merges indexes **by name**,
so an index supplied via `UV_INDEX` overrides the same-named one in
`pyproject.toml` (and the implicit `pypi`) without editing the file or touching the
lock. Prefer this name-override form even for PyPI: it's verified `--locked`-safe,
whereas `UV_INDEX_URL`'s effect on the `--locked` freshness check has shifted
across uv versions (see the caveat above). One unified, consistent block:

```dockerfile
FROM CACHE_HOST:5000/ghcr/astral-sh/uv:python3.12-bookworm-slim
ARG CACHE_HOST=localhost
COPY ca.crt /usr/local/share/ca-certificates/package-cache.crt
RUN update-ca-certificates
ENV UV_NATIVE_TLS=true \
    UV_INDEX="pypi=https://${CACHE_HOST}:3141/root/pypi/+simple/ \
pytorch-cpu=https://${CACHE_HOST}:3141/root/pytorch-cpu/+simple/ \
pytorch-cuda=https://${CACHE_HOST}:3141/root/pytorch-cu124/+simple/"
COPY pyproject.toml uv.lock ./
RUN uv sync --extra gpu --locked       # NOT --frozen
```

- `UV_INDEX` overrides three indexes by name: implicit `pypi` → devpi `root/pypi`
  for everything off PyPI, and the named `pytorch-cpu` / `pytorch-cuda` →
  devpi `root/pytorch-cpu` / `root/pytorch-cu124`. (uv ignores the index whose
  extra isn't selected, so shipping all three is fine — `--extra gpu` uses
  `pytorch-cuda`, `--extra cpu` uses `pytorch-cpu`.)
- **CA:** `UV_NATIVE_TLS=true` + the system-store install (`update-ca-certificates`)
  is the cleanest single switch when you're overriding several indexes; the
  `SSL_CERT_FILE=…/ca.crt` form from the PyPI section works too.
- Same rules as the PyPI case: `--locked` not `--frozen` (`--frozen` ignores the
  overrides and hits `download.pytorch.org` / `download-r2.pytorch.org` directly —
  fails air-gapped); and **don't** rewrite the index URLs in `pyproject.toml`
  itself (that's a lock input — it bakes the mirror into `source.registry` and
  tarnishes the lock, same as adding devpi there). Keep the override in the
  environment. The CI guards above also catch a `download.pytorch.org` → mirror
  rewrite if you extend the grep:
  `! grep -qE ':3141|CACHE_HOST|/root/(pypi|pytorch)' uv.lock`.

> First online `uv sync` of a CUDA build pulls multi-GB wheels (torch + the full
> `nvidia-*` set) through devpi — expect `caches/pip` to grow accordingly, and give
> the first run time before checkpointing.

## npm (Verdaccio)

Point npm at Verdaccio over HTTPS and give it the CA via `NODE_EXTRA_CA_CERTS`
(npm/node ignore the system store). Env-only variant:

```dockerfile
ARG CACHE_HOST=localhost
COPY ca.crt /usr/local/share/ca-certificates/package-cache.crt
ENV NPM_CONFIG_REGISTRY=https://${CACHE_HOST}:4873/
ENV NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/package-cache.crt
RUN npm install
```

```bash
# run from a context that has ca.crt (e.g. cp certs/ca.crt next to the Dockerfile)
docker build --network=host --build-arg CACHE_HOST=localhost -t myapp .
```

`NODE_EXTRA_CA_CERTS` makes npm *trust* the HTTPS registry (replaces the old
`strict-ssl=false`, which disabled the check). `.npmrc` equivalent: a
`cafile=/...package-cache.crt` line alongside `registry=https://...`. Verdaccio
proxies scoped (`@scope/pkg`) and unscoped packages alike — no per-scope lines.

## apt (apt-cacher-ng)

apt-cacher-ng stays on **plain HTTP** (`:3142`), *not* behind the `tls-proxy`: it's
a forward proxy apt connects *through*, and apt/apk have no supported way to reach
a proxy over TLS. There's no cert to trust here and never was an insecure flag —
proxy config isn't a TLS override. (Package integrity still comes from apt's own
signed `Release`/`Packages` and apk's signed `APKINDEX`, independent of transport.)

apt-cacher-ng is a **caching HTTP proxy**, not a rewritten mirror — keep the
normal `deb http://...` sources and route apt through the proxy:

```dockerfile
ARG CACHE_HOST=localhost
RUN echo "Acquire::http::Proxy \"http://${CACHE_HOST}:3142\";" \
      > /etc/apt/apt.conf.d/01proxy

RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*
```

```bash
docker build --network=host --build-arg CACHE_HOST=localhost -t myapp .
```

Gotcha: apt-cacher-ng only caches **HTTP** repos. An `https://` source is
tunneled (CONNECT) and passes through uncached. If a repo is HTTPS-only, rewrite
its source line to go *through* the proxy as a path instead, e.g.
`deb http://CACHE_HOST:3142/HTTPS///download.example.com/repo ...`. The Debian/
Ubuntu base repos are reachable over HTTP, so the proxy line above caches them as
written.

## apk (Alpine, via apt-cacher-ng)

There's no dedicated apk proxy — apk honors `http_proxy`, so it routes through the
same apt-cacher-ng on `:3142`. Two things are required, and both matter:

1. **Force the Alpine repos to HTTP.** apt-cacher-ng can only cache plain HTTP; an
   `https://` repo is tunneled (CONNECT) and passes through uncached. Alpine base
   images often ship `https://` mirror lines, so rewrite them.
2. **Set `http_proxy` for the `apk` step only** — inline on the `RUN`, so the
   proxy isn't baked into the runtime image's environment.

```dockerfile
# FROM alpine:3.20  (base image still comes through the Docker mirror)
ARG CACHE_HOST=localhost
RUN sed -i 's|https://|http://|g' /etc/apk/repositories \
 && http_proxy=http://${CACHE_HOST}:3142 \
    apk add --no-cache curl
```

```bash
docker build --network=host --build-arg CACHE_HOST=localhost -t myapp .
```

Use `--no-cache` so apk doesn't also persist its index into the image — the
download still goes through the proxy either way.

> **Caching is already wired in.** apt-cacher-ng's default file patterns are
> Debian-oriented and wouldn't cache `.apk` files on their own, so the `apt-cache`
> service mounts [`config/apt-cacher-ng-apk.conf`](../config/apt-cacher-ng-apk.conf),
> which extends the patterns (`PfilePatternEx`/`VfilePatternEx`) to treat `.apk`
> packages as precious and `APKINDEX.tar.gz` as volatile. Nothing to do per-build;
> it's noted here so you know where apk caching comes from.

---

## Air-gapped side

Nothing in the Dockerfiles changes — same endpoints, same build args. The
proxies just serve from cache instead of fetching:

- **pip** — runs with `DEVPI_OFFLINE=1`, serves cached files, fails on a miss.
- **npm** — Verdaccio serves cached tarballs; uplink is simply unreachable.
- **apt** — serve-only; a miss fails (no upstream), which is expected.
- **apk** — same apt-cacher-ng, serve-only; a miss for an uncached `.apk` fails,
  same as apt.

So a build that succeeded online — populating all four caches — reproduces
air-gapped as long as every package it needs was checkpointed across. If a build
fails on a miss air-gapped, the fix is to add that package on the online side and
re-export, not to change the build.
