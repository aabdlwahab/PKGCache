package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Finding the desktop app.
//
// The console is HTML on a loopback port, and a browser tab is a poor container for it: no
// icon of its own in the Dock, the taskbar or alt-tab, and an address bar nobody needs. So
// there is an application window, and since the cutover there is exactly one of it —
// pkgcache-app, one program on all three platforms.
//
// Before that there were three: a cgo WebKitGTK binary on Linux, a WebView2 binary on
// Windows, and a Swift helper on macOS, each found by a different name. That map is gone
// and so is the reason for it.
//
// pkgcache does not need the app to work. `pkgcache widget` prefers it and falls back to a
// browser, which is what it did before any of them existed.

// appName is the desktop app's binary, per platform.
func appName() string {
	if runtime.GOOS == "windows" {
		return "pkgcache-app.exe"
	}
	return "pkgcache-app"
}

// helperPathFor finds a program beside this binary or on PATH.
//
// Beside first, because that is what every installer here produces: the .deb puts both in
// /usr/bin, the macOS bundle puts both in Contents/MacOS with symlinks into it, and
// somebody who copied two files onto a machine has them in one directory. A variable so a
// test can answer without a filesystem.
var helperPathFor = func(name string) (string, bool) {
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if info, statErr := os.Stat(beside); statErr == nil && !info.IsDir() {
			return beside, true
		}
	}
	found, err := exec.LookPath(name)
	return found, err == nil
}

// DaemonPath finds the pkgcache binary that holds the cache.
//
// Every program here that is not pkgcache itself needs this, and each one that forgot it
// found out the same way: local.Ensure re-executes os.Executable when it is not told
// otherwise, which is right for pkgcache starting pkgcache and silently wrong for anything
// else. The app launched itself as a cache and waited thirty seconds; the docker shim ran
// `docker -data-dir …` and got "unknown shorthand flag: 'd'".
//
// So it lives here, once, next to the rule it exists to satisfy. A caller that uses it
// must also set EnsureOptions.DifferentBinary, or the version check reads a daemon of
// another build as a stale one and stops it on every call.
func DaemonPath() (string, bool) {
	name := "pkgcache"
	if runtime.GOOS == "windows" {
		name = "pkgcache.exe"
	}
	return helperPathFor(name)
}

// AppPath returns the desktop app's path, if this machine has it installed.
func AppPath() (string, bool) { return helperPathFor(appName()) }

// GOOS is runtime.GOOS, named so callers read as platform-parameterised rather than
// platform-specific.
func GOOS() string { return runtime.GOOS }

// StartApp launches the desktop app and returns without waiting for it.
//
// Detached, because the app outlives the command that asked for it: `pkgcache tray` in a
// shell should not hold a terminal open for as long as somebody keeps an icon in their
// status bar, and closing that terminal should not take the icon with it.
//
// background asks the app to come up as an icon with no window, which is what a login
// entry and `pkgcache tray` both want. `pkgcache widget` passes false and gets a window.
func StartApp(ctx context.Context, app string, background bool) error {
	var args []string
	if background {
		args = append(args, "-background")
	}
	// #nosec G204 -- app is resolved beside this binary or on PATH, never from input.
	cmd := exec.CommandContext(ctx, app, args...)
	// Its own session, so it survives the shell that started it. Output goes nowhere:
	// anything worth saying, the icon says.
	detach(cmd)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("local: start %s: %w", app, err)
	}
	// Released rather than waited for. A tracked child would make this command the app's
	// parent for its whole life.
	return cmd.Process.Release()
}
