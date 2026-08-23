#!/bin/sh
# pkgcache installer for macOS and Linux.
#
# Every step here exists because its absence broke a real installation:
#
#   - The download is verified against a SHA-256 before anything is installed. A
#     truncated copy is still a valid Mach-O header, so it installs cleanly and is then
#     killed by the kernel with no explanation but "killed".
#   - The binary is moved into place, never written over. macOS caches a binary's code
#     signature against its inode; writing new bytes into the old inode leaves the cache
#     describing a file that no longer exists, and every later run is killed. A move
#     replaces the directory entry, so the new file gets a new inode and no stale cache.
#   - The server's CA is pinned to a fingerprint given on the command line. A cache
#     serving its own certificate cannot be verified by a machine that has not been told
#     what to expect, and "trust whatever answers this address" is not an installer.
#
# usage:
#   install.sh --server https://cache:8443 --ca-sha256 AA:BB:...   # from a team cache
#   install.sh --from ./pkgcache-darwin-arm64                      # from a local file
#   install.sh --from https://host/pkgcache-linux-amd64            # from a plain URL
#
# options:
#   --prefix DIR    where to install (default: /usr/local/bin, or ~/.local/bin
#                   when that is not writable and sudo is unavailable)
#   --limit SIZE    disk budget to configure, e.g. 25G (default: 25G)
#   --sha256 HEX    expected checksum when --from cannot supply one
#   --no-configure  install only; do not run `pkgcache setup`
set -eu

SERVER=""; PIN=""; FROM=""; PREFIX=""; LIMIT="25G"; WANT_SUM=""; CONFIGURE=1
TOOL="pkgcache"

die() { printf 'pkgcache install: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--server) SERVER="${2:-}"; shift 2 ;;
	--ca-sha256) PIN="${2:-}"; shift 2 ;;
	--from) FROM="${2:-}"; shift 2 ;;
	--prefix) PREFIX="${2:-}"; shift 2 ;;
	--limit) LIMIT="${2:-}"; shift 2 ;;
	--sha256) WANT_SUM="${2:-}"; shift 2 ;;
	--no-configure) CONFIGURE=0; shift ;;
	-h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option $1" ;;
	esac
done

[ -n "$SERVER" ] || [ -n "$FROM" ] || die "give --server (with --ca-sha256) or --from"

# ---- the machine ----------------------------------------------------------------
case "$(uname -s)" in
Darwin) OS=darwin ;;
Linux) OS=linux ;;
*) die "unsupported system $(uname -s); this installs macOS and Linux builds" ;;
esac
case "$(uname -m)" in
x86_64|amd64) ARCH=amd64 ;;
arm64|aarch64) ARCH=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac
note "pkgcache installer — $OS/$ARCH"

# ---- tools ----------------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then GET="curl"
elif command -v wget >/dev/null 2>&1; then GET="wget"
else die "neither curl nor wget is available"; fi

sum256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
	else die "no sha256sum or shasum; cannot verify the download"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

# fetch URL DEST [CAFILE|insecure]
fetch() {
	_url="$1"; _dest="$2"; _ca="${3:-}"
	if [ "$GET" = curl ]; then
		set -- -fsSL --proto '=https,http' -o "$_dest"
		case "$_ca" in
		insecure) set -- "$@" -k ;;
		"") ;;
		*) set -- "$@" --cacert "$_ca" ;;
		esac
		curl "$@" "$_url"
	else
		set -- -q -O "$_dest"
		case "$_ca" in
		insecure) set -- "$@" --no-check-certificate ;;
		"") ;;
		*) set -- "$@" --ca-certificate="$_ca" ;;
		esac
		wget "$@" "$_url"
	fi
}

# normalise a fingerprint for comparison: no colons, lower case.
norm() { printf '%s' "$1" | tr 'A-F' 'a-f' | tr -d ': \t\r\n'; }

BINARY="$WORK/$TOOL"

if [ -n "$FROM" ]; then
	case "$FROM" in
	http://*|https://*) note "downloading $FROM"; fetch "$FROM" "$BINARY" ;;
	*) [ -f "$FROM" ] || die "no such file: $FROM"; cp "$FROM" "$BINARY" ;;
	esac
	if [ -n "$WANT_SUM" ]; then
		GOT="$(sum256 "$BINARY")"
		[ "$(norm "$GOT")" = "$(norm "$WANT_SUM")" ] ||
			die "checksum mismatch: got $GOT, expected $WANT_SUM"
		note "checksum verified"
	else
		note "no --sha256 given, so the download is unverified"
	fi
