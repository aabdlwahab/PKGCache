#!/bin/sh
# Build a .deb for pkgcache, without dpkg-deb.
#
# A .deb is an ar archive of exactly three members in order: debian-binary, then
# control.tar.gz, then data.tar.gz. Building it with ar and tar means this runs on the
# machine that already cross-compiles every other target, rather than requiring a Debian
# host to package a static binary that has no Debian dependencies anyway.
#
# usage: build.sh <binary> <arch> <version> [outdir]
set -eu

BINARY="${1:?usage: build.sh <binary> <arch> <version> [outdir]}"
ARCH="${2:?arch: amd64 or arm64}"
VERSION="${3:?version}"
OUT="${4:-.}"

[ -f "$BINARY" ] || { echo "no such binary: $BINARY" >&2; exit 1; }
case "$ARCH" in amd64|arm64) ;; *) echo "arch must be amd64 or arm64" >&2; exit 1 ;; esac

# A Debian version may not carry a leading 'v' or any '-' beyond the revision, and our
# build stamps look like "23888d5-dirty". Normalised rather than rejected: the package
# should be buildable from a working tree, and the git description is the useful part.
DEBVER="$(printf '%s' "$VERSION" | sed 's/^v//; s/-/+/g')"
[ -n "$DEBVER" ] || DEBVER="0"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM
ROOT="$WORK/root"
CTRL="$WORK/control"
mkdir -p "$ROOT/usr/bin" "$ROOT/usr/share/applications" \
	"$ROOT/usr/share/doc/pkgcache" "$CTRL"

install -m 0755 "$BINARY" "$ROOT/usr/bin/pkgcache"

# The window helper is optional: it needs cgo and WebKitGTK, so it is not built on every
# host. When it is absent pkgcache falls back to a browser tab, which is why this is a
# Recommends below and not a Depends.
if [ -n "${PKGCACHE_WINDOW:-}" ] && [ -f "$PKGCACHE_WINDOW" ]; then
	install -m 0755 "$PKGCACHE_WINDOW" "$ROOT/usr/bin/pkgcache-window"
fi

cat > "$ROOT/usr/share/applications/pkgcache.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=pkgcache
GenericName=Package cache
Comment=Keep this machine's package downloads in your status bar
Exec=pkgcache tray
Icon=pkgcache
Terminal=false
Categories=Development;Utility;
Keywords=cache;packages;docker;pip;npm;
StartupNotify=false
DESKTOP

cat > "$ROOT/usr/share/doc/pkgcache/copyright" <<'COPY'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: pkgcache
COPY

# Debian expects the changelog to be compressed, and lintian complains either way about
# a stub. It is here because its absence is a policy violation, not because it is read.
# The date comes from SOURCE_DATE_EPOCH, or is fixed, so two builds of one binary
# produce identical packages. `date -R` here would put the clock in the archive and make
# every rebuild differ.
STAMP="$(date -R -u -d "@${SOURCE_DATE_EPOCH:-0}" 2>/dev/null || date -u -r "${SOURCE_DATE_EPOCH:-0}" +"%a, %d %b %Y %H:%M:%S +0000")"
printf 'pkgcache (%s) stable; urgency=low\n\n  * Built from source.\n\n -- pkgreg <root@localhost>  %s\n' \
	"$DEBVER" "$STAMP" | gzip -9n > "$ROOT/usr/share/doc/pkgcache/changelog.Debian.gz"

INSTALLED_KB="$(du -sk "$ROOT" | cut -f1)"

cat > "$CTRL/control" <<CONTROL
Package: pkgcache
Version: $DEBVER
Section: devel
Priority: optional
Architecture: $ARCH
Maintainer: pkgreg <root@localhost>
Installed-Size: $INSTALLED_KB
Recommends: libwebkit2gtk-4.1-0, libgtk-3-0
Description: Package cache for one machine
 pkgcache keeps this machine's package downloads — pip, npm, Docker images, apt and
 git — on local disk, and can sit in front of a team cache so a download crosses the
 network once for everyone rather than once for each person.
 .
 It is a single static binary with no runtime dependencies. The recommended GTK and
 WebKit libraries are needed only for the native window; without them the console
 opens in a browser tab instead.
CONTROL

# md5sums lets `dpkg -V` verify the package after installation.
( cd "$ROOT" && find . -type f -printf '%P\0' | sort -z |
	xargs -0 -r md5sum > "$CTRL/md5sums" ) 2>/dev/null || \
( cd "$ROOT" && find . -type f | sed 's|^\./||' | sort | while read -r f; do
	md5sum "$f"; done > "$CTRL/md5sums" )

cat > "$CTRL/postinst" <<'POSTINST'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
	echo "pkgcache installed. Point it at your team cache with:"
	echo "  pkgcache setup -server https://<cache>:8443 -ca-sha256 <fingerprint> -limit 25G"
	echo "Then keep it in your status bar with: pkgcache tray"
fi
POSTINST
chmod 0755 "$CTRL/postinst"

printf '2.0\n' > "$WORK/debian-binary"

# --sort=name and a fixed mtime make the package reproducible: two builds of the same
# binary produce byte-identical .deb files, so a checksum means something.
TAROPTS="--owner=0 --group=0 --numeric-owner --sort=name --mtime=@0"
# shellcheck disable=SC2086
tar $TAROPTS -czf "$WORK/data.tar.gz" -C "$ROOT" ./usr
# shellcheck disable=SC2086
tar $TAROPTS -czf "$WORK/control.tar.gz" -C "$CTRL" ./control ./md5sums ./postinst

mkdir -p "$OUT"
# Resolved before anything changes directory: the ar call runs inside $WORK, and a
# relative output directory would be relative to that instead of to the caller.
OUT="$(cd "$OUT" && pwd)"
DEB="$OUT/pkgcache_${DEBVER}_${ARCH}.deb"
rm -f "$DEB"
# The member order is part of the format, not a convention. -D is deterministic mode:
# without it ar stamps each member with the clock and the builder's uid, and two builds
# of one binary would differ for no reason anybody can act on.
( cd "$WORK" && ar rcD "$DEB" debian-binary control.tar.gz data.tar.gz )
echo "$DEB"
