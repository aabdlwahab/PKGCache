#!/bin/sh
# Build the pkgcache .deb packages, without dpkg-deb.
#
# A .deb is an ar archive of exactly three members in order: debian-binary, then
# control.tar.gz, then data.tar.gz. Building it with ar and tar means this runs on the
# machine that already cross-compiles every other target, rather than requiring a Debian
# host to package a static binary that has no Debian dependencies anyway.
#
# Four packages, and the reason for each split is different:
#
#   pkgcache               the daemon and the CLI. One static binary, no dependencies, and
#                          the only thing a CI runner, a build box or a container should
#                          install.
#   pkgcache-desktop       a metapackage. Depends on one of the two below, whichever this
#                          release can actually run, and carries nothing itself.
#   pkgcache-desktop-gtk4  the app against GTK4 and WebKitGTK 6.0, which is what Wails
#                          prefers and what Ubuntu 24.04 and newer have.
#   pkgcache-desktop-gtk3  the same app against GTK3 and webkit2gtk-4.1, which is what
#                          Ubuntu 22.04 has.
#
# The first split is the graphics stack: `apt install pkgcache-desktop` pulls the app and
# the daemon — one command on a laptop — while `apt install pkgcache` stays the small thing
# a server wants, with no GTK anywhere near it.
#
# The second split is the toolkit, and it exists because one binary cannot serve both
# releases. The app is built against GTK 4.14; 22.04 has 4.6, and the symbols the newer one
# provides are simply not there. Publishing only the GTK4 build meant 22.04 installed it and
# then could not start it.
#
# The metapackage is what keeps that invisible. Its dependency is an alternative —
#
#   Depends: pkgcache-desktop-gtk4 (= V) | pkgcache-desktop-gtk3 (= V)
#
# — and apt takes the first alternative it can satisfy: GTK4 on 24.04, and on 22.04, where
# that one is unsatisfiable, the GTK3 build instead. Nobody has to know which toolkit their
# release has, the install instructions say `pkgcache-desktop` everywhere as they always
# did, and a 22.04 machine that had the old broken package upgrades straight into a working
# one.
#
# The two builds own the same paths, so each Conflicts with the other and only one is ever
# installed. Both Replace the pre-split pkgcache-desktop, which owned those paths itself.
#
# All of this only works from a repository, which is what `pkgreg publish-apt` serves.
#
# usage: build.sh <daemon-binary> <arch> <version> [outdir]
#
# environment:
#   PKGCACHE_APP    the GTK4 app binary. Without it only the daemon package is built,
#                   which is what a host with no GUI toolchain can honestly produce.
#   PKGCACHE_APP_GTK3
#                   the GTK3 app binary, from a `-tags gtk3` build. Optional: without it
#                   the gtk4 package and the metapackage are still built, and the
#                   metapackage's alternative simply has one branch that no repository
#                   carries — which resolves fine on 24.04 and leaves 22.04 exactly where
#                   it was. Supplying it is what makes 22.04 work.
#   PKGCACHE_ICON   the icon, an SVG. Defaults to assets/logo.svg beside this repo.
#   PKGCACHE_GUI_DEPENDS
#                   what the GTK4 build links against, with the versions it links against.
#   PKGCACHE_GUI_DEPENDS_GTK3
#                   the same for the GTK3 build. Both default to the stack their toolkit
#                   needs and neither normally wants setting.
#   PKGCACHE_LIMIT_DEFAULT
#                   the disk budget the daemon package sets for the installing user when
#                   that user has none. Defaults to "none" — no cap, with the free-space
#                   floor still applying — which is the answer that guesses least about
#                   somebody else's disk, and the same one the Windows installer uses.
set -eu

BINARY="${1:?usage: build.sh <daemon-binary> <arch> <version> [outdir]}"
ARCH="${2:?arch: amd64 or arm64}"
VERSION="${3:?version}"
OUT="${4:-.}"

