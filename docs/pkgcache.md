# pkgcache — a package cache for one machine

`pkgcache` is [pkgreg](system-overview.md) with no network surface: one loopback port,
no certificate, no account, no privileged setup. The first command that needs it starts
it, it exits when nothing has used it for a while, and the next command starts it again.

For the design and the decisions behind it, see [local-cache-plan.md](local-cache-plan.md).

```sh
make pkgcache            # or: go install github.com/brightskies/pkgreg/cmd/pkgcache@latest
pkgcache limit 25G
pkgcache run -- npm ci
```

That is the whole setup. Nothing is installed, nothing is reachable from another
machine, and `pkgcache limit -uninstall` is not a thing you need because nothing was
put anywhere except your own cache directory.

## Commands

| | |
|---|---|
| `pkgcache run -- <cmd>` | run one command with pip, uv, npm, yarn, pnpm and git pointed at the cache |
| `pkgcache shell` | the same, as a child shell; type `exit` to leave |
| `pkgcache env` | print the exports, for a shell that has to keep them |
| `pkgcache build` / `compose` | `docker build` / `docker compose` through the cache, Dockerfile untouched |
| `pkgcache limit 25G \| none` | set the cache budget — required before first use |
| `pkgcache status` | size against the limit, uptime, and whether it is still caching |
| `pkgcache prune` | reclaim space, when you ask and not before |
| `pkgcache persist` | settings that outlive the session, plus socket activation |
| `pkgcache docker-setup` | teach the Docker daemon about this cache |
| `pkgcache docker-build-setup` | send apt and apk through the cache in every build on this machine |
| `pkgcache stop`, `serve`, `version` | |

## The limit is mandatory, and nothing is ever deleted

A server evicts. `pkgcache` does not — the packages in it are the ones your current work
depends on, and a background process deleting them to hold a number nobody chose is the
wrong behaviour on a machine you are sitting in front of.

So it asks for a size once and never guesses:

```sh
pkgcache limit 25G       # cap at 25 GiB
pkgcache limit none      # no cap; a free-disk floor still applies
PKGCACHE_LIMIT=25G       # the same, for CI
```

When the cache is full it **keeps serving and stops storing**. Your build still works;
it is just no longer being made faster. That is reported four ways, because a cache that
silently degrades is worse than one that fails:

- on stderr after `run` and `shell`;
- by `pkgcache status`, which exits non-zero;
- as a desktop notification, once an hour at most, skipped where there is no desktop;
- as exit status **75** from every pkgcache command — `run` included, so a pipeline that
  has quietly stopped caching goes red. A failing command's own status always wins, so
  75 means "it worked, and nothing was cached".

`pkgcache prune` reclaims space. It is the only thing that deletes.

## What works with no setup at all

| Ecosystem | How | Anything privileged? |
|---|---|---|
| **pypi** (pip, uv) | `PIP_INDEX_URL`, `UV_DEFAULT_INDEX` | no |
| **npm** (npm, yarn, pnpm) | `NPM_CONFIG_REGISTRY` | no |
| **git** | `GIT_CONFIG_*` `insteadOf` — an unmodified `git clone https://github.com/…` is served from the cache | no |
| **files** | `PKGCACHE_FILES_URL` | no |
| **oci** (docker) | native Linux: works over loopback. Docker Desktop and remote daemons: their loopback is not yours | **yes** — one `docker-setup` |
| **apt / apk** | the proxy address is published as `PKGCACHE_APT_PROXY`; `docker-build-setup` wires it into every build | for machine-wide, yes |

`http_proxy` is deliberately never exported: the forward proxy relays `http://` only, so
setting it would send curl, wget and every HTTPS client through something that cannot
serve them.

## Docker

On native Linux `pkgcache build` works with no setup — the daemon accepts loopback, and
the build runs with `--network=host`.

Elsewhere the daemon cannot see your loopback, so:

```sh
pkgcache docker-setup                 # one file, reversible with -uninstall
pkgcache build -host-address -t app .
```

`docker-setup -mirror` additionally makes `docker pull python:3.12` — unmodified, no
wrapper — come from the cache. It is opt-in because it reroutes every pull on the
machine.

## Settings that outlive the session

```sh
pkgcache persist
```

writes `~/.npmrc`, `~/.config/pip/pip.conf`, `~/.config/uv/uv.toml` and `~/.gitconfig`,
each fenced by markers so `-uninstall` removes exactly what it added and leaves your own
settings byte for byte. Nothing under `/etc`, no root.

It also installs **socket activation**, and refuses to install without it unless you
pass `-anyway`. That is the point rather than a precaution: a `.npmrc` naming a port
nothing is listening on fails `npm install` rather than slowing it, and the daemon is
deliberately one that exits when idle. With activation, systemd holds the port open, the
first connection starts the cache, and it still exits when idle.

## Working offline

```sh
pkgcache run -offline -- uv pip install -r requirements.txt
```

serves what the cache holds and never touches the network. Anything it does not have is
refused with a specific error rather than a hang.

## The one thing this does not promise

A loopback socket is not per-user. On a shared machine any local account can use your
cache — the data directory is `0700` so its *contents* stay private, but the socket in
front of it is not authenticated. On a multi-user host, run `pkgreg serve` with tokens
instead.

## Where things live

```
~/.local/share/pkgcache/     Linux ($XDG_DATA_HOME), 0700
~/Library/Application Support/pkgcache/    macOS
%LOCALAPPDATA%\pkgcache\     Windows
```

`PKGCACHE_DATA_DIR` overrides it. Inside: `blobs/` and `db/` (the cache itself),
`budget.json` (your limit), `daemon.json` (the running daemon), `daemon.log`.

## Verified where

Linux is implemented and run: real `npm`, `uv` and `git` through the cache and again
from it with the origin switched off; a real `docker build`; socket activation against a
pre-bound descriptor; twenty concurrent starts producing one daemon.

The macOS and Windows branches of `docker-setup`, `persist` and the desktop notification
are written to the same contract and **have not been run**. The generated systemd units
are not exercised against a live user session.