else
	# ---- trust the cache before taking anything from it ---------------------------
	[ -n "$PIN" ] || die "--server needs --ca-sha256; ask whoever runs the cache for it"
	SERVER="${SERVER%/}"
	CA="$WORK/ca.crt"
	note "fetching the CA from $SERVER"
	fetch "$SERVER/api/ca.crt" "$CA" insecure ||
		die "could not reach $SERVER/api/ca.crt"
	# The CA is fetched over an unverified connection and is then verified itself: its
	# fingerprint has to equal the one given on the command line, which came from a
	# person rather than from the network. Everything after this uses it as the trust
	# root, so a substituted CA fails here rather than becoming trusted.
	CA_SUM="$(openssl x509 -in "$CA" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2)"
	[ -n "$CA_SUM" ] || CA_SUM="$(sum256 "$CA")"
	[ "$(norm "$CA_SUM")" = "$(norm "$PIN")" ] || die "the CA at $SERVER does not match the
fingerprint you gave.
  served:   $CA_SUM
  expected: $PIN
Nothing was installed. Either the fingerprint is stale or this is not the cache you meant."
	note "CA verified against the fingerprint you gave"

	note "asking $SERVER what it publishes"
	LIST="$WORK/downloads.json"
	fetch "$SERVER/api/v1/downloads" "$LIST" "$CA" || die "could not list downloads"

	# One line per binary, so this needs no JSON parser on the target machine.
	FIELDS="$(tr '{' '\n' < "$LIST" | grep "\"tool\":\"$TOOL\"" |
		grep "\"os\":\"$OS\"" | grep "\"arch\":\"$ARCH\"" | head -1)"
	[ -n "$FIELDS" ] && NAME="$(printf '%s' "$FIELDS" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')"
	[ -n "${NAME:-}" ] || die "$SERVER publishes no $TOOL build for $OS/$ARCH.
Whoever runs it publishes them with \`pkgreg publish-client\`.
Meanwhile you can install a file you already have:
  $0 --from ./pkgcache-$OS-$ARCH"
	WANT_SUM="$(printf '%s' "$FIELDS" | sed -n 's/.*"sha256":"\([^"]*\)".*/\1/p')"

	note "downloading $NAME"
	fetch "$SERVER/api/v1/downloads/$NAME" "$BINARY" "$CA" || die "download failed"
	[ -n "$WANT_SUM" ] || die "the cache published $NAME without a checksum"
	GOT="$(sum256 "$BINARY")"
	[ "$(norm "$GOT")" = "$(norm "$WANT_SUM")" ] || die "checksum mismatch — the download is
corrupt or truncated.
  got:      $GOT
  expected: $WANT_SUM
Nothing was installed. Run this again."
	note "checksum verified: $GOT"
fi

# ---- install ---------------------------------------------------------------------
chmod 755 "$BINARY"

if [ -z "$PREFIX" ]; then
	PREFIX=/usr/local/bin
	if [ ! -w "$PREFIX" ] && ! command -v sudo >/dev/null 2>&1; then
		PREFIX="$HOME/.local/bin"
		note "no write access to /usr/local/bin and no sudo; installing to $PREFIX"
	fi
fi
mkdir -p "$PREFIX" 2>/dev/null || SUDO=sudo
SUDO="${SUDO:-}"
[ -w "$PREFIX" ] || SUDO=sudo
[ -z "$SUDO" ] || command -v sudo >/dev/null 2>&1 || die "$PREFIX is not writable and sudo is missing"

DEST="$PREFIX/$TOOL"
${SUDO:+$SUDO} mkdir -p "$PREFIX"
# Moved, not copied over: see the note at the top about inodes and signature caches.
${SUDO:+$SUDO} rm -f "$DEST"
${SUDO:+$SUDO} cp "$BINARY" "$DEST.new"
${SUDO:+$SUDO} chmod 755 "$DEST.new"
${SUDO:+$SUDO} mv -f "$DEST.new" "$DEST"

if [ "$OS" = darwin ]; then
	# Present on anything that arrived through a browser; harmless when absent.
	${SUDO:+$SUDO} xattr -d com.apple.quarantine "$DEST" 2>/dev/null || true
fi

command -v hash >/dev/null 2>&1 && hash -r 2>/dev/null || true
note "installed $DEST"
"$DEST" version || die "the installed binary does not run"

# ---- point it at the cache it came from -------------------------------------------
if [ -n "$SERVER" ] && [ "$CONFIGURE" -eq 1 ]; then
	note ""
	note "pointing this machine at $SERVER"
	"$DEST" setup -server "$SERVER" -ca-sha256 "$PIN" -limit "$LIMIT"
fi

case ":$PATH:" in
*":$PREFIX:"*) ;;
*) note ""; note "$PREFIX is not on your PATH. Add it:"; note "  export PATH=\"$PREFIX:\$PATH\"" ;;
esac
