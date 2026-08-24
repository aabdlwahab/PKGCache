#!/bin/bash
# Build pkgcache.pkg — the macOS installer.
#
# This runs on a Mac, and that is a deliberate choice rather than a missing feature. A
# .pkg is a xar archive carrying a Bill of Materials in a binary format that only Apple's
# tools write correctly, and a package with a subtly wrong BOM installs and then misbehaves
# in ways that are very hard to diagnose from the receiving end. pkgbuild and productbuild
# ship with the Command Line Tools, as do sips and iconutil, so the Mac that builds the
# app can build the installer too.
#
# What the package installs:
#
#   /Applications/pkgcache.app                      the app, with pkgcache inside it
#   /usr/local/bin/pkgcache      -> into the bundle
#   /usr/local/bin/pkgcache-app  -> into the bundle
#
# Both binaries live inside the bundle and both symlinks point into it. That is not
# tidiness: an update replaces the bundle, so the daemon and the app move together. They
# talk over a local API, and this branch has already proved that a mismatched pair is a
# failure nobody can diagnose from the outside.
#
# The bundle itself is built by bundle.sh, which is also what somebody uses to install
# from a build tree. This wraps it in a .pkg for the people who want to double-click.
#
# usage:
#   ./build-pkg.sh --version 1.2.3
#   ./build-pkg.sh --server https://cache:8443 --ca-sha256 AA:BB:...   # self-configuring
#
# options:
#   --binary-arm64 PATH   pkgcache for Apple Silicon  (default ../../go/bin/pkgcache-darwin-arm64)
#   --binary-amd64 PATH   pkgcache for Intel          (default ../../go/bin/pkgcache-darwin-amd64)
#   --app-arm64 PATH      pkgcache-app for Apple Silicon (default ../../go/bin/pkgcache-app-darwin-arm64)
#   --app-amd64 PATH      pkgcache-app for Intel         (default ../../go/bin/pkgcache-app-darwin-amd64)
#   --version V           package version             (default: git describe, else 0.0.0)
#   --server URL          bake in a cache to configure on install
#   --ca-sha256 PIN       its CA fingerprint; required with --server
#   --limit SIZE          disk budget to configure    (default 25G)
#   --sign-app IDENTITY   Developer ID Application identity for the helper and app
#   --sign-pkg IDENTITY   Developer ID Installer identity for the .pkg
#   --out DIR             where to write the package  (default .)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARM64="$HERE/../../go/bin/pkgcache-darwin-arm64"
AMD64="$HERE/../../go/bin/pkgcache-darwin-amd64"
APP_ARM64="$HERE/../../go/bin/pkgcache-app-darwin-arm64"
APP_AMD64="$HERE/../../go/bin/pkgcache-app-darwin-amd64"
VERSION=""; SERVER=""; PIN=""; LIMIT="25G"
SIGN_APP=""; SIGN_PKG=""; OUT="."
IDENTIFIER="org.pkgreg.pkgcache"

die() { printf 'build-pkg: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--binary-arm64) ARM64="$2"; shift 2 ;;
	--binary-amd64) AMD64="$2"; shift 2 ;;
	--app-arm64) APP_ARM64="$2"; shift 2 ;;
	--app-amd64) APP_AMD64="$2"; shift 2 ;;
	--version) VERSION="$2"; shift 2 ;;
	--server) SERVER="$2"; shift 2 ;;
	--ca-sha256) PIN="$2"; shift 2 ;;
	--limit) LIMIT="$2"; shift 2 ;;
	--sign-app) SIGN_APP="$2"; shift 2 ;;
	--sign-pkg) SIGN_PKG="$2"; shift 2 ;;
	--out) OUT="$2"; shift 2 ;;
	-h|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option $1" ;;
	esac
done

[ "$(uname -s)" = Darwin ] || die "this builds a macOS installer and has to run on macOS"
command -v pkgbuild >/dev/null || die "pkgbuild is missing; install the Command Line Tools:
  xcode-select --install"