HERE="$(cd "$(dirname "$0")" && pwd)"
APP="${PKGCACHE_APP:-}"
APP_GTK3="${PKGCACHE_APP_GTK3:-}"
ICON="${PKGCACHE_ICON:-$HERE/../../assets/logo.svg}"
# The version floors are the whole point of this line, and leaving them off produced a
# package that installed cleanly on Ubuntu 22.04 and then died the moment it was run.
#
# 22.04 has both of the bare names: libgtk-4-1 is 4.6.9, and libwebkitgtk-6.0-4 arrived
# there as a backport. So apt resolved the dependencies, reported success, and handed the
# user a binary the dynamic linker could not finish loading — eighteen undefined symbols,
# because the app is built on 24.04 against GTK 4.14 and calls into APIs that do not exist
# in 4.6: the whole GtkFileDialog family (4.10), gtk_css_provider_load_from_string (4.12),
# gdk_monitor_get_scale (4.14), and g_idle_add_once from GLib 2.74 against 22.04's 2.72.
#
# An unsatisfiable dependency is the honest answer: apt says what is missing and installs
# nothing, instead of a crash with no explanation attached to it. The floors are the
# versions 24.04 ships, which is what the binary is actually built and tested against.
#
# They are also what makes the metapackage's alternative decide correctly. apt takes the
# first branch it can satisfy, so these floors are the whole mechanism by which a 22.04
# machine passes over the GTK4 build and lands on the GTK3 one.
GUI_DEPENDS="${PKGCACHE_GUI_DEPENDS:-libgtk-4-1 (>= 4.14), libglib2.0-0 (>= 2.74), libwebkitgtk-6.0-4}"
# No floors here, and that is not an oversight. This build exists for the older release, so
# the useful constraint is the opposite one — it must stay installable on 22.04, whose GTK3
# is 3.24.33 and whose webkit2gtk-4.1 is present. Both are old enough that a floor naming
# what the binary uses would exclude nothing.
GUI_DEPENDS_GTK3="${PKGCACHE_GUI_DEPENDS_GTK3:-libgtk-3-0, libwebkit2gtk-4.1-0}"
LIMIT_DEFAULT="${PKGCACHE_LIMIT_DEFAULT:-none}"
LICENSE="$HERE/../../LICENSE"
NOTICE="$HERE/../../NOTICE"

[ -f "$BINARY" ] || { echo "no such binary: $BINARY" >&2; exit 1; }
case "$ARCH" in amd64|arm64) ;; *) echo "arch must be amd64 or arm64" >&2; exit 1 ;; esac
[ -z "$APP" ] || [ -f "$APP" ] || { echo "no such app binary: $APP" >&2; exit 1; }
[ -f "$LICENSE" ] || { echo "no LICENSE at $LICENSE" >&2; exit 1; }
[ -f "$NOTICE" ] || { echo "no NOTICE at $NOTICE" >&2; exit 1; }

# A Debian version may not carry a leading 'v' or any '-' beyond the revision, and our
# build stamps look like "23888d5-dirty". Normalised rather than rejected: the package
# should be buildable from a working tree, and the git description is the useful part.
DEBVER="$(printf '%s' "$VERSION" | sed 's/^v//; s/-/+/g')"
[ -n "$DEBVER" ] || DEBVER="0"

# And it has to begin with a digit, which a git description often does not: `git describe
# --always` on a tree with no tags is a bare commit hash, and five hex characters in eight
# are letters. This was a coin flip — 410a9e2 installed and d4fd0d0 did not, with dpkg
# refusing at unpack time on a package that had built without complaint.
#
# Prefixed rather than rejected, and with '~' because it sorts before everything: an
# untagged development build then orders below every real release rather than above them.
case "$DEBVER" in
[0-9]*) ;;
*) DEBVER="0~$DEBVER" ;;
esac

