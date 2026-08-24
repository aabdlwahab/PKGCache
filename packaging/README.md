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

That needs a repository, which `pkgreg publish-apt` serves from the instance the packages
came from — and which `install.sh` configures for you. The version lock is deliberate: the
app talks to the daemon over a local API, and two halves of one product drifting apart on
a machine produces a bug report nobody can read.

## macOS

    ./packaging/macos/build-pkg.sh --version 1.0.0 \
        --server https://cache.internal:8443 --ca-sha256 AA:BB:...

Produces a `.pkg` that installs a universal `pkgcache` into `/usr/local/bin`, the menu bar
item as `/Applications/pkgcache.app`, and a symlink so `pkgcache tray` finds the helper
beside itself. With `--server` the package configures the machine as it installs.

It has to run on a Mac. A `.pkg` is a xar archive carrying a Bill of Materials in a binary
format that only Apple's tools write correctly, and a package with a subtly wrong BOM
installs and then misbehaves in ways that are hard to diagnose from the receiving end.
`pkgbuild` and `productbuild` come with the same Command Line Tools that `swiftc` does, so
any Mac that can build the menu bar helper can build the installer.

Unsigned packages are refused by Finder with "unidentified developer". Either right-click →
Open, or `sudo installer -pkg pkgcache-1.0.0.pkg -target /`. To ship without that, pass
`--sign-app` and `--sign-pkg` and notarise — the script prints the commands.

## Ubuntu and Debian

    make deb                      # from go/
    sudo apt install ./pkgcache_1.0.0_amd64.deb ./pkgcache-desktop_1.0.0_amd64.deb

From a repository it is one name and apt finds the other; from local files both are named
because apt cannot fetch a sibling that is not in a repository.

`make deb` builds the desktop half only where a `bin/pkgcache-app-linux-<arch>` exists.
The app needs cgo, so a host that cannot build it still produces a complete daemon package
rather than failing — which is also what a release runner does for the architecture it is
not native to.

Built with `ar` and `tar` rather than `dpkg-deb`, so it cross-builds from any host, and
reproducibly: two builds of one binary are byte-identical, and that property is the reason
this is still a shell script rather than nfpm.

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

The uninstaller removes the login entry and the PATH edit as well as the files, which are
the two things an uninstaller usually forgets and the two that leave a machine haunted.

`install.ps1` remains as the scriptable path, for CI and for anyone who would rather not
run an installer.

## The script, anywhere

    sh install.sh --server https://cache.internal:8443 --ca-sha256 AA:BB:...
    sh install.sh --from ./pkgcache-darwin-arm64      # a file you already have

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