command -v iconutil >/dev/null || die "iconutil is missing; install the Command Line Tools:
  xcode-select --install"
[ -n "$SERVER" ] && [ -z "$PIN" ] && die "--server needs --ca-sha256"

if [ -z "$VERSION" ]; then
	VERSION="$(git -C "$HERE" describe --tags --always --dirty 2>/dev/null || echo 0.0.0)"
fi
# CFBundleVersion must be dot-separated digits, and a git description is not. The bundle
# gets a sanitised number; the human-readable string keeps what was asked for.
SHORT="$(printf '%s' "$VERSION" | sed 's/^v//')"
NUMERIC="$(printf '%s' "$SHORT" | tr -cd '0-9.' | sed 's/^\.*//; s/\.*$//')"
[ -n "$NUMERIC" ] || NUMERIC="0.0.0"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM
ROOT="$WORK/root"
SCRIPTS="$WORK/scripts"
APP="$ROOT/Applications/pkgcache.app"
mkdir -p "$ROOT/usr/local/bin" "$APP/Contents/MacOS" "$APP/Contents/Resources" "$SCRIPTS"

# ---- pkgcache itself, universal where possible -----------------------------------
echo "==> pkgcache"
if [ -f "$ARM64" ] && [ -f "$AMD64" ]; then
	# One package for both architectures rather than two downloads and a question the
	# person installing should not have to answer.
	lipo -create "$ARM64" "$AMD64" -output "$ROOT/usr/local/bin/pkgcache"
	echo "    universal: $(lipo -archs "$ROOT/usr/local/bin/pkgcache")"
elif [ -f "$ARM64" ]; then
	cp "$ARM64" "$ROOT/usr/local/bin/pkgcache"; echo "    arm64 only"
elif [ -f "$AMD64" ]; then
	cp "$AMD64" "$ROOT/usr/local/bin/pkgcache"; echo "    amd64 only"
else
	die "no pkgcache binary found.
Looked for:
  $ARM64
  $AMD64
