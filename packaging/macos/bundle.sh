#!/bin/bash
# Build pkgcache.app — the macOS application bundle.
#
# This is what turns two binaries into something macOS treats as software. Three things
# only exist inside a bundle, and all three have bitten this project already:
#
#   - Notifications. UNUserNotificationCenter refuses to work without a bundle identifier,
#     so a bare binary cannot post one at all.
#   - The Dock and Launchpad icon, which comes from CFBundleIconFile and nowhere else. A
#     bare binary shows a generic placeholder however many icons it embeds.
#   - Being launchable. `open -a pkgcache`, Spotlight, and a login item all address a
#     bundle, not a path.
#
# pkgcache goes *inside* the bundle, with a symlink into it from /usr/local/bin. That is
# the same trick the menu bar helper used, for a better reason now: an update replaces the
# bundle, and both halves of the product have to move together or the app talks to a daemon
# from another build.
#
# usage:
#   ./bundle.sh                       # build into ./pkgcache.app
#   ./bundle.sh --install             # and copy it to /Applications
#   ./bundle.sh --uninstall           # remove it, the symlink and the login item
#
# options:
#   --app PATH        pkgcache-app binary   (default ../../go/bin/pkgcache-app-darwin-<arch>)
#   --daemon PATH     pkgcache binary       (default ../../go/bin/pkgcache)
#   --icon PATH       1024px PNG            (default ./appicon-1024.png)
#   --version V       CFBundleShortVersionString (default: git describe)
#   --sign IDENTITY   Developer ID Application identity
#   --out DIR         where to write the bundle (default .)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARCH="$(uname -m)"
case "$ARCH" in arm64) GOARCH=arm64 ;; x86_64) GOARCH=amd64 ;; *) GOARCH="$ARCH" ;; esac

APP="$HERE/../../go/bin/pkgcache-app-darwin-$GOARCH"
DAEMON="$HERE/../../go/bin/pkgcache"
ICON="$HERE/appicon-1024.png"
VERSION=""; SIGN=""; OUT="$HERE"; INSTALL=0; UNINSTALL=0
IDENTIFIER="org.pkgreg.pkgcache.app"
TARGET="/Applications/pkgcache.app"

die() { printf 'bundle: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--app) APP="$2"; shift 2 ;;
	--daemon) DAEMON="$2"; shift 2 ;;
	--icon) ICON="$2"; shift 2 ;;
	--version) VERSION="$2"; shift 2 ;;
	--sign) SIGN="$2"; shift 2 ;;
	--out) OUT="$2"; shift 2 ;;
	--install) INSTALL=1; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	-h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option $1" ;;
	esac
done

[ "$(uname -s)" = Darwin ] || die "this builds a macOS bundle and has to run on macOS"

# ---- uninstall ---------------------------------------------------------------------
#
# Everything this puts anywhere, taken out again. A program that installs six things owes
# a way to remove six things, and doing it here means the list cannot drift from the list
# above it.
if [ "$UNINSTALL" -eq 1 ]; then
	echo "==> stopping"
	pkill -x pkgcache-app 2>/dev/null || true
	[ -x /usr/local/bin/pkgcache ] && /usr/local/bin/pkgcache stop >/dev/null 2>&1 || true
	echo "==> removing the login item"
	[ -x /usr/local/bin/pkgcache-app ] &&
		/usr/local/bin/pkgcache-app -off-login >/dev/null 2>&1 || true
	rm -f "$HOME/Library/LaunchAgents/$IDENTIFIER-app.plist"
	echo "==> removing files"
	sudo rm -rf "$TARGET"
	sudo rm -f /usr/local/bin/pkgcache /usr/local/bin/pkgcache-app
	echo
	echo "Removed. Your cache directory was left alone:"
	echo "  $HOME/Library/Application Support/pkgcache"
	echo "Delete it yourself if you want the cached packages gone too."
	exit 0
fi

[ -f "$APP" ] || die "no app binary at $APP
Build it first:  cd ../../go && make app"
[ -f "$DAEMON" ] || die "no pkgcache binary at $DAEMON
Build it first:  cd ../../go && make pkgcache"
[ -f "$ICON" ] || die "no icon at $ICON"

if [ -z "$VERSION" ]; then
	VERSION="$(git -C "$HERE" describe --tags --always --dirty 2>/dev/null || echo 0.0.0)"
