# pkgcache — a package cache for one machine

`pkgcache` is [pkgreg](system-overview.md) with no network surface: one loopback port,
no certificate, no account, no privileged setup. The first command that needs it starts
it, it exits when nothing has used it for a while, and the next command starts it again.


```sh
make pkgcache            # or: go install github.com/aabdlwahab/PKGCache/cmd/pkgcache@latest
pkgcache setup -limit 25G
pkgcache run -- npm ci
```

With a team cache in front of the registries:

```sh
pkgcache setup -server https://cache.internal:8443 -ca-sha256 AB:CD:… -limit 25G
```

That is the whole setup. Nothing is installed, nothing is reachable from another
machine, and `pkgcache limit -uninstall` is not a thing you need because nothing was
put anywhere except your own cache directory.

## Installing

Each platform has a real installer, and they verify what they download before installing
it. Installing from a cache also configures the machine to use it, so there is no second
step to forget:

```sh
# macOS — a .pkg somebody double-clicks; carries the menu bar app too
./packaging/macos/build-pkg.sh --version 1.0.0 \
    --server https://cache.internal:8443 --ca-sha256 AA:BB:...

# Ubuntu / Debian
make -C go deb && sudo apt install ./go/bin/pkgcache_1.0.0_amd64.deb

# any Unix, or Windows with install.ps1
sh packaging/install.sh --server https://cache.internal:8443 --ca-sha256 AA:BB:...
```

A checksum is verified before anything is installed, and the binary is moved into place
rather than written over — on macOS a code signature is cached against the file's inode,
so writing new bytes into the old one leaves every later run killed by the kernel with no
message but "killed". See [the installers](../packaging/README.md).

## Commands

| | |
|---|---|
| `pkgcache run -- <cmd>` | run one command with pip, uv, npm, yarn, pnpm and git pointed at the cache |
| `pkgcache shell` | the same, as a child shell; type `exit` to leave |
| `pkgcache env` | print the exports, for a shell that has to keep them |
| `pkgcache build` / `compose` | `docker build` / `docker compose` through the cache, Dockerfile untouched |
| `pkgcache pull <image>` | pull an image through the cache, and keep the name you asked for |
| `pkgcache crate` | run the crate orchestrator with its builds served from the cache |
| `pkgcache warmlock` | fill the cache from a lock file, and point the lock at it |
| `pkgcache setup` | point this machine at a cache, once — budget, team cache, everything |
| `pkgcache project` | the projects this cache serves: `ls`, `create`, `rm`, `use` |
| `pkgcache limit 25G \| none` | change the budget later |
| `pkgcache status` | size against the limit, uptime, per-project figures, and whether it is still caching |
| `pkgcache widget` | a small window that watches this cache; `-on-login` opens it with your session |
| `pkgcache tray` | the same, kept in the status bar; `-on-login` starts it with your session |
| `pkgcache console` | the full console, in your browser |
| `pkgcache prune` | reclaim space, when you ask and not before |
| `pkgcache export` / `import` | carry what a project holds to a machine with no network, and back |
| `pkgcache checkpoint` / `snapshots` / `rollback` | name what a project holds now, list those names, return to one |
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

## Projects

The same projects a `pkgreg` server has, without the accounts — there is one person here
and nobody to keep out.

```sh
pkgcache project ls                 # marking the current one, and where its misses go
pkgcache project create work
pkgcache project use work           # every run, build and persist defaults to it
pkgcache run -project side -- npm ci   # or name one for a single command
PKGCACHE_PROJECT=ci pkgcache run -- npm ci   # or for a pipeline that must not depend on
                                             # what somebody selected in a shell
```

A project on a laptop is exactly two things:

- **A separate upstream chain.** Work resolves through the company's cache; a side
  project goes straight to the public registry. That is configured per project with
  `pkgcache setup -project work -server …`, and the global project's configuration is
  the fallback for any project without its own.
