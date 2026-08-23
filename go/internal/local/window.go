package local

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// A real application window, where this machine has one.
//
// The console is HTML on a loopback port, and a browser tab is a poor container for it: no
// icon of its own in the Dock, the taskbar or alt-tab, an address bar nobody needs, and on
// a machine with no Chromium-family browser installed, no chromeless window at all — which
// is precisely how this came up.
//
// So the window is preferred over a browser when the platform's own engine is available:
//
//	linux    pkgcache-window, built against WebKitGTK on Ubuntu
//	windows  pkgcache-window, WebView2 through a pure-Go binding
//	darwin   pkgcache-menubar -window, which already owns a native process for the icon
//
// All three are helpers rather than code in this binary, and the reason is the release: with
// them, pkgcache is still one static CGO_ENABLED=0 executable for five targets built from
// one host. Any of them may be absent, which is not a failure — the browser paths below it
// are the fallback, and they were the whole feature until now.

// windowHelpers names the helper each platform's window lives in.
var windowHelpers = map[string]string{
	"linux":   "pkgcache-window",
	"windows": "pkgcache-window.exe",
	"darwin":  "pkgcache-menubar",
}

// helperPathFor finds a helper beside this binary or on PATH.
//
// Beside first, because that is what a release ships and what somebody who copied two files
// onto a machine will have. A variable so a test can answer without a filesystem.
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

// NativeWindow returns a launcher for an application window, if this machine has one.
func NativeWindow(url, goos string) (Launcher, bool) {
	name, known := windowHelpers[goos]
	if !known {
		return Launcher{}, false
	}
	path, found := helperPathFor(name)
	if !found {
		return Launcher{}, false
	}
	args := []string{url}
	if goos == "darwin" {
		// The macOS helper is both things, so it has to be told which one is wanted.
		args = []string{"-window", url}
	}
	return Launcher{Program: path, Args: args, AppWindow: true, Native: true}, true
}

// GOOS is runtime.GOOS, named so callers read as platform-parameterised rather than
// platform-specific.
func GOOS() string { return runtime.GOOS }
