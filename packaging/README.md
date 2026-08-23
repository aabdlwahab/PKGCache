# Installing pkgcache

Three installers, one per platform. Each verifies what it installs before installing it,
and each can point the machine at a team cache as part of the install rather than leaving
that as a step to remember.

| platform | artefact | built by |
|---|---|---|
| macOS | `pkgcache-<version>.pkg` | `packaging/macos/build-pkg.sh`, **on a Mac** |
| Ubuntu / Debian | `pkgcache_<version>_<arch>.deb` | `make deb` |
| Windows | `install.ps1` | `make installers` |
| macOS / Linux (script) | `install.sh` | `make installers` |

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
    sudo apt install ./pkgcache_1.0.0_amd64.deb

Ships the binary and a desktop entry. GTK and WebKit are `Recommends`, not `Depends`:
without them the console opens in a browser tab instead of a native window. Built with `ar`
and `tar` rather than `dpkg-deb`, so it cross-builds from any host, and reproducibly — two
builds of one binary are byte-identical.

## Windows

    .\install.ps1 -Server https://cache.internal:8443 -CaSha256 AA:BB:...

Installs into `%LOCALAPPDATA%\Programs\pkgcache` and adds it to the user's PATH, so no
elevation is needed.

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
