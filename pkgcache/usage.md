# pkgcache — usage

One Python codebase, **one container**, that runs four pull-through package caches —
**OCI/Docker, npm, PyPI (pip/uv), and apt+apk** — each on its own port in a single
process, with a native SQLite manifest ledger and a live download-progress feed. The
three HTTPS roles terminate TLS in-process (no separate proxy); apt/apk is a plain-HTTP
forward proxy. (The protocols can't share a port: OCI owns `/v2/` at the root and apt
is a forward proxy — hence four ports, one process.)

- Online: each role caches what it fetches from upstream on first request.
- Offline (`OFFLINE=1`): each role serves only from cache; misses fail (the air gap).

---

## 1. Prerequisites

```bash
cd package-registry
./scripts/gen-certs.sh          # mint the private CA + server cert (once)
                                # pass extra hostnames/IPs as args if needed
docker compose build            # build the package-registry/pkgcache:local image
```

`gen-certs.sh` writes `certs/ca.crt` (share this with clients) and `certs/server.*`
(the container terminates TLS with these in-process; keys stay out of git). Pass any
extra hostnames/IPs clients will use as args to `gen-certs.sh` so they're in the cert.

---

## 2. Bring it up

```bash
# Online side (caches on demand from upstream):
docker compose --profile online up -d

# Air-gapped side (serve from cache only, never reach upstream):
OFFLINE=1 docker compose --profile offline up -d

# Add the control UI on either side (its own profile):
docker compose --profile online --profile ui up -d     # http://<HOST>:8088
```

> **`OFFLINE` is an env flag, not a separate service.** The docker cache is a single
> service for both sides; set `OFFLINE=1` when bringing up the offline profile.

---

## 3. Ports & URLs

All four ports are served by the **one `pkgcache` container** (`webui` is a separate
optional container).

| Role | Port | Proto | Client URL (`HOST` = cache host) |
|------|------|-------|----------------------------------|
| OCI / Docker | 5000 | HTTPS | `HOST:5000/<dest>/<image>` — `<dest>` ∈ `dockerhub` \| `ghcr` \| `quay` |
| npm | 4873 | HTTPS | `https://HOST:4873/` |
| pip / PyPI | 3141 | HTTPS | `https://HOST:3141/<index>/+simple/` — `<index>` ∈ `root/pypi` \| `root/pytorch-cu124` \| `root/pytorch-cpu` |
| apt / apk | 3142 | **HTTP** | `http://HOST:3142` (forward proxy — no TLS) |
| Web UI | 8088 | HTTP | `http://HOST:8088` (`ui` profile only) |

The HTTPS ports terminate TLS in-process using your private CA. apt-cacher-style
forward proxying can't be fronted with TLS, so apt/apk stay plain HTTP.

---

## 4. Trust the CA on each client (once)

The HTTPS roles use the private CA, so each client must trust `certs/ca.crt`.

| Client | How to trust `ca.crt` |
|--------|------------------------|
| **docker** | `sudo cp certs/ca.crt /etc/docker/certs.d/HOST:5000/ca.crt` (dir name includes the port) |
| **pip / uv** | `--cert /path/ca.crt`, or `export PIP_CERT=/path/ca.crt` (they ignore the system store) |
| **npm** | `npm config set cafile /path/ca.crt` (or `--cafile`) |
| **apt / apk** | nothing — they use the plain-HTTP proxy on 3142 |