- **A separate accounting boundary.** "What is this project costing" is one `GROUP BY`,
  because there is one catalog rather than one per project.

It is **not an isolation boundary**, and nothing should be built on the assumption that
it is. Content is stored once by digest, which is the point: two projects that need the
same 2.5 GB wheel hold one copy. Deleting a project drops its catalog entries and leaves
the bytes for `pkgcache prune` to reclaim once nothing references them.

The URL carries the project, exactly as on a server:
`http://127.0.0.1:41780/<project>/npm/…`. An unregistered name is a 404 rather than
somebody else's content.

## Three tiers

With a team cache configured, a lookup goes local, then the team's cache, then the
registries. It is not a mode: `setup` writes two ordinary upstream rows per index, the
team at priority 10 and the public registry at 20, and the engine walks a chain it
already knows how to walk.

The team's CA is fetched over an unverified connection and refused unless it matches
the fingerprint you were given separately; every request after that is verified
normally.

**When the team cache cannot serve**, the next origin is tried — on a connection
refused, a DNS failure, a timeout or a 5xx. Deliberately *not* on a 404, which is what
a cache in offline mode answers and falling through would defeat, and not on a 401 or
403, which is a misconfigured credential that going around would hide.

`setup -no-direct` omits the public row, for a machine that must never fetch from the
internet itself. `pkgcache status` shows all three tiers for the current project and
probes the team cache, so one that has been down for a week is visible rather than just
"builds got slower".

**Per project.** `setup -project work` configures the chain for one of this machine's
projects, and `-team-project` names the project on the team's side — which defaults to
their global project, because assuming a name exists on somebody else's server is not
this program's call. A project with no configuration of its own inherits the global
project's, so a machine pointed at a team cache routes everything through it until told
otherwise, and `pkgcache project ls` marks an inherited chain with `*`. Two team caches
mean two self-minted CAs, so the file the outbound pool trusts is a bundle assembled
from those records; removing a project's configuration removes its CA with it.

Chained ecosystems today are **pypi, npm and oci**. apt and git derive their origin from
the request itself rather than from configuration, and `files` has no upstream at all —
its content arrives by upload. Those three are absent rather than half-supported.

OCI is the one whose URLs do not look like the others'. The distribution spec fixes
`/v2` as the API root, so a chained origin has to name it — the team's root for Docker
Hub is `https://cache:8443/v2/dockerhub`, and the public origin beside it is written
`https://registry-1.docker.io/v2` rather than bare. Both ends of a chain have to be
repository roots of the same shape, because a fallback is the head's URL with its prefix
swapped; written bare, every fallback pull would lose its `/v2` and 404 against the real
registry. The project also rides a different segment here: the server reads it from
after `/v2` and treats a literal `global` there as a registry name, so the global project
addresses `/v2/<registry>` and every other project `/v2/<project>/<registry>`.

Registries the image name spells out — `nvcr.io`, `gcr.io`, anything not aliased — are
chained through one wildcard row rather than one row each, because the registry is a
segment on the team's `/v2` root exactly as it is on ours. That row has no public origin
beside it: the fallback would have to be whichever registry the pull turns out to name,
which is not knowable when the chain is written. A discovered registry therefore resolves
through the team cache and only through it, which is also what makes `-no-direct` hold
for a registry nobody configured.

## Replacing pkgreg-client

`pkgcache setup -no-cache -server … -ca-sha256 …` is `pkgreg-client` exactly: a
verified loopback bridge to a team cache, with no local store. In that mode nothing is
written to the cache directory and no database is opened, which a test asserts by
looking at the directory afterwards.

`pkgreg-client` still exists as a shim that forwards to `pkgcache` and says so once.

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

Elsewhere the daemon cannot see your loopback — on macOS and Windows it is a virtual
machine — so the build is pointed at `host.docker.internal` instead. That is worked out
for you and printed when it happens; the daemon still has to be told once that a
plain-HTTP registry at that address is acceptable:

