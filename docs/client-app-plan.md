# The desktop app — one client, three platforms, one install

Today `pkgcache` is a good daemon with a GUI assembled around it out of whatever each
platform would accept: a cgo WebKitGTK binary on Linux, a WebView2 binary on Windows, a
Swift helper on macOS, and three separate status-bar implementations behind them. It
works, and it reads as three half-products because it is three half-products.

Two goals, and the second one constrains the first:

1. The GUI becomes one thing, built once.
2. **Each platform has one install that installs everything** — daemon, CLI, app,
   status bar item, autostart, configuration — and one way to remove it again.

The daemon does not change.

## Status

**Phase 0's gate is cleared: the app compiles on macOS and on Ubuntu 24.04 with GTK4.**
Wails v3 beta.12 held on both platforms, which was the one genuine risk in this plan.

| | state |
|---|---|
| `internal/appcore` — the app's logic, toolkit-free | **done**, 7 tests; the CLI's tray uses it too |
| `internal/feed` — appcast, apt indexes, Release, OpenPGP signing | **done**, 40 tests, verified against real `gpg` |
| `pkgreg publish-apt` | **done**, run end to end against real `.deb` files |
| `pkgreg doctor` verifying the repository | **done**, 4 tests; catches a rewritten index on real data |
| `GET /apt/...` serving | **done**, 10 tests including traversal and symlink escape |
| The two Debian packages | **done**, byte-reproducible; **installed by real apt** |
| `install.sh` apt bootstrap | its key-and-source steps verified in a container |
| `cmd/pkgcache-app` — the Wails app | **compiles on macOS and Linux; never yet run** |
| Windows NSIS installer, macOS `.pkg` changes | not started |
| Phase 5 cutover | not started; it waits on the app running |

### What real apt does with this

A clean Ubuntu 24.04 container, given only the published repository and its keyring:

```
Get:1 http://…:8099 stable InRelease [1874 B]        # no warning: the signature verified
Depends: pkgcache
Depends: libgtk-4-1
Depends: libwebkitgtk-6.0-4
```

`apt install pkgcache-desktop` — one command — pulled `pkgcache` with it, resolved the GTK4
stack, installed both, and `dpkg -V` reports every file matching its recorded digest. That
is the whole claim of the two-package split, demonstrated rather than argued.

### What building it found

The app was written against the Wails API read from the module source, without a compiler.
Two real defects came out of that, both fixed:

- `MenuItem.SetLabel` and `SetEnabled` do **not** marshal onto the UI thread, unlike
  `SystemTray.SetTooltip`, which does. Updating the menu from the poll ticker without
  `application.InvokeSync` is precisely the flaky-tray failure the code it replaces warned
  about.
- `showWindow` may start a cold daemon, which the client waits up to thirty seconds for.
  On the UI thread that is a frozen menu.
Only two, in fact. A third thing I reported as a defect was not one, and the correction
is worth keeping written down:

- The macOS build prints twenty-five `built for newer 'macOS' version` linker warnings. I
  read those as a binary promising macOS 11.0 support it did not have — a crash on older
  Macs that would never reproduce on a new one. `otool -l` says otherwise: the linked
  binary declares `minos 11.0`, which is exactly what `build-pkg.sh` writes into
  `LSMinimumSystemVersion`. The two already agreed and there was no defect.
  `CGO_CFLAGS=-mmacosx-version-min=11.0` takes the twenty-five warnings down to one, which
  is worth doing so real warnings stay visible, but it is tidying and not a fix. Six of
  Wails' darwin files pass only `-x objective-c` and compile against the host SDK; the
  survivor is Go's own runtime object at 13.0. `MACOSX_DEPLOYMENT_TARGET` does nothing
  here — clang ignores it wherever `-mmacosx-version-min` is already given, which for
  Wails is nearly everywhere.

A fourth finding shaped the design rather than fixing a bug: `getDownload` in the control
API matches filenames against a strict single-segment grammar, so it **cannot** serve
`dists/stable/InRelease`. The repository needed its own handler, which is why the signing
key is a sibling of the served tree rather than inside it.

### Still unknown

Nobody has watched the app run. It compiles on both platforms and its logic is tested, but
whether the window appears, the icon lands in the bar, and the menu greys correctly when
the daemon sleeps are all unobserved.

## The split

The decision everything else follows from: **the GUI stops being a subcommand of the CLI
and becomes a separate program that talks to the daemon over the loopback API it already
serves.**

| | what it is | how it builds |
|---|---|---|
| `pkgcache` | the daemon and the CLI, exactly as today | `CGO_ENABLED=0`, five targets from one host |
| `pkgcache-app` | the desktop app: window, status bar, notifications | cgo, on each platform's own runner |

