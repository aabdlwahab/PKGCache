#!/bin/sh
# Verify a published pkgcache apt repository, from the outside.
#
# Everything else that checks this repository checks it from the inside: `pkgreg doctor`
# reads the files on disk and confirms the signature covers the indexes and the indexes
# match their hashes. That is the right check for an operator and it is not this one.
#
# The failures this catches are the ones that live between the files and the client:
#
#   - a host that applies Content-Encoding to Packages.gz, so apt receives decompressed
#     bytes where it expected gzip and reports a hash mismatch on somebody's laptop while
#     every file on the server is perfect;
#   - a pool path that 404s because the origin serves a subdirectory the indexes do not
#     know about;
#   - a keyring that is not published beside the repository it verifies, which turns the
#     documented install into `[trusted=yes]` on every machine that tries it.
#
# So it does what the documentation tells a person to do, against a URL, on a machine that
# has never seen this repository — and then puts the machine back as it found it.
#
# usage: verify-apt-repo.sh <base-url>
#
#   verify-apt-repo.sh https://aabdlwahab.github.io/PKGCache/apt
#   verify-apt-repo.sh http://127.0.0.1:8000/apt      # a tree served before it is deployed
set -eu

BASE="${1:?usage: verify-apt-repo.sh <base-url>}"
BASE="${BASE%/}"

command -v apt-get >/dev/null 2>&1 || {
	echo "verify-apt-repo: this needs a Debian or Ubuntu machine; apt-get is not here" >&2
	exit 1
}
command -v curl >/dev/null 2>&1 || {
	echo "verify-apt-repo: curl is not here, and the keyring is fetched with it" >&2
	exit 1
}

if [ "$(id -u)" = 0 ]; then SUDO=""
elif command -v sudo >/dev/null 2>&1; then SUDO="sudo"
else echo "verify-apt-repo: this needs root or sudo" >&2; exit 1; fi

KEYRING=/usr/share/keyrings/pkgcache-archive-keyring.asc
SOURCE=/etc/apt/sources.list.d/pkgcache.sources

# Removed however this exits, including the failure paths. A verification that leaves a
# source file behind turns the next unrelated `apt update` on this machine into a puzzle.
cleanup() {
	$SUDO apt-get remove -y pkgcache pkgcache-desktop >/dev/null 2>&1 || true
	$SUDO rm -f "$SOURCE" "$KEYRING"
}
trap cleanup EXIT INT TERM

echo "verifying $BASE"

# The keyring, fetched exactly as the documentation says to fetch it. Its absence is a
# complete failure of the repository and is worth its own message: everything below would
# otherwise fail with apt's version, which talks about signatures rather than about a
# missing file.
curl -fsSL "$BASE/pkgcache-archive-keyring.asc" | $SUDO tee "$KEYRING" >/dev/null || {
	echo "verify-apt-repo: no keyring at $BASE/pkgcache-archive-keyring.asc" >&2
	exit 1
}

printf '%s\n' \
	"# Written by verify-apt-repo.sh. Removed when it finishes." \
	"Types: deb" \
	"URIs: $BASE" \
	"Suites: stable" \
	"Components: main" \
	"Signed-By: $KEYRING" |
	$SUDO tee "$SOURCE" >/dev/null

# ---- the assertion, scoped to this one source ------------------------------------
#
# Scoped because an unrelated repository the machine happens to have would otherwise fail
# this script and be reported as a fault in the one it is testing. This is the check that
# the signature verifies and that every index matches the hash Release vouches for.
echo
echo "--- apt-get update, this repository only ---"
$SUDO apt-get update \
	-o Dir::Etc::sourcelist=sources.list.d/pkgcache.sources \
	-o Dir::Etc::sourceparts=/dev/null \
	-o APT::Get::List-Cleanup=0

# And now the machine's own lists, which the dependency resolution below needs: the
# desktop package depends on GTK and a WebKit that come from the distribution, and a
# runner image usually ships with no package lists at all.
echo
echo "--- apt-get update, everything ---"
$SUDO apt-get update

# ---- the daemon: proves the pool is fetchable ------------------------------------
#
# pkgcache has no dependencies at all, so this installs the package and nothing else. A
# hash mismatch on the .deb itself lands here rather than in the index check above.
echo
echo "--- apt-get install pkgcache ---"
$SUDO apt-get install -y pkgcache
# `version`, not `--version`: pkgcache dispatches on a subcommand and treats an unknown
# one as an error, so the flag spelling would exit 2 and fail this script on a repository
# that is perfectly good.
pkgcache version

# ---- the app: proves the version lock resolves -----------------------------------
#
# Simulated, not installed. What is being tested is that apt can satisfy
# `pkgcache (= <this version>)` together with the graphics stack — and actually pulling
# that stack onto a CI runner to learn it would take minutes and prove nothing more.
echo
echo "--- apt-get install -s pkgcache-desktop ---"
$SUDO apt-get install -s pkgcache-desktop

echo
echo "verify-apt-repo: $BASE installs"