```sh
pkgcache docker-setup                 # one file, reversible with -uninstall
pkgcache build -t app .
```

`-host-address` forces the gateway and `-host-address=false` forces loopback, for a setup
neither rule describes.

`docker pull` through the cache does not need any of that:

```sh
pkgcache pull nvcr.io/nvidia/pytorch:24.01   # named nvcr.io/nvidia/pytorch:24.01 after
```

It rewrites the reference, fetches through the cache, and tags the image back to the name
you typed — so a compose file or a manifest naming that image still finds it. The registry
comes from the name, so a registry nobody configured works the first time it is asked for.

`docker-setup -mirror` is the other approach: it registers the cache as a Docker Hub
mirror, so an unmodified `docker pull python:3.12` is served from it with no wrapper at
all. It is opt-in because it reroutes every pull on the machine — and note that it also
needs a cache that answers unprefixed `/v2/` paths, which is `server.registry_mirror` on a
pkgreg server. A `pkgcache` on a laptop does not set that today, so the daemon asks, gets a
404 and falls back to Docker Hub: use `pkgcache pull` there.

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

## A window to leave open

```sh
pkgcache widget              # a small window: no tabs, no address bar
pkgcache widget -on-login    # and again next time you log in
pkgcache console             # the full console instead
```

It opens in a real application window where this machine has one — its own icon in the
Dock, the taskbar or alt-tab, no address bar, and **no browser needed at all**. That is
`pkgcache-app`, one program for all three platforms, installed by each platform's
installer and built with `make app` on a machine of that platform:

| | Engine |
|---|---|
| Linux | WebKitGTK, through GTK4. Needs `libgtk-4-dev` and `libwebkitgtk-6.0-dev` to build; `-tags gtk3` builds against GTK3 and WebKit2GTK 4.1 where 6.0 is not packaged |
| Windows | WebView2, Edge's engine. The runtime ships with Windows 11 and with Edge on 10 |
| macOS | WKWebView |

It is the only cgo in the product and its own Go module for that reason: `go build ./...`
in the main module never needs a GUI toolchain, and every other binary stays a static
`CGO_ENABLED=0` executable cross-built from one host.

Without the app installed it falls back to a Chromium-family browser in app mode, then to
an ordinary browser window, then to printing the address — and it says which one you got.
`pkgcache widget -tab` asks for a browser tab on purpose.

The widget opens on four questions in one column, in this order: is the cache working,
how much room is left, is it actually helping, and what is it doing right now. When the
cache fills up it says so there too — that is the fourth of the four channels, next to
stderr, the exit status and the desktop notification.

Three more tabs make it the client's control surface rather than only its monitor:

- **packages** — what this project holds, largest first. Tick the ones you are never going
  to build against again and remove them; anything a checkpoint holds is kept and said so.
  *Reclaim space* is the same collection `pkgcache prune` runs.
- **transfer** — the checkpoints, with the one you are on marked. Take one, go back to one,
  write a pack, or apply the pack waiting in `shuttle/in`.
- **sources** — where every project's misses go, and the form that changes it for the one
  you are in: the team cache's address and the CA fingerprint you were given out of band.
  Nothing is trusted without that fingerprint, and the running cache starts using it
  immediately — no restart.

It is the same console, in a shell built for 420 pixels, so there is no second frontend
and nothing extra in the binary. A Chromium-family browser (Chrome, Chromium, Edge,
Brave) gives the window without tabs; anything else gets an ordinary one, and pkgcache
says which you got. Over SSH, with no browser to open, it prints the address instead.

`-on-login` writes one marked file under your home — an XDG autostart entry, a LaunchAgent
or a Startup script — and `-off-login` removes it. The window watches the cache; it never
keeps it running, so the daemon still exits when nothing has used it.

### In the status bar