fi
SHORT="$(printf '%s' "$VERSION" | sed 's/^v//')"
# CFBundleVersion must be dot-separated digits, and a git description is not.
NUMERIC="$(printf '%s' "$SHORT" | tr -cd '0-9.' | sed 's/^\.*//; s/\.*$//')"
[ -n "$NUMERIC" ] || NUMERIC="0.0.0"

BUNDLE="$OUT/pkgcache.app"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/Contents/MacOS" "$BUNDLE/Contents/Resources"

echo "==> binaries"
install -m 0755 "$APP" "$BUNDLE/Contents/MacOS/pkgcache-app"
install -m 0755 "$DAEMON" "$BUNDLE/Contents/MacOS/pkgcache"

# ---- the icon ----------------------------------------------------------------------
#
# sips and iconutil rather than a committed .icns: the format wants ten sizes, and a
# generated one cannot drift from the logo the way a binary blob in the tree would.
echo "==> icon"
ICONSET="$(mktemp -d)/pkgcache.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
	sips -z "$size" "$size" "$ICON" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z "$double" "$double" "$ICON" \
		--out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$BUNDLE/Contents/Resources/pkgcache.icns"
rm -rf "$(dirname "$ICONSET")"

cat > "$BUNDLE/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>pkgcache</string>
	<key>CFBundleDisplayName</key><string>pkgcache</string>
	<!-- Load-bearing: UNUserNotificationCenter refuses to work without it, which is why
	     a bare binary cannot post a notification at all. -->
	<key>CFBundleIdentifier</key><string>$IDENTIFIER</string>
	<key>CFBundleExecutable</key><string>pkgcache-app</string>
	<key>CFBundleIconFile</key><string>pkgcache</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$SHORT</string>
	<key>CFBundleVersion</key><string>$NUMERIC</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<key>NSHighResolutionCapable</key><true/>
	<!-- Not an agent. The Swift helper this replaces was LSUIElement — menu bar only —
	     and this is a real application with a window, a Dock icon and a place in the app
	     switcher. That is the point of the exercise; see docs/client-app-plan.md. -->
</dict>
</plist>
PLIST

if [ -n "$SIGN" ]; then
	echo "==> signing"
	# Nested binaries first: a binary signed after its container invalidates the
	# container's signature.
	codesign --force --options runtime --timestamp -s "$SIGN" \
		"$BUNDLE/Contents/MacOS/pkgcache"
	codesign --force --options runtime --timestamp -s "$SIGN" \
		"$BUNDLE/Contents/MacOS/pkgcache-app"
	codesign --force --options runtime --timestamp -s "$SIGN" "$BUNDLE"
	codesign --verify --deep --strict --verbose=2 "$BUNDLE"
fi

echo
echo "built $BUNDLE"

if [ "$INSTALL" -eq 0 ]; then
	cat <<DONE

To install it:
  ./bundle.sh --install

Or try it where it is:
  open "$BUNDLE"
DONE
	exit 0
fi

# ---- install -----------------------------------------------------------------------
echo "==> installing to $TARGET"
# Removed and replaced rather than written over: macOS caches a code signature against a
# file's inode, and writing new bytes into an old one leaves every later run killed by the
# kernel with no message but "killed".
sudo rm -rf "$TARGET"
sudo cp -R "$BUNDLE" "$TARGET"
sudo xattr -dr com.apple.quarantine "$TARGET" 2>/dev/null || true

echo "==> putting both commands on PATH"
sudo mkdir -p /usr/local/bin
# Symlinks into the bundle, so an update that replaces the bundle moves both halves at
# once. The app talks to the daemon over a local API and a mismatched pair is a bug report
# nobody can read.
sudo ln -sf "$TARGET/Contents/MacOS/pkgcache" /usr/local/bin/pkgcache
sudo ln -sf "$TARGET/Contents/MacOS/pkgcache-app" /usr/local/bin/pkgcache-app

echo
/usr/local/bin/pkgcache version || die "the installed binary does not run"

cat <<DONE

Installed.

  open -a pkgcache            the window and the status bar icon
  pkgcache setup -limit 25G   give the cache a disk budget, once
  pkgcache-app -on-login      start the icon when you log in

  ./bundle.sh --uninstall     take all of it back out
DONE