Carry `certs/ca.crt` to the air-gapped side on the shuttle (it's the same CA on both sides).

---

## 5. Per-ecosystem client usage

### Docker / OCI
Official Docker Hub images live under `library/`:
```bash
docker pull HOST:5000/dockerhub/library/alpine:3.20
docker pull HOST:5000/dockerhub/library/ubuntu:24.04
# user/org images keep their namespace:
docker pull HOST:5000/dockerhub/grafana/grafana:11.0.0
# other registries:
docker pull HOST:5000/ghcr/astral-sh/uv:python3.12-bookworm-slim
docker pull HOST:5000/quay/prometheus/prometheus:v2.53.0
```
In a Dockerfile:
```dockerfile
ARG REGISTRY=HOST:5000/dockerhub
FROM ${REGISTRY}/library/python:3.12-slim
```

### pip / uv
```bash
pip install  --index-url https://HOST:3141/root/pypi/+simple/ --cert ca.crt numpy
# PyTorch CUDA wheels (off PyPI):
pip install  --index-url https://HOST:3141/root/pytorch-cu124/+simple/ --cert ca.crt torch
# uv:
UV_INDEX_URL=https://HOST:3141/root/pypi/+simple/ SSL_CERT_FILE=ca.crt uv pip install numpy
```

### npm
```bash
npm install  --registry https://HOST:4873/ --cafile ca.crt <pkg>
# or persist it:
npm config set registry https://HOST:4873/
npm config set cafile /path/ca.crt
```

### apt (in an image or on a host)
```dockerfile
RUN echo 'Acquire::http::Proxy "http://HOST:3142";' > /etc/apt/apt.conf.d/01proxy \
 && apt-get update && apt-get install -y curl
```
Use **http** mirror lines (the proxy is HTTP-only; it does not tunnel HTTPS).

### apk (Alpine)
```dockerfile
RUN sed -i 's/https/http/' /etc/apk/repositories \
 && http_proxy=http://HOST:3142 apk add --no-cache ca-certificates
```

---

## 6. Pre-seeding before the air gap

Cache-on-use fills the cache as real builds run. To deliberately warm a complete set
*before* crossing the gap, list it and pull it through the online proxies:

```bash
cp pkgcache/seed.example.yaml seed.yaml      # edit: images / wheels / npm / apt / apk
CACHE_HOST=HOST CA_CERT=certs/ca.crt ./scripts/prefetch.py seed.yaml
python3 scripts/pkgops.py checkpoint "seeded base images + torch"
```
`prefetch.py` drives the canonical client fetch for each entry through the proxies,
so the cache **and** the ledger populate exactly as a real install would. (Needs
`docker`, `pip`, `npm` on the host; apt/apk entries run in throwaway containers.)

---

## 7. Manifest, ledger & the Web UI

Each proxy records every cached artifact into `caches/<eco>/ledger.db` (SQLite) at
commit time — name, version, sha256 digest, size, origin, timestamp. No filesystem
walking, no re-hashing.

```bash
# Export the git-committed manifest (deterministic subset) from the ledgers:
./scripts/gen_manifest.py
# Repair a drifted ledger by rescanning the cache, then export:
./scripts/gen_manifest.py --rebuild      # needs: pip install ./pkgcache
```

The **Web UI** (`http://HOST:8088`) reads the ledgers live:
- `GET /api/packages?eco=&q=&sort=&page=` — what's cached now (filter/sort/paginate),
- `GET /api/downloads` / `GET /api/recent` — live in-flight downloads and hit/miss feed,
- plus the snapshot/export/import controls over git+dvc.

Each role also exposes its own live progress JSON directly:
`/v2/_progress` (oci), `/-/progress` (npm), `/+progress` (pip), `/acng-progress` (apt)
— add `?sse=1` for a stream — and `/healthz` for health.

---

## 8. Air-gap round trip

```bash
# online host
./scripts/prefetch.py seed.yaml                       # (optional) warm a set
python3 scripts/pkgops.py checkpoint "added X"        # live snapshot: manifest → dvc add → git commit
python3 scripts/pkgops.py export /media/shuttle       # dvc push + git bundle + certs → drive

# air-gapped host
python3 scripts/pkgops.py import /media/shuttle       # git pull + dvc pull + checkout + certs
OFFLINE=1 docker compose --profile offline up -d
```

---

## 9. Configuration

Upstreams live in [pkgcache.yaml](pkgcache.yaml) (baked into the image at
`/etc/pkgcache/pkgcache.yaml`). Edit it and rebuild to change registries/indexes.

Environment overrides (set per service in `docker-compose.yml`):

| Var | Default | Meaning |
|-----|---------|---------|
| `PKGCACHE_ROLE` | *(unset)* | unset = all four roles in one process; set to `oci`/`npm`/`pypi`/`apt` for a single role |
| `OFFLINE` | `0` | `1` = serve from cache only |
| `PKGCACHE_CACHE_ROOT` | `/caches` | base cache dir; each role uses `<root>/<eco>` (compose mounts `./caches` here) |
| `PKGCACHE_TLS_CERT` / `PKGCACHE_TLS_KEY` | `/certs/server.*` | in-process TLS for the HTTPS roles |
| `PKGCACHE_PORT` | per-role | listen port (single-role mode) |
| `PKGCACHE_REQUEST_TIMEOUT` | `1200` | upstream read timeout (s) — generous for multi-GB wheels |
| `PKGCACHE_CONFIG` | `/etc/pkgcache/pkgcache.yaml` | config file path |

### Run without Docker (dev)
```bash
pip install ./pkgcache
# all four roles in one process (HTTP unless you point TLS env at a cert):
PKGCACHE_CACHE_ROOT=./caches PKGCACHE_CONFIG=pkgcache/pkgcache.yaml \
  PKGCACHE_TLS_CERT= PKGCACHE_TLS_KEY= python -m pkgcache
# or a single role:
PKGCACHE_ROLE=pypi PKGCACHE_CACHE_ROOT=./caches/pip \
  PKGCACHE_CONFIG=pkgcache/pkgcache.yaml python -m pkgcache
```

---

## 10. Operational notes

- **One process, four roles, single worker each.** All four roles run in one
  process (four uvicorn servers on one event loop). The progress registry and the
  single-flight de-duplication are in-process, so don't run multiple workers or
  replicas — scale up the host, not the worker count.
- **Byte-faithful cache.** Upstreams are fetched with `Accept-Encoding: identity` so
  cached bytes match the index-declared hashes and `Content-Length` exactly.
- **No garbage collection.** Caches grow unbounded by design; size is managed by DVC
  checkpoint hygiene, not eviction.
- **Open forward proxy.** The apt/apk role proxies to any host (no allowlist) — run
  it only on trusted/isolated networks.
- **Anonymous pulls only.** No upstream credentials; private images are out of scope.
  Docker Hub's anonymous rate limit applies (mitigated by caching each image once +
  prefetch).
- **One-way cutover.** Checkpoints created by the *old* (zot/verdaccio/devpi/
  apt-cacher-ng) stack are not servable by these proxies — re-warm or keep the old
  stack to serve them.

---

## 11. Adding a new ecosystem (crates.io, Go modules, Maven, …)

1. Add `src/pkgcache/handlers/<eco>.py` implementing the `Repository` protocol
   (`role`, `progress_path`, `client_endpoint`, `mount`, `rebuild_ledger`) — reuse the
   shared core (`storage`, `inflight`, `upstream`, `progress`, `ledger`).
2. Register it in [src/pkgcache/repositories.py](src/pkgcache/repositories.py), and add
   its `role` to the maps in [src/pkgcache/core/config.py](src/pkgcache/core/config.py)
   (`_DEFAULT_PORTS`, `_ROLE_SUBDIR`, `_HTTPS_ROLES` if TLS, and the `load_all` loop).
3. Publish its port on the `pkgcache` service in `docker-compose.yml`.

The manifest export, checkpoint, DVC versioning, and the Web UI pick it up with no
other changes.