# Checked rather than assumed, because the failure lands on somebody else's machine at
# install time. dpkg's rule for an upstream version is a leading digit followed by
# alphanumerics and . + ~ : - only.
printf '%s' "$DEBVER" | grep -qE '^[0-9][A-Za-z0-9.+~:-]*$' || {
	echo "build.sh: '$DEBVER' is not a valid Debian version" >&2
	echo "  Derived from --version '$VERSION'. Pass a version dpkg accepts." >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT INT TERM

mkdir -p "$OUT"
# Resolved before anything changes directory: the ar call runs inside a work directory,
# and a relative output path would be relative to that instead of to the caller.
OUT="$(cd "$OUT" && pwd)"

# --sort=name and a fixed mtime make the package reproducible: two builds of the same
# binary produce byte-identical .deb files, so a checksum means something.
TAROPTS="--owner=0 --group=0 --numeric-owner --sort=name --mtime=@0"

# assemble <name> <root> <controldir> — turns a staged tree into a .deb.
#
# The member order is part of the format, not a convention. -D is deterministic mode:
# without it ar stamps each member with the clock and the builder's uid, and two builds of
# one binary would differ for no reason anybody can act on.
assemble() {
	_name="$1"; _root="$2"; _ctrl="$3"
	_stage="$WORK/stage-$_name"
	mkdir -p "$_stage"
	printf '2.0\n' > "$_stage/debian-binary"

	# md5sums lets `dpkg -V` verify the package after installation.
	( cd "$_root" && find . -type f -printf '%P\0' 2>/dev/null | sort -z |
		xargs -0 -r md5sum > "$_ctrl/md5sums" ) 2>/dev/null || \
	( cd "$_root" && find . -type f | sed 's|^\./||' | sort | while read -r f; do
		md5sum "$f"; done > "$_ctrl/md5sums" )

	# shellcheck disable=SC2086
	tar $TAROPTS -czf "$_stage/data.tar.gz" -C "$_root" .
	_members="./control ./md5sums"
	[ -f "$_ctrl/postinst" ] && _members="$_members ./postinst"
	[ -f "$_ctrl/prerm" ] && _members="$_members ./prerm"
	[ -f "$_ctrl/conffiles" ] && _members="$_members ./conffiles"
	# shellcheck disable=SC2086
	tar $TAROPTS -czf "$_stage/control.tar.gz" -C "$_ctrl" $_members

	_deb="$OUT/${_name}_${DEBVER}_${ARCH}.deb"
	rm -f "$_deb"
	( cd "$_stage" && ar rcD "$_deb" debian-binary control.tar.gz data.tar.gz )
	echo "$_deb"
}

# ---- pkgcache: the daemon and the CLI --------------------------------------------
ROOT="$WORK/root-daemon"
CTRL="$WORK/ctrl-daemon"
mkdir -p "$ROOT/usr/bin" "$ROOT/usr/share/doc/pkgcache" "$CTRL"
install -m 0755 "$BINARY" "$ROOT/usr/bin/pkgcache"

# The docker-compatible shim, where the build produced one. Optional the same way the app
# is: a host that did not build it still gets a complete daemon package.
if [ -n "${PKGCACHE_SHIM:-}" ] && [ -f "$PKGCACHE_SHIM" ]; then
	install -m 0755 "$PKGCACHE_SHIM" "$ROOT/usr/bin/pkgcache-docker"
fi

# A real DEP-5 copyright rather than the two-line stub this used to be.
#
# Policy 12.5 requires the file to state the licence, and the stub stated nothing — which
# meant the only thing a customer received on Ubuntu named no licence anywhere. The full
# Apache text is not repeated here because Debian ships it: /usr/share/common-licenses is
# guaranteed to exist and referring to it is what policy asks for.
write_copyright() {
	cat > "$1" <<'COPY'
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: pkgcache

Files: *
Copyright: pkgreg
License: Apache-2.0

License: Apache-2.0
 Licensed under the Apache License, Version 2.0 (the "License"); you may not
 use this file except in compliance with the License. You may obtain a copy
 of the License at
 .
     http://www.apache.org/licenses/LICENSE-2.0
 .
 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
 License for the specific language governing permissions and limitations
 under the License.
 .
 On Debian systems the complete text of the Apache License version 2.0 can
 be found in /usr/share/common-licenses/Apache-2.0.
 .
 The third-party notices for the binaries in this package are in the NOTICE
 file beside this one. Minimal images configure dpkg to exclude
 /usr/share/doc, which keeps this file and drops that one; the same notices
 ship with every release and are in the source repository as NOTICE.
COPY
}
write_copyright "$ROOT/usr/share/doc/pkgcache/copyright"
install -m 0644 "$NOTICE" "$ROOT/usr/share/doc/pkgcache/NOTICE"

# Debian expects the changelog to be compressed, and lintian complains either way about
# a stub. It is here because its absence is a policy violation, not because it is read.
# The date comes from SOURCE_DATE_EPOCH, or is fixed, so two builds of one binary
# produce identical packages. `date -R` here would put the clock in the archive and make
# every rebuild differ.
STAMP="$(date -R -u -d "@${SOURCE_DATE_EPOCH:-0}" 2>/dev/null || date -u -r "${SOURCE_DATE_EPOCH:-0}" +"%a, %d %b %Y %H:%M:%S +0000")"
CHANGELOG="pkgcache ($DEBVER) stable; urgency=low\n\n  * Built from source.\n\n -- pkgreg <root@localhost>  $STAMP\n"
printf "$CHANGELOG" | gzip -9n > "$ROOT/usr/share/doc/pkgcache/changelog.Debian.gz"

cat > "$CTRL/control" <<CONTROL
Package: pkgcache
Version: $DEBVER
Section: devel
Priority: optional
Architecture: $ARCH
Maintainer: pkgreg <root@localhost>
Installed-Size: $(du -sk "$ROOT" | cut -f1)
Suggests: pkgcache-desktop
Description: Package cache for one machine
 pkgcache keeps this machine's package downloads — pip, npm, Docker images, apt and
 git — on local disk, and can sit in front of a team cache so a download crosses the
 network once for everyone rather than once for each person.
 .
 One static binary with no runtime dependencies, which is what makes it safe to install
 on a build box or in a container image. The desktop app that watches it is a separate
 package, pkgcache-desktop, so that a machine with no screen carries no graphics stack.
CONTROL

# A disk budget for the person installing, where they have none.
#
# pkgcache refuses to start without one and will not guess a size for somebody else's
# disk. For the CLI that is right — the error names the commands that answer it. For the
# desktop half it is not: the autostart entry launches the app at the next login and the
# first thing a new machine shows is that error in a window, telling somebody who ran
# `apt install` to go and find a terminal. The Windows installer already made this
# decision; this is the same one.
#
# The budget lives under the user's home, so root's copy is worth nothing to them — the
# same trap the macOS postinstall documents. SUDO_USER and PKEXEC_UID are the two ways a
# person's identity survives into a postinst, and where neither is set this does nothing
# at all: a root shell, a container or a CI runner is exactly where a guessed budget is
# unwanted, and PKGCACHE_LIMIT covers that case anyway.
cat > "$CTRL/postinst" <<POSTINST
#!/bin/sh
set -e
[ "\$1" = configure ] || exit 0

user="\${SUDO_USER:-}"
if [ -z "\$user" ] && [ -n "\${PKEXEC_UID:-}" ]; then
	user="\$(getent passwd "\$PKEXEC_UID" 2>/dev/null | cut -d: -f1)"
fi
[ -n "\$user" ] && [ "\$user" != root ] || exit 0

home="\$(getent passwd "\$user" 2>/dev/null | cut -d: -f6)"
[ -n "\$home" ] || exit 0

# -H in spirit: without HOME set to theirs, pkgcache would write root's budget and the
# person who installed it would see nothing and be told everything worked.
as_user() {
	if command -v runuser >/dev/null 2>&1; then
		runuser -u "\$user" -- env HOME="\$home" "\$@"
	else
		su -s /bin/sh "\$user" -c "HOME='\$home' \$*"
	fi
}

# pkgcache limit with no argument exits non-zero exactly when none is set, so it is the
# query as well as the setter. Asking first is what stops an upgrade from overwriting a
# size somebody chose deliberately.
if as_user /usr/bin/pkgcache limit >/dev/null 2>&1; then
	:
elif as_user /usr/bin/pkgcache limit $LIMIT_DEFAULT >/dev/null 2>&1; then
	echo "pkgcache: cache limit set to $LIMIT_DEFAULT for \$user; change it with 'pkgcache limit'"
else
	echo "pkgcache: no disk budget is set, and pkgcache will not start without one." >&2
	echo "  pkgcache limit 25G     cap the cache at 25 GiB" >&2
	echo "  pkgcache limit none    no cap; a free-space floor still applies" >&2
fi
exit 0
POSTINST
chmod 0755 "$CTRL/postinst"

# Nothing else is printed here on purpose. The old version of this package ended by
# telling somebody to run two more commands, which is the whole feeling this split exists
# to remove: installing is the install.
assemble pkgcache "$ROOT" "$CTRL"

# ---- pkgcache-desktop-gtk4 / -gtk3: the app ---------------------------------------
if [ -z "$APP" ]; then
	echo "note: PKGCACHE_APP is unset, so only the daemon package was built." >&2
	exit 0
fi
[ -f "$ICON" ] || { echo "no such icon: $ICON" >&2; exit 1; }

# One function, called once per toolkit. The two packages differ in exactly three things —
# their name, their binary and what they link against — and everything else about them is
# the same icon, the same launcher entry, the same login item and the same maintainer
# scripts. Writing that twice is how the two would quietly drift apart.
#
#   $1  toolkit: gtk4 or gtk3
#   $2  the app binary
#   $3  the Depends clause for its graphics stack
desktop_package() {
	toolkit="$1"
	app_binary="$2"
	gui_depends="$3"
	pkg="pkgcache-desktop-$toolkit"
	case "$toolkit" in
	gtk4) other="pkgcache-desktop-gtk3"; stack="GTK4 and WebKitGTK 6.0" ;;
	gtk3) other="pkgcache-desktop-gtk4"; stack="GTK3 and webkit2gtk-4.1" ;;
	esac

	ROOT="$WORK/root-$toolkit"
	CTRL="$WORK/ctrl-$toolkit"
	mkdir -p "$ROOT/usr/bin" "$ROOT/usr/share/applications" "$ROOT/etc/xdg/autostart" \
		"$ROOT/usr/share/icons/hicolor/scalable/apps" "$ROOT/usr/share/doc/$pkg" "$CTRL"

	install -m 0755 "$app_binary" "$ROOT/usr/bin/pkgcache-app"