The daemon keeps the property the release story rests on. The app is where cgo is
allowed, because it is already per-platform in every way that matters and pretending
otherwise is what produced the current arrangement.

Two binaries, one install. They are separate programs for build reasons, and a person
installing this should never learn that.

The app is a client like any other. It finds or starts the daemon through
[internal/local](../go/internal/local/client.go), points a webview at
`http://127.0.0.1:<port>/widget`, and the page talks to `/api/v1` itself — which is what
happens today, minus two binaries and a Swift file.

## Why Wails v3

It is Go, it uses each platform's own web engine, and it has the two things the current
arrangement had to build by hand: a window and a status bar item, on all three platforms,
from one source tree. The existing console and widget HTML move over unchanged, because
the app loads them from the daemon exactly as the browser does now.

It also packages and updates. `wails3 package` produces a `.app` and `.dmg`, an NSIS or
MSI installer, and `.deb`/`.rpm` through nfpm — which is the second goal, and the reason
this beats hand-rolling three more installers. Its built-in updater then covers macOS and
Windows from one appcast feed, which is the whole of Phase 4's client side.

Requirements, all of which are already met or nearly so:

- Go 1.24+. The module is on 1.25.0.
- Linux: `gcc`, `gtk4` and `webkitgtk-6.0`, or GTK3 and WebKit2GTK 4.1 under `-tags gtk3`.
  Ubuntu 24.04 — the target the current helper is already specialised to — has both.
- macOS 10.15+ with the Command Line Tools, which is what builds the Swift helper today.
- Windows: the WebView2 runtime, which is what `go-webview2` needs today.

**It is beta.** As of August 2026 Wails v3 has not cut 3.0. The desktop API is declared
stable and it is in production use, but this is the one genuine risk in the plan, and
Phase 0 exists to retire it before anything is committed to.

## What is deleted

Not a rewrite — a deletion. Roughly 1,800 lines of platform glue go away:

| | lines | |
|---|---|---|
| `go/cmd/pkgcache-window/` | ~200 | three engines, one of them a stub |
| `internal/tray/tray_{linux,darwin,windows,other}.go` | ~850 | D-Bus, Shell_NotifyIcon, a pipe protocol |
| `internal/tray/{menu,icon}_linux.go` | ~260 | drawing a menu over D-Bus by hand |
| `tools/menubar/main.swift` | 403 | a second language, for one platform |
| `internal/local/window.go` | 70 | finding the helper that no longer exists |

Two dependencies fall out of `go.mod` with them: `godbus/dbus` and `jchv/go-webview2` are
imported by exactly this code and nothing else.

**What stays** is the part that was always right: [internal/tray/tray.go](../go/internal/tray/tray.go)
— `State`, `Action`, `Label`, `Enabled`, `Tooltip`, `Menu` — is platform-free domain logic
with tests, and Wails renders it instead of D-Bus. `trayState` and `trayDo` in
[cmd/pkgcache/tray.go](../go/cmd/pkgcache/tray.go) move into the app with their bodies
intact. The rule they encode moves with them: **the app never keeps the daemon alive.**
An app that held the cache up would quietly remove the idle exit, and the idle exit is
what makes this polite to leave installed.

## What "one install" means

Only the macOS `.pkg` does the whole job today. It is the reference; the other three are
measured against it:

| | binary | app | icon | launcher entry | autostart | configures | uninstall |
|---|---|---|---|---|---|---|---|
| macOS `.pkg` | yes | yes | yes | Applications | yes | yes | **no** |
| `.deb` | yes | *optional* | **no** | `.desktop` | **no** | **no** | `dpkg -r` |
| `install.ps1` | yes | **no** | **no** | **no** | **no** | yes | **no** |
| `install.sh` | yes | **no** | **no** | **no** | **no** | yes | **no** |

The `.deb` postinst currently ends by printing two more commands to run. That is the
patchwork feeling, stated out loud by the software itself.

Every cell above becomes "yes" — including the uninstall column, which is currently "no"
even on macOS. That is not a nice-to-have: the ethic is already written down in
[internal/local/autostart.go](../go/internal/local/autostart.go) — *a program that installs
something into somebody's session owes them a way to take it out again that leaves no
trace* — and an installer that does six things owes it six times over.

Two properties of the current installers have to survive, because they were expensive to
learn:

- **Verify before installing, and move rather than overwrite.** Every step in
  [packaging/README.md](../packaging/README.md) is there because its absence broke a real
  installation, and none of them stop being true.
