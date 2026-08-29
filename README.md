<p align="center">
  <img src="assets/logo.svg" alt="PKGCache logo" width="92" height="92" />
</p>

# PKGCache

A package cache that a download crosses once. It sits in front of PyPI, npm, Docker Hub,
apt and git, keeps what it fetches on local disk, and can carry its contents across an
air gap as a verified pack.

Two programs, one codebase:

| | | |
|---|---|---|
| **pkgreg** | the shared cache | Runs on a server. Projects, accounts, quotas, an audit log, a web console, and ordered upstream chains. |
| **pkgcache** | the single-machine cache | Runs on a laptop. Same cache, same projects, no accounts — and it can sit in front of a team's `pkgreg` so a package crosses the network once for everyone. |

Both are one static Go binary with no runtime dependencies, cross-compiled for Linux,
macOS and Windows from a single host.

## Install pkgcache

On Debian and Ubuntu it is a package, from a signed repository. `apt upgrade` keeps it
current from then on, which is the part no download page can do:

```bash
curl -fsSL https://aabdlwahab.github.io/PKGCache/apt/pkgcache-archive-keyring.asc \
  | sudo tee /usr/share/keyrings/pkgcache-archive-keyring.asc >/dev/null
sudo tee /etc/apt/sources.list.d/pkgcache.sources >/dev/null <<'EOF'
Types: deb
URIs: https://aabdlwahab.github.io/PKGCache/apt
Suites: stable
Components: main
Signed-By: /usr/share/keyrings/pkgcache-archive-keyring.asc
EOF
sudo apt update

sudo apt install pkgcache-desktop    # a laptop: daemon, CLI, docker shim and the app
sudo apt install pkgcache            # a server or CI runner: no desktop graphics stack
```

The key that signs it has fingerprint
`1ECAC3BB65F1568F0F4F063E1C5782827618C926`;
`gpg --show-keys /usr/share/keyrings/pkgcache-archive-keyring.asc` prints what your
machine actually trusts.

macOS and Windows have real installers, which verify what they download before installing
it and can point the machine at your team cache as part of the install:

```bash
# macOS — build the .pkg once, then anyone double-clicks it
./packaging/macos/build-pkg.sh --version 1.0.0 \
    --server https://cache.internal:8443 --ca-sha256 AA:BB:...

# any Unix, from a running cache
sh packaging/install.sh --server https://cache.internal:8443 --ca-sha256 AA:BB:...
```

See [packaging/README.md](packaging/README.md) for Windows and for what each installer
does. The fingerprint there comes from whoever runs the cache — it is what makes a cache
serving its own certificate verifiable.

## Use it

```bash
pkgcache setup -limit 25G                 # a cache of your own, no server needed
pkgcache setup -server https://cache.internal:8443 \
               -ca-sha256 AA:BB:... -limit 25G   # ...in front of a team cache

pkgcache run -- pip install -r requirements.txt   # one command, through the cache
pkgcache shell                                    # a shell where every tool uses it
pkgcache build -t myapp .                         # docker build, through the cache
pkgcache pull nvcr.io/nvidia/pytorch:24.01        # any registry, still named that after
pkgcache tray                                     # keep it in your status bar
```

`pkgcache status` says what is running and what it holds. Nothing is installed
machine-wide and nothing is left behind when the shell exits.

## What it caches

| ecosystem | reached as | chains to a team cache |
|---|---|---|
| PyPI | `PIP_INDEX_URL`, `UV_DEFAULT_INDEX` | yes |
| npm | `NPM_CONFIG_REGISTRY` | yes |
| OCI images | a registry on `127.0.0.1` — any upstream registry, named in the path | yes |
| apt / apk | an HTTP proxy | no — the origin comes from the request |
| git | a path-prefixed mirror | no — same reason |
| files | upload and download | no — nothing upstream to chain to |

Chaining means ordered upstreams: the team cache first, the public origin second, and
automatic fall-through when the team cache cannot be reached.

## Run a server

```bash
cd go
make build
./bin/pkgreg init -data-dir /var/lib/pkgreg -hostnames cache.internal
./bin/pkgreg publish-client -data-dir /var/lib/pkgreg bin
./bin/pkgreg serve -config /var/lib/pkgreg/pkgreg.yaml
```

`init` mints a CA and prints its fingerprint — that is the string clients verify against.
`pkgreg systemd` installs it as a service. `pkgreg doctor` checks configuration, storage
and TLS and says what is wrong.

The console is at `https://<host>:8443/`, and `/tutorial` walks a newcomer through
downloading a client and configuring their tools.

## Across an air gap

The cache is versioned, so what it holds can be moved as a verified pack:

```bash
pkgcache checkpoint -m "before the release"
pkgcache export --since <checkpoint>     # a pack, full or delta
# carry it across
pkgcache import ./pack.tar               # verified, and fast-forward only
```

`pkgcache snapshots` lists what a project holds; `pkgcache rollback` makes an earlier
checkpoint current again.

## Documentation

| | |
|---|---|
| [System overview](docs/system-overview.md) | what the pieces are and how they fit |
| [Go server](go/README.md) | commands, deployment, layout |
| [pkgcache](docs/pkgcache.md) | the single-machine cache in full |
| [Installers](packaging/README.md) | macOS, Ubuntu, Windows |
| [Client onboarding](docs/client-onboarding.md) | pointing tools at a cache |
| [Docker builds](docs/docker-builds.md) | builds that resolve through the cache |
| [Git](docs/git-cache.md) | cloning and fetching through the cache |
| [Client bridge](docs/client-bridge.md) | reaching the cache from inside a container |
| [Running and testing](docs/running-and-testing.md) | the test suite and what it covers |
| [Architecture](docs/go-architecture.md) | the design, in detail |

## Layout

```
go/          the implementation: both binaries, one module
  cmd/         pkgreg, pkgcache, and the platform helpers
  internal/    ecosystems, storage, control plane, console
packaging/   installers for macOS, Ubuntu and Windows
docs/        documentation
examples/    Dockerfiles that build through the cache
```

## Development

```bash
cd go
make build      # both binaries
make test       # unit, integration and real-client acceptance
make lint
```

The acceptance tests drive real `pip`, `uv`, `npm` and `docker` clients against a live
instance, so they need Docker and a Python toolchain. Everything is one Go module:
`github.com/aabdlwahab/PKGCache`.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