Build them with \`make pkgcache-release\` and copy bin/pkgcache-darwin-* here."
fi
chmod 755 "$ROOT/usr/local/bin/pkgcache"

# ---- the app ----------------------------------------------------------------------
echo "==> the app"
BINARY="$APP/Contents/MacOS/pkgcache-app"
if [ -f "$APP_ARM64" ] && [ -f "$APP_AMD64" ]; then
	lipo -create "$APP_ARM64" "$APP_AMD64" -output "$BINARY"
	echo "    universal: $(lipo -archs "$BINARY")"
elif [ -f "$APP_ARM64" ]; then
	cp "$APP_ARM64" "$BINARY"; echo "    arm64 only"
elif [ -f "$APP_AMD64" ]; then
	cp "$APP_AMD64" "$BINARY"; echo "    amd64 only"
else
	die "no pkgcache-app binary found.
Looked for:
  $APP_ARM64
  $APP_AMD64
The app needs cgo, so it is built natively rather than cross-compiled:
  cd ../../go && make app"
fi
chmod 755 "$BINARY"

# pkgcache goes inside the bundle too, so an update moves both halves at once. The copy
# in /usr/local/bin below is a symlink into here rather than a second file.
cp "$ROOT/usr/local/bin/pkgcache" "$APP/Contents/MacOS/pkgcache"
chmod 755 "$APP/Contents/MacOS/pkgcache"
rm -f "$ROOT/usr/local/bin/pkgcache"

# ---- the icon ---------------------------------------------------------------------
#
# Generated from the same SVG as every other icon in this project. sips and iconutil are
# Command Line Tools, so this needs no third-party image tooling.
echo "==> icon"
ICON="${ICON:-$HERE/appicon-1024.png}"
[ -f "$ICON" ] || die "no icon at $ICON"
ICONSET="$WORK/pkgcache.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
	sips -z "$size" "$size" "$ICON" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z "$double" "$double" "$ICON" \
		--out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/pkgcache.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>pkgcache</string>
	<key>CFBundleDisplayName</key><string>pkgcache</string>
	<key>CFBundleIdentifier</key><string>$IDENTIFIER.app</string>
	<key>CFBundleExecutable</key><string>pkgcache-app</string>
	<key>CFBundleIconFile</key><string>pkgcache</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$SHORT</string>
	<key>CFBundleVersion</key><string>$NUMERIC</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<!-- No LSUIElement, deliberately. The Swift helper this replaces was an agent —
	     menu bar only, no Dock icon, no place in the app switcher. This is a real
	     application with a window, which is the point of the exercise; see
	     docs/client-app-plan.md. -->
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

# Both resolve into the bundle. See the note at the top: one update, both halves.
ln -s /Applications/pkgcache.app/Contents/MacOS/pkgcache \
	"$ROOT/usr/local/bin/pkgcache"
ln -s /Applications/pkgcache.app/Contents/MacOS/pkgcache-app \
	"$ROOT/usr/local/bin/pkgcache-app"

# ---- what happens after the files land --------------------------------------------
cat > "$SCRIPTS/postinstall" <<POSTINSTALL
#!/bin/bash
# Runs as root. Anything touching the person's own cache has to drop back to them:
# pkgcache keeps its state under the user's home, and configuring root's copy would
# leave the actual user with nothing and no sign of why.
set -u
CONSOLE_USER="\$(stat -f%Su /dev/console 2>/dev/null || echo root)"
run_as_user() {
	if [ "\$CONSOLE_USER" = root ] || [ -z "\$CONSOLE_USER" ]; then
		"\$@"
	else
		# -H matters more than it looks: without it HOME stays /var/root, and
		# pkgcache would configure root's cache while the person who installed it
		# sees nothing and is told everything worked.
		/usr/bin/sudo -H -u "\$CONSOLE_USER" "\$@"
	fi
}

# Installed from a package rather than downloaded, so this is normally a no-op. It is
# here for the copy that arrives on a USB stick and picks up a quarantine flag on the way.
/usr/bin/xattr -dr com.apple.quarantine /Applications/pkgcache.app 2>/dev/null || true
/usr/bin/xattr -d com.apple.quarantine /usr/local/bin/pkgcache 2>/dev/null || true

echo "pkgcache \$(/usr/local/bin/pkgcache version 2>/dev/null || echo installed)"
POSTINSTALL

if [ -n "$SERVER" ]; then
	cat >> "$SCRIPTS/postinstall" <<POSTINSTALL

# Baked in at build time: this package was made for one cache, so the machine it lands
# on is pointed at that cache instead of being left to be configured by hand.
echo "pointing this machine at $SERVER"
run_as_user /usr/local/bin/pkgcache setup \\
	-server "$SERVER" -ca-sha256 "$PIN" -limit "$LIMIT" || {
	echo "pkgcache was installed but could not be configured." >&2
	echo "Run it yourself:" >&2
	echo "  pkgcache setup -server $SERVER -ca-sha256 $PIN -limit $LIMIT" >&2
}
POSTINSTALL
fi

cat >> "$SCRIPTS/postinstall" <<'POSTINSTALL'

# A menu bar item that has to be started from a terminal is not a menu bar item. It is
# registered to start at login and opened now, so installing is the whole of it.
run_as_user /usr/local/bin/pkgcache-app -on-login >/dev/null 2>&1 ||
	echo "could not register the login item; run: pkgcache-app -on-login" >&2

# launchctl asuser is what puts this in the person's GUI session. Plain sudo would start
# it in root's, where a menu bar has nowhere to appear.
CONSOLE_UID="$(id -u "$CONSOLE_USER" 2>/dev/null || echo 0)"
if [ "$CONSOLE_UID" != 0 ]; then
	/bin/launchctl asuser "$CONSOLE_UID" /usr/bin/sudo -u "$CONSOLE_USER" \
		/usr/bin/open -a /Applications/pkgcache.app >/dev/null 2>&1 ||
		echo "installed, but could not open it; open pkgcache from Applications" >&2
fi

echo ""
echo "pkgcache is in your menu bar, and will be there when you log in."
echo "To stop that:  pkgcache-app -off-login"
exit 0
POSTINSTALL
chmod 755 "$SCRIPTS/postinstall"

# ---- signing ----------------------------------------------------------------------
if [ -n "$SIGN_APP" ]; then
	echo "==> signing the app"
	# The helper is signed inside the bundle, then the bundle itself: a nested binary
	# signed after its container invalidates the container's signature.
	codesign --force --options runtime --timestamp -s "$SIGN_APP" \
		"$APP/Contents/MacOS/pkgcache"
	codesign --force --options runtime --timestamp -s "$SIGN_APP" \
		"$APP/Contents/MacOS/pkgcache-app"
	codesign --force --options runtime --timestamp -s "$SIGN_APP" "$APP"
	codesign --verify --deep --strict --verbose=2 "$APP"
fi

# ---- the package -------------------------------------------------------------------
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
PKG="$OUT/pkgcache-$SHORT.pkg"
echo "==> building $PKG"
COMPONENT="$WORK/component.pkg"

# Relocation off, deliberately.
#
# pkgbuild marks every bundle it finds as relocatable, which means Installer asks the
# system whether a copy of this bundle identifier already exists anywhere and, if one does,
# installs the update *there* instead of where the payload says. For an ordinary app that
# is a courtesy to somebody who keeps their apps in a different folder. Here it is a trap:
# /usr/local/bin/pkgcache and /usr/local/bin/pkgcache-app are absolute symlinks into
# /Applications/pkgcache.app, so an install redirected anywhere else leaves both dangling:
# no cache, no app, and no stderr anybody reads because a GUI launch has none.
cat > "$WORK/component.plist" <<COMPONENTPLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
	<dict>
		<key>BundleHasStrictIdentifier</key><true/>
		<key>BundleIsRelocatable</key><false/>
		<key>BundleIsVersionChecked</key><true/>
		<key>BundleOverwriteAction</key><string>upgrade</string>
		<key>RootRelativeBundlePath</key><string>Applications/pkgcache.app</string>
	</dict>
</array>
</plist>
COMPONENTPLIST

pkgbuild \
	--root "$ROOT" \
	--scripts "$SCRIPTS" \
	--identifier "$IDENTIFIER" \
	--version "$NUMERIC" \
	--install-location / \
	--component-plist "$WORK/component.plist" \
	"$COMPONENT"

cat > "$WORK/distribution.xml" <<DIST
<?xml version="1.0" encoding="utf-8"?>
<installer-gui-script minSpecVersion="2">
	<title>pkgcache</title>
	<organization>org.pkgreg</organization>
	<options customize="never" require-scripts="false" hostArchitectures="arm64,x86_64"/>
	<volume-check>
		<allowed-os-versions><os-version min="11.0"/></allowed-os-versions>
	</volume-check>
	<choices-outline><line choice="default"/></choices-outline>
	<choice id="default" title="pkgcache">
		<pkg-ref id="$IDENTIFIER"/>
	</choice>
	<pkg-ref id="$IDENTIFIER" version="$NUMERIC" onConclusion="none">component.pkg</pkg-ref>
</installer-gui-script>
DIST

if [ -n "$SIGN_PKG" ]; then
	productbuild --distribution "$WORK/distribution.xml" --package-path "$WORK" \
		--sign "$SIGN_PKG" "$PKG"
else
	productbuild --distribution "$WORK/distribution.xml" --package-path "$WORK" "$PKG"
fi

echo ""
echo "built $PKG"
shasum -a 256 "$PKG"
if [ -z "$SIGN_PKG" ]; then
	cat <<'UNSIGNED'

This package is unsigned. macOS will refuse to open it from Finder with "unidentified
developer"; the person installing can right-click it and choose Open, or run:

  sudo installer -pkg <the .pkg> -target /

To ship it without that, build with --sign-app and --sign-pkg using Developer ID
identities, then notarise:

  xcrun notarytool submit <the .pkg> --apple-id … --team-id … --wait
  xcrun stapler staple <the .pkg>
UNSIGNED
fi