# Scalable, so one file covers every panel size and no rasteriser is needed at build
# time. Its absence is why the launcher entry has shown a blank square until now: the
# old package shipped a .desktop naming an icon it never installed.
install -m 0644 "$ICON" "$ROOT/usr/share/icons/hicolor/scalable/apps/pkgcache.svg"

# The launcher entry, and the name the desktop matches a window to it by.
#
# StartupWMClass says org.wails.pkgcache and the file is called pkgcache.desktop, which
# looks inconsistent and is the only combination that works on both display servers.
# Measured, by running the shipped binary under a headless X server and a headless
# compositor and reading the identifiers off the wire:
#
#   X11      WM_CLASS = "pkgcache", "pkgcache"      from g_set_prgname, which the app sets
#   Wayland  xdg_toplevel.set_app_id("org.wails.pkgcache")
#
# They differ because GTK4 takes the Wayland app_id from the GApplication's id and only
# falls back to the program name when there is none — and Wails builds that id as
# `fmt.Sprintf("org.wails.%s", name)` from Options.Name, with no way to override it. So
# the program name reaches X11 and never reaches Wayland.
#
# GNOME matches a window by trying, in order, the GTK application id, then StartupWMClass
# against the window's class, then the desktop file's own basename. This entry answers on
# the second for Wayland and on the third for X11:
#
#   Wayland  app_id org.wails.pkgcache == StartupWMClass
#   X11      WM_CLASS pkgcache         == pkgcache.desktop
#
# One file, not two. An `org.wails.pkgcache.desktop` alias would match earlier and was the
# first thing tried — but a second entry the shell can match is a second dock icon beside
# the pinned one, which is a worse bug than the one being fixed.
#
# Ubuntu has defaulted to Wayland since 22.04, so before this the icon was a gear on the
# distribution most people install on, and correct everywhere it was tested by hand.
cat > "$ROOT/usr/share/applications/pkgcache.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=pkgcache
GenericName=Package cache
Comment=Watch what this machine is caching
Exec=pkgcache-app
Icon=pkgcache
Terminal=false
Categories=Development;Utility;
Keywords=cache;packages;docker;pip;npm;apt;
StartupNotify=true
StartupWMClass=org.wails.pkgcache
DESKTOP