- **The installer configures the machine.** `build-pkg.sh --server … --ca-sha256 …` bakes
  a team cache into the package, so installing *is* the setup. Every platform gets this,
  and a pkgreg server keeps being able to hand out an installer built for itself.

## Linux: two packages, one command

**Decided: the Debian split, `pkgcache` and `pkgcache-desktop`, served from an apt
repository pkgreg hosts.**

The problem it solves: the app cannot run without GTK and WebKit, so a single package
containing it must declare them as `Depends` rather than the `Recommends` they are today.
That drags a desktop graphics stack onto every CI runner, build box and container that
installs pkgcache — and headless is a real case here, not a hypothetical one. `pkgcache
build`, `docker-build-setup` and `pkgcache run --` all live on machines with no screen,
and [window_linux.go](../go/cmd/pkgcache-window/window_linux.go) already carries a
written-out error for exactly that situation.

So:

| package | contains | depends on |
|---|---|---|
| `pkgcache` | daemon, CLI | nothing — the static binary it is today |
| `pkgcache-desktop` | the app, icon, `.desktop`, login item | `pkgcache (= same version)`, GTK4, WebKitGTK 6.0 |

The version lock is deliberate. The app talks to the daemon over its loopback API, and two
halves of one product drifting apart on a machine is a bug report nobody can read.

**This is one command, not two, because apt resolves the dependency:**

```sh
apt install pkgcache-desktop      # pulls pkgcache in with it
apt install pkgcache              # headless: the binary, no desktop stack
```

That only works from a repository — the reason this is worth building rather than shipping
two loose `.deb` files, which is two downloads and the thing this plan exists to remove.

What it costs, and what it buys:

- **Costs** an apt repository: generating `dists/`, `Packages.gz` and a signed `InRelease`,
  and managing the signing key. pkgreg's [apt support](../go/internal/eco/apt/apt.go) does
  not help — it is a pull-through proxy that caches Debian repos and understands those
  filenames only to decide freshness. Publishing one is new work.
- **Buys** the update story this plan otherwise does not have. `apt upgrade` is how Linux
  machines stay current, and nothing else in this product currently updates itself at all.

## Phases

Each ends somewhere shippable, and the first one is allowed to end the plan.

### Phase 0 — the spike, and the gate (3–4 days)

Build, on all three platforms, a window that loads `127.0.0.1/widget` and a status bar
item whose menu changes as state changes — then run `wails3 package` and look at what
comes out. The questions are whether the beta API holds, whether GTK4/WebKitGTK-6.0 is
reachable on Ubuntu 24.04 without a fight, whether the macOS status item can be an agent
or must bring a Dock icon, and whether the generated packages can carry a postinstall step
and an uninstaller.

**Gate:** if this is painful, stop and do the packaging work alone against the existing
helpers — the installers need every one of those missing cells filled either way, so
nothing is wasted by finding out here.

### Phase 1 — parity (1 week)

`go/cmd/pkgcache-app`: window plus status bar, reusing `internal/tray`'s logic and
`internal/local`'s daemon discovery. Everything the three helpers do today, done once.
The old helpers keep building alongside it, so nothing is at risk yet.

### Phase 2 — the part that makes it an app (1 week)

The things a browser tab cannot do, roughly in order of how much they matter:

- **Notifications.** "FULL: serving, not storing" is the whole reason the status bar item
  exists, and today it is a tooltip nobody hovers. It should be a notification.
- **Native menus.** A webview on macOS has no copy and paste without an Edit menu.
- **Window state.** Size and position, remembered.
- **A single instance.** Clicking the icon twice opens one window, not two.
- **Open at login**, through the platform API instead of the hand-written desktop files
  and registry entries in [internal/local/autostart.go](../go/internal/local/autostart.go).
- **Start the daemon on launch**, since opening the app is somebody asking to look at it.

### Phase 3 — the installers (2 weeks)

Each artefact installs both binaries, the icon set, the launcher entry, the login item,
and an uninstaller — and configures the machine when it was built for a cache.

- **macOS** — `.dmg` or `.pkg`. Closest to done: keep the postinstall logic from
  [build-pkg.sh](../packaging/macos/build-pkg.sh), including `sudo -H` dropping back to the
  console user, and add the uninstaller it lacks.
- **Windows** — NSIS or MSI, replacing PATH-editing in `install.ps1` with a real install:
  Start Menu entry, Add/Remove Programs, per-user so no elevation is needed.
- **Linux** — the two packages above, both built in this phase. The repository that serves
  them is Phase 4; until it exists they install together from local files.
- **`install.sh` / `install.ps1`** stay as the scriptable path, and install the same full
  set rather than a lone binary.