```sh
pkgcache tray               # one icon: notification area, status bar, or menu bar
pkgcache tray -on-login     # and again next time you log in
```

The icon says whether the cache is working, turns to the alarm colour and asks for
attention when it has filled up and stopped storing, and fades when the cache has gone
idle. Clicking it opens the window; its menu offers the console, offline, reclaiming space
and stopping the cache.

The two window items are two different things. **Open pkgcache** is the 420-pixel panel
above. **Open the console** is the full operator console — inventory, sources, transfers,
statistics and the jobs behind them — and it opens in your browser, with tabs and an
address bar, because that is where somebody reading statistics wants it.

**It never keeps the cache running.** Everything in the menu that needs a live daemon is
greyed while it sleeps — the icon reads the files the daemon published on its way out and
says "asleep" rather than starting one behind your back. Opening the window is the one
exception, because that is you asking.

One program, three platform mechanisms, and one of them has a caveat:

| | |
|---|---|
| Linux | `StatusNotifierItem` over D-Bus. Native on KDE, Plasma, most tiling setups, and anything with libappindicator. **GNOME needs a shell extension** (AppIndicator support) or nothing appears — `pkgcache widget` is the same window without it |
| Windows | the notification area |
| macOS | `NSStatusItem` in the menu bar, as a regular app rather than an accessory: it has a Dock icon and you can alt-tab to it |

`pkgcache tray` starts that app; the icon and the menu are the same on all three, because
what the menu says and when each item is greyed out live in `internal/tray` and
`internal/appcore`, with no toolkit in either.


## Carrying a cache somewhere with no network

The packages you need in order to keep working on a plane, at a customer site, or in a
room with no network are the ones already in your cache — that is what it is for, and why
it never evicts. So the client can hand them over:

```sh
# on the machine that has a network
pkgcache export -file /media/usb/work.tar

# on the machine that does not
pkgcache import -file /media/usb/work.tar
pkgcache run -- npm ci          # served from the pack
```

`export` checkpoints the project first, so the pack is what the cache holds *now* rather
than what it held the last time somebody thought about it, and prints the checkpoint it
made. `import` applies the pack as it reads it — there is no second command — and creates
the project if this machine does not have it.

A second trip should carry only what is new:

```sh
pkgcache snapshots                       # on the receiving machine: * marks where it is
pkgcache export -since 80a9a71cb8e6      # on the sending machine, against that
```

**An import cannot lose what you already have.** A pack records where it started, and it
is refused unless that matches the receiving project's checkpoint — nothing is written on
refusal. If it is refused, either ask for a delta from the checkpoint `pkgcache snapshots`
shows, or import into a project of its own. The one command that *does* replace a
project's content is `pkgcache rollback`, which says so.

The packs are ordinary `pkgreg` transfer packs, so they move in any direction: laptop to
laptop with no server involved, a team cache to a laptop, or a laptop to the disconnected
site's own server. A pack is verified against the digests it names, which catches
corruption — it says nothing about who made it.

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
`budget.json` (your limit), `project.json` (the project you are working in), `team.json`
and `team-ca.crt` (the team cache and the CAs it is verified against), `shuttle/in` and
`shuttle/out` (packs, when you do not give a path of your own), `daemon.json` (the running
daemon), `daemon.log`.

## Verified where

Linux is implemented and run: real `npm`, `uv` and `git` through the cache and again
from it with the origin switched off; a real `docker build`; socket activation against a
pre-bound descriptor; twenty concurrent starts producing one daemon; two caches passing a
pack between them, with the receiving one offline and installing from it — including a
delta second trip, and a pack that does not continue from the receiver's checkpoint being
refused without writing anything. The widget is rendered in a real browser at 420 and 320
pixels wide in both themes, and its export and import buttons driven through to their
results.

The macOS and Windows branches of `docker-setup`, `persist` and the desktop notification
are written to the same contract and **have not been run**. The generated systemd units
are not exercised against a live user session.