# Started for every user who logs in, because an app that has to be launched from a
# terminal before it can watch anything is not installed, it is merely present.
#
# A person who does not want it does not edit this file: the desktop way to opt out is a
# per-user override in ~/.config/autostart, which is what `pkgcache-app --off-login`
# writes and what any desktop's own Startup Applications panel writes too.
cat > "$ROOT/etc/xdg/autostart/pkgcache.desktop" <<'AUTOSTART'
[Desktop Entry]
Type=Application
Name=pkgcache
Comment=Keep pkgcache in the status bar
Exec=pkgcache-app --background
Icon=pkgcache
Terminal=false
X-GNOME-Autostart-enabled=true
AUTOSTART

# A file under /etc a local administrator may reasonably want to change, which is exactly
# what conffiles is for: dpkg then asks before replacing an edited copy on upgrade.
echo "/etc/xdg/autostart/pkgcache.desktop" > "$CTRL/conffiles"

write_copyright "$ROOT/usr/share/doc/$pkg/copyright"
install -m 0644 "$NOTICE" "$ROOT/usr/share/doc/$pkg/NOTICE"
printf "$CHANGELOG" | gzip -9n > "$ROOT/usr/share/doc/$pkg/changelog.Debian.gz"

# Conflicts and Replaces, both naming the other build, because the two own identical
# paths — /usr/bin/pkgcache-app and one launcher entry. Conflicts is what stops apt from
# ever choosing both; Replaces is what lets one take those paths over from the other on a
# machine switching between them, which is what a release upgrade is.
#
# The pair also Replaces and Breaks the pre-split pkgcache-desktop, which owned those same
# paths before it became a metapackage. Without that, dpkg refuses the upgrade with a file
# conflict against a package that is still installed.
cat > "$CTRL/control" <<CONTROL
Package: $pkg
Source: pkgcache
Version: $DEBVER
Section: devel
Priority: optional
Architecture: $ARCH
Maintainer: pkgreg <root@localhost>
Installed-Size: $(du -sk "$ROOT" | cut -f1)
Depends: pkgcache (= $DEBVER), $gui_depends
Conflicts: $other, pkgcache-desktop (<< $DEBVER)
Replaces: $other, pkgcache-desktop (<< $DEBVER)
Breaks: pkgcache-desktop (<< $DEBVER)
Provides: pkgcache-desktop-app
Description: Desktop app for pkgcache ($stack)
 A window and a status bar item for the cache pkgcache keeps on this machine: what is
 downloading, how much of the disk budget is used, how much is being served from here,
 and a notification when the cache fills up and stops storing.
 .
 This is the build against $stack. It is one of two, and the toolkit is the
 only difference between them: install pkgcache-desktop instead and apt picks whichever
 one this release of the distribution can run.
 .
 It depends on the exact same version of pkgcache, because the app talks to the daemon
 over a local API and two halves of one product drifting apart on a machine produces a
 bug report nobody can read.
