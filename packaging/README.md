# Installing pkgcache

Three installers, one per platform. Each verifies what it installs before installing it,
and each can point the machine at a team cache as part of the install rather than leaving
that as a step to remember.

| platform | artefact | built by |
|---|---|---|
| macOS | `pkgcache-<version>.pkg` | `packaging/macos/build-pkg.sh`, **on a Mac** |
| Ubuntu / Debian | `pkgcache_<v>_<arch>.deb` and `pkgcache-desktop_<v>_<arch>.deb` | `make deb` |
| Windows | `pkgcache-<version>-setup.exe` | `make windows-installer` |
| macOS / Linux (script) | `install.sh` | `make installers` |

For a release, none of those are run by hand. Pushing a `pkgcache-v*` tag runs
[`.github/workflows/installer-release.yml`](../.github/workflows/installer-release.yml),
which builds all three, attests every artefact, and publishes them with checksums
beside them. Publishing the release then runs
[`.github/workflows/pages.yml`](../.github/workflows/pages.yml), which indexes the two
`.deb` files into the public apt repository — see
[Publishing the public repository](#publishing-the-public-repository).

Signing depends on credentials being configured, and today they are not. With the Apple
secrets it signs and notarizes the `.pkg`; with the Windows certificate it
Authenticode-signs the Windows binaries *and* the installer that carries them. Without
them it builds the same artefacts unsigned and says so — in the job summary and in the
release notes, which carry the Gatekeeper and SmartScreen steps a reader will otherwise
read as a broken download. A partial set of secrets fails the job rather than quietly
producing an unsigned installer, because that means somebody meant to sign.

Attestation is not signing and does not need an account: every file is bound to the
workflow, commit and runner that produced it, and `gh attestation verify` checks that.
It is not what Gatekeeper looks at.

Between releases, every merge to `main` runs
[`.github/workflows/main-artifacts.yml`](../.github/workflows/main-artifacts.yml), which
builds the same artefacts from the same scripts and attaches them to the run — both
`.deb`s for both architectures, the `.pkg`, the `setup.exe`, every raw binary, and one
bundled download with `SHA256SUMS`. Those are **unsigned**: Gatekeeper refuses the `.pkg`
until it is opened from the right-click menu and SmartScreen warns about the installer, so
they are for the people who merged the change rather than for customers. That is also the
answer to "where do I get a build of this commit" — building one on a laptop is how a
packaged app and a packaged daemon came to be from different commits.

### What each one installs

| | macOS `.pkg` | Ubuntu `.deb` | Windows `setup.exe` |
|---|---|---|---|
| `pkgcache` — daemon and CLI | ✓ | ✓ | ✓ |
| `pkgcache-app` — window and status bar | ✓ | ✓ *(pkgcache-desktop)* | ✓ |
| `pkgcache-docker` — the shim | ✓ | ✓ | ✓ |
| starts at login | ✓ | ✓ | optional |
| sets a disk budget if the machine has none | ✓ | ✓ *(installing user)* | ✓ |
| LICENSE and NOTICE | ✓ | ✓ | ✓ |
| an uninstaller | `pkgcache-uninstall` | `apt remove` | Add/Remove Programs |

The last four rows were not always ticks, and each blank was the same mistake: a decision
made carefully for one platform and never carried to the other two.

## Two packages on Linux, one command

The app cannot run without GTK and a WebKit, so a single package containing it would have
to declare them as `Depends` — dragging a desktop graphics stack onto every CI runner,
build box and container that installs pkgcache. So the Debian build produces two:

| package | contains | depends on |
|---|---|---|
| `pkgcache` | daemon, CLI | nothing; the static binary it has always been |
| `pkgcache-desktop` | app, icon, launcher entry, login item | `pkgcache (= same version)`, GTK4, WebKitGTK 6.0 |

It is still one command, because apt resolves the dependency:

```sh
apt install pkgcache-desktop      # pulls pkgcache in with it
apt install pkgcache              # headless: no desktop stack
```

That needs a repository, and there are two. The public one at
`https://aabdlwahab.github.io/PKGCache/apt` is what a stranger installs from; a `pkgreg`
instance serves its own with `pkgreg publish-apt`, which is what `install.sh` configures
when you point it at a team cache. Both are the same tree, built by the same code.

The version lock is deliberate: the app talks to the daemon over a local API, and two
halves of one product drifting apart on a machine produces a bug report nobody can read.

## macOS

    ./packaging/macos/build-pkg.sh --version 1.0.0 \
        --server https://cache.internal:8443 --ca-sha256 AA:BB:...

Produces a `.pkg` that installs a universal `pkgcache`, `pkgcache-app` and
`pkgcache-docker` inside `/Applications/pkgcache.app`, with symlinks to all three from
`/usr/local/bin`. With `--server` the package configures the machine as it installs;
without one it still sets `--limit` (25G by default), because pkgcache refuses to start
without a disk budget and the last thing this package does is open the app.

`pkgcache-uninstall` removes the bundle, all four symlinks, the login item and the
installer receipt, and names the cache directory rather than deleting it. It is the same
list `bundle.sh --uninstall` removes, shipped inside the bundle so that somebody who was
handed a `.pkg` and nothing else can still remove the product.

It has to run on a Mac. A `.pkg` is a xar archive carrying a Bill of Materials in a binary
format that only Apple's tools write correctly, and a package with a subtly wrong BOM
installs and then misbehaves in ways that are hard to diagnose from the receiving end.
`pkgbuild` and `productbuild` come with the Command Line Tools, so any Mac that can build
the app — which needs cgo and the same tools — can build the installer.

Unsigned packages are refused by Finder with "unidentified developer". Either right-click →
Open, or `sudo installer -pkg pkgcache-1.0.0.pkg -target /`. To ship without that, pass
`--sign-app` and `--sign-pkg` and notarise — the script prints the commands.

## Ubuntu and Debian

From the public repository, which is what a person should be given:

    curl -fsSL https://aabdlwahab.github.io/PKGCache/apt/pkgcache-archive-keyring.asc \
      | sudo tee /usr/share/keyrings/pkgcache-archive-keyring.asc >/dev/null
    sudo tee /etc/apt/sources.list.d/pkgcache.sources >/dev/null <<'EOF'
    Types: deb
    URIs: https://aabdlwahab.github.io/PKGCache/apt
    Suites: stable
    Components: main
    Signed-By: /usr/share/keyrings/pkgcache-archive-keyring.asc
    EOF
    sudo apt update && sudo apt install pkgcache-desktop

Or from files you built yourself:

    make deb                      # from go/
    sudo apt install ./pkgcache_1.0.0_amd64.deb ./pkgcache-desktop_1.0.0_amd64.deb

From a repository it is one name and apt finds the other; from local files both are named
because apt cannot fetch a sibling that is not in a repository. Only the repository gets
upgrades: a file installed by hand stays the version it was until somebody downloads
another one.

`make deb` builds the desktop half only where a `bin/pkgcache-app-linux-<arch>` exists.
The app needs cgo, so a host that cannot build it still produces a complete daemon package
rather than failing — which is also what a release runner does for the architecture it is
not native to.

Built with `ar` and `tar` rather than `dpkg-deb`, so it cross-builds from any host, and
reproducibly: two builds of one binary are byte-identical, and that property is the reason
this is still a shell script rather than nfpm.

## Publishing the public repository

[`.github/workflows/pages.yml`](../.github/workflows/pages.yml) builds it, on every push
to `main` and on every published release. It downloads the `.deb` files from the newest
`pkgcache-v*` release, runs `pkgreg publish-apt` over them, and deploys the result beside
the site as `apt/`. No new code makes that work — it is the same publisher an instance
runs, pointed at a different directory.

Only the newest release is indexed, so the pool is about 45 MB and every deploy rebuilds
it from scratch. Nothing accumulates, nothing can drift, and no `.deb` enters git history
— which is what `.gitignore` has insisted on from the start. The trade is that
`apt install pkgcache=1.2.2` stops working the day 1.2.3 ships; widening it is a change to
how many releases that one step walks.

Two things had to be arranged by hand, once:

- **Pages is deployed from Actions**, not from the branch. Settings → Pages → Source →
  *GitHub Actions*. The workflow assembles `index.html`, `assets/`, `tutorial/` and
  `docs/` alongside the generated `apt/`, rather than the branch being served whole.
- **The signing key exists as the `APT_SIGNING_KEY` secret.** It was generated once with

      cd go && go run ./cmd/pkgreg publish-apt --data-dir /tmp/aptkey --print-key

  which creates `/tmp/aptkey/apt/signing-key.asc`, prints its fingerprint, and needs no
  packages to do it.

That key is the trust root of every machine that installs this way, and the reason
`loadOrCreateSigningKey` will never regenerate one: a new key means every machine that
trusted the old one stops trusting the repository, which is a far worse day than a missing
key. It is stored once, backed up offline, and not rotated. Anyone who can read it can
publish a package the world installs without complaint.

`scripts/verify-apt-repo.sh <base-url>` is what proves it works, and the workflow runs it
twice: against the assembled tree before deploying, and against the live URL afterwards.
The second run is the one that matters. It is the only thing that catches a failure living
in the host rather than in the files — a `Content-Encoding` applied to `Packages.gz` hands
apt decompressed bytes where it expected gzip, and the result is a hash mismatch on a
stranger's laptop with every file on the server perfect.

Until the first `pkgcache-v*` tag is pushed there is nothing to index, and the workflow
says so and deploys the site alone rather than failing.

## macOS: the app bundle

    cd go && make pkgcache && make app
    ./packaging/macos/bundle.sh --install

Three things exist only inside a bundle, and all three cost a debugging round while this
was being built: notifications, because `UNUserNotificationCenter` refuses to work without
a bundle identifier; the Dock and Launchpad icon, which comes from `CFBundleIconFile` and
nowhere else; and being launchable by `open -a` or Spotlight at all.

`pkgcache` goes *inside* the bundle, with symlinks into it from `/usr/local/bin`. An update
replaces the bundle, so both halves of the product move together — the app talks to the
daemon over a local API, and a mismatched pair is a bug report nobody can read.

`--uninstall` removes the bundle, both symlinks and the login item, and tells you where the
cache directory is rather than deleting it.

## Windows

    .\pkgcache-1.0.0-setup.exe

Per-user, so there is no UAC prompt and no administrator needed — everything lands under
`%LOCALAPPDATA%\Programs\pkgcache` and `HKCU`. It installs both binaries, a Start Menu
shortcut, an Add/Remove Programs entry, the PATH edit and optionally the login item, and
`makensis -DSERVER=… -DCASHA256=…` bakes in a cache so that installing is also the setup.

It also sets a disk budget, because pkgcache refuses to start without one and the first run
would otherwise show a webview error with no way to learn why. The default is `none` — no
cap, with the free-space floor still applying — which is the answer that guesses least
about somebody else's disk. `-DLIMIT=25G` changes it. Either way it is set only when the
machine has none: an upgrade never overwrites a limit somebody chose.

It checks for the WebView2 runtime, which is what the window is drawn by. Windows 11 ships
it and Edge installs it on 10, so it is nearly always there — and its absence is confusing
exactly because everything else works: the daemon runs, the CLI works, and clicking the
icon opens nothing with no error anywhere, because the app is linked `-H windowsgui` and
has no console to print to. Not fatal, and not a download the installer performs; the
cache is entirely usable from a terminal without a window.

The uninstaller removes the login entry and the PATH edit as well as the files, which are
the two things an uninstaller usually forgets and the two that leave a machine haunted.

amd64 only. Windows on ARM runs the x64 build under emulation, so this is a question of
speed rather than of whether it works, and a second installer nobody here can test is
worth less than the one that is known to.

`install.ps1` remains as the scriptable path, for CI and for anyone who would rather not
run an installer.

## The script, anywhere

    sh install.sh --server https://cache.internal:8443 --ca-sha256 AA:BB:...
    sh install.sh --from ./pkgcache-darwin-arm64      # a file you already have

On Linux, against a cache that publishes an apt repository, this installs the whole
product by installing `pkgcache-desktop`. Everywhere else — Linux without that repository,
and macOS always — it installs the `pkgcache` binary alone: no app, no shim, no menu bar
item, no uninstaller. `install.ps1` is the same deal on Windows. They are the scriptable
path, for CI and for anyone who would rather not run an installer, and the three
installers above are what a person is given.

## What every one of these does, and why

Each step is here because its absence broke a real installation:

- **The download is verified before anything is installed.** A truncated binary is still a
  valid-looking executable; it installs cleanly and is then killed by the kernel with no
  message but "killed". Verification happens first, and a mismatch leaves whatever was
  already installed untouched.
- **The binary is moved into place, never written over.** macOS caches a binary's code
  signature against its inode. Writing new bytes into the old inode leaves that cache
  describing a file that no longer exists, and every later run is killed.
- **The CA is pinned to a fingerprint given by a person.** A cache serving its own
  certificate cannot be verified by a machine that has not been told what to expect. The
  CA is fetched over an unverified connection, checked against the fingerprint, and only
  then used as the trust root for the download.
- **Quarantine is cleared** — `com.apple.quarantine` on macOS, Mark of the Web on Windows.
- **A disk budget is set where the machine has none.** pkgcache will not guess a size for
  somebody else's disk and refuses to start without one, which is right for the CLI —
  where the error names the command that answers it — and wrong for a product whose
  installer then opens a window. Each installer asks first, so an upgrade never overwrites
  a size somebody chose.
- **The licence travels with the software.** Apache-2.0 section 4 asks that whoever
  receives the work receives the licence and the NOTICE, and for a long time none of the
  three carried either. The NOTICE names the third-party modules linked into every binary
  that ships, not just the server's.