CI already has macOS and Windows runners and already signs and notarises `pkgreg-client`;
the app and its installers join that path rather than inventing one.

The `.deb` is byte-reproducible today because it is built with `ar` and `tar`. nfpm may
give that up. Worth measuring, and worth keeping the current script if it does.

### Phase 4 — the update path (2 weeks)

Server-side work, in pkgreg rather than in the client, and the only part of this plan that
is not about the desktop. Nothing in this product updates itself today on any platform.

`publish-client` already writes binaries into `DATA_DIR/downloads` and is the command an
operator runs on a production host. It becomes the single source of truth: it knows what
versions exist and what their checksums are, and it renders that into the feed each
platform's own update mechanism expects.

| platform | mechanism | what pkgreg publishes | signed with |
|---|---|---|---|
| Linux | apt | `dists/`, `Packages.gz`, `InRelease` | an OpenPGP key |
| macOS | the Wails updater | `appcast.xml` | an Ed25519 key |
| Windows | the Wails updater | the same appcast, per-platform enclosures | the same Ed25519 key |

Two feeds, not three: Wails v3 ships a built-in updater with an Appcast provider,
signature verification, an update window with skip-this-version and restart-to-install,
and binary delta patches. One appcast serves both macOS and Windows.

Linux deliberately does not use it. An app that updates itself behind dpkg's back is an
anti-pattern on Linux and would fight the package manager; apt is the right answer there,
and the updater is compiled out of the Linux build.

Four things to get right:

- **The keys.** Two kinds of new key material, each with a lifecycle: where it lives, who
  can reach it, and how it rotates. pkgreg has PKI machinery in
  [internal/pki](../go/internal/pki), but neither an OpenPGP repository key nor an Ed25519
  appcast key is a TLS key and neither should pretend to be one.

- **The bootstrap, and where trust comes from.** Installing from a repository requires the
  repository to be configured, which requires its key — the usual chicken and egg, solved
  the way Docker and Tailscale solve it: `install.sh` adds the source and the key, then
  runs apt. Still one command.

  This has a better answer here than most projects get, because the trust already exists.
  The CA fingerprint given out of band and pinned by [install.sh](../packaging/install.sh)
  is already the anchor for everything else, so both signing keys are fetched over that
  verified connection rather than becoming further secrets to hand around. For the same
  reason the appcast URL needs no configuration of its own: the machine already knows its
  server from `pkgcache setup -server`, and the feed is derived from it.

- **Both halves update together.** On Linux the `= same version` dependency enforces it.
  On macOS the updater replaces the `.app` bundle and nothing outside it — so `pkgcache`
  moves *inside* the bundle with `/usr/local/bin/pkgcache` as a symlink into it, which is
  the trick [build-pkg.sh](../packaging/macos/build-pkg.sh) already uses for the menu bar
  helper and for the same reason. On Windows both binaries live in one directory the
  updater replaces wholesale, stopping a running daemon first as `install.ps1` already does.

- **An update is not an install.** It replaces binaries and touches nothing else. The cache
  directory, the team cache, the pinned CA and the disk budget all survive it, and an
  update that silently re-ran `setup` would be a data-loss bug wearing a friendly name.

Air-gapped machines are the case to keep honest: a cache that must never reach the internet
should not grow a component that phones home. The update check is off by default wherever
`-no-direct` is set, and can be turned off on its own everywhere else.

### Phase 5 — cutover (2–3 days)

Delete the helpers. `pkgcache widget`, `pkgcache tray` and `pkgcache console` stay as thin
shims that launch the app or print the URL, because a terminal is still a reasonable place
to ask for a window and breaking that for people over SSH would be gratuitous.

## Open questions

**Does the macOS app get a Dock icon?** Today the Swift helper is an `LSUIElement` agent —
menu bar only, deliberately. A full-fledged app usually has a Dock presence. Suggested
answer: a real app with a window, and the menu bar item as an option rather than the whole
of it. This is a product decision, not a technical one.

**GTK4 or GTK3?** GTK4 and WebKitGTK 6.0 is Wails' default and the better bet going
forward; `-tags gtk3` matches what the current helper links against. Phase 0 decides.

## Cost

Six to seven weeks. Phase 3 is the least predictable; Phase 4 is the newest, being the only
part of this plan that is server work rather than client work, and the only part that
introduces key material somebody has to look after for as long as the product exists.

The daemon is untouched throughout, so the blast radius is the GUI, the installers, and one
new publishing path. Phases 0–3 stand on their own: they end with three real installers and
no update path, which is strictly better than today and a reasonable place to stop if
Phase 4's key management is more than the team wants to own.