CONTROL

# Icon and desktop caches are indexed, not scanned, so a newly installed launcher entry
# does not appear until the index is told. Both tools are absent on a minimal system and
# their absence is not a failure — the entry is still correct, it just shows up on the
# next login instead of immediately.
cat > "$CTRL/postinst" <<'POSTINST'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
	if command -v gtk-update-icon-cache >/dev/null 2>&1; then
		gtk-update-icon-cache -q /usr/share/icons/hicolor 2>/dev/null || true
	fi
	if command -v update-desktop-database >/dev/null 2>&1; then
		update-desktop-database -q /usr/share/applications 2>/dev/null || true
	fi
fi
POSTINST
chmod 0755 "$CTRL/postinst"

# Removing the package should stop the app, not leave a window belonging to software that
# is no longer installed. The daemon is deliberately left alone: it is the other package's,
# it exits on its own when idle, and stopping somebody's cache because they removed a
# window would be a surprise.
cat > "$CTRL/prerm" <<'PRERM'
#!/bin/sh
set -e
case "$1" in
	remove|deconfigure|upgrade)
		pkill -x pkgcache-app 2>/dev/null || true
		;;
esac
exit 0
PRERM
chmod 0755 "$CTRL/prerm"

assemble "$pkg" "$ROOT" "$CTRL"
}

desktop_package gtk4 "$APP" "$GUI_DEPENDS"
if [ -n "$APP_GTK3" ]; then
	[ -f "$APP_GTK3" ] || { echo "no such gtk3 app binary: $APP_GTK3" >&2; exit 1; }
	desktop_package gtk3 "$APP_GTK3" "$GUI_DEPENDS_GTK3"
else
	echo "note: PKGCACHE_APP_GTK3 is unset, so Ubuntu 22.04 gets no app from this build." >&2
fi

# ---- pkgcache-desktop: the metapackage --------------------------------------------
#
# Carries no program. Its whole content is one dependency line, and that line is the
# mechanism: apt walks the alternatives left to right and installs the first it can
# satisfy. On 24.04 that is the GTK4 build. On 22.04 the GTK4 build's floors cannot be
# met, so apt passes over it and takes the GTK3 one — no user choice, no release
# detection, and the same `apt install pkgcache-desktop` everywhere.
#
# The versions are pinned to this build for the same reason the app pins the daemon: a
# metapackage that would accept an older app is a machine where `apt upgrade` can leave
# two halves of one product at different versions.
#
# Architecture is the build's own rather than `all`, because the packages it depends on
# are architecture-specific and this repository publishes one set of files per arch.
ROOT="$WORK/root-meta"
CTRL="$WORK/ctrl-meta"
mkdir -p "$ROOT/usr/share/doc/pkgcache-desktop" "$CTRL"

write_copyright "$ROOT/usr/share/doc/pkgcache-desktop/copyright"
install -m 0644 "$NOTICE" "$ROOT/usr/share/doc/pkgcache-desktop/NOTICE"
printf "$CHANGELOG" | gzip -9n > "$ROOT/usr/share/doc/pkgcache-desktop/changelog.Debian.gz"

cat > "$CTRL/control" <<CONTROL
Package: pkgcache-desktop
Source: pkgcache
Version: $DEBVER
Section: devel
Priority: optional
Architecture: $ARCH
Maintainer: pkgreg <root@localhost>
Installed-Size: $(du -sk "$ROOT" | cut -f1)
Depends: pkgcache-desktop-gtk4 (= $DEBVER) | pkgcache-desktop-gtk3 (= $DEBVER)
Description: Desktop app for pkgcache
 A window and a status bar item for the cache pkgcache keeps on this machine: what is
 downloading, how much of the disk budget is used, how much is being served from here,
 and a notification when the cache fills up and stops storing.
 .
 This package installs whichever build of the app this release can run: the GTK4 one on
 Ubuntu 24.04 and newer, and the GTK3 one on 22.04, whose GTK is too old for the other.
 Installing this rather than either by name is what makes that somebody else's problem.
CONTROL

assemble pkgcache-desktop "$ROOT" "$CTRL"
