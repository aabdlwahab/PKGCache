package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Opening the console, and opening it as something that looks like an app.
//
// The console is already a complete UI, embedded in the binary and served on the loopback
// port. What it lacks on a laptop is a window: nobody keeps a browser tab open to watch a
// cache. A Chromium-family browser in app mode gives a window with no tabs and no address
// bar for the price of one flag, which is the whole of the "widget" for every platform at
// once — no cgo, no toolkit, no second frontend.
//
// It is a flag and not a standard, so it can go away. That is survivable because the
// fallback is an ordinary window, and because -print exists for anyone who would rather
// place it themselves.

// Launcher is a resolved way to open a URL.
type Launcher struct {
	// Program is the executable to run.
	Program string
	// Args are its arguments, URL included.
	Args []string
	// AppWindow reports whether this launcher gives a window without browser chrome.
	// False means the URL opens as an ordinary tab, which callers say out loud rather
	// than silently delivering something other than what was asked for.
	AppWindow bool
	// Native reports that this is an application window with the platform's own engine in
	// it, rather than a browser asked to hide its chrome. Worth distinguishing because the
	// two fail differently: a browser can be missing, and a native window cannot be
	// mistaken for a tab.
	Native bool
}

// ErrNoBrowser reports that this machine has nothing to open a URL with.
//
// Common and not an error in the usual sense: there is no browser over SSH, in CI, or in
// a container. Callers print the URL instead.
var ErrNoBrowser = errors.New("no browser found on this machine")

// appWindowFlag is what the Chromium family calls it. Firefox has no equivalent — -kiosk
// is fullscreen, which is not the same request — so a Firefox machine gets a tab.
const appWindowFlag = "--app="

// chromiumNames are the binaries that understand appWindowFlag, most-likely first.
var chromiumNames = map[string][]string{
	"linux": {
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"brave-browser", "microsoft-edge", "microsoft-edge-stable", "vivaldi",
	},
	// On macOS the binaries live inside bundles that are not on PATH, so they are opened
	// by bundle name through `open -a`, which is checked differently below.
	"darwin": {"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium"},
	"windows": {
		"chrome.exe", "msedge.exe", "brave.exe",
	},
}

// LookPath is exec.LookPath, replaced in tests.
type LookPath func(string) (string, error)

// ResolveLauncher decides how to open a URL, preferring a window without browser chrome.
//
// Every input that varies by machine is a parameter — the platform, the PATH lookup, the
// environment — because the whole point of this function is to be tested on a machine
// that is none of the three.
func ResolveLauncher(
	url string, appWindow bool, goos string, look LookPath, getenv func(string) string,
) (Launcher, error) {
	if look == nil {
		look = exec.LookPath
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if goos == "linux" && !hasDisplay(getenv) {
		// No display: notify-send is skipped for the same reason. A browser here would
		// either fail or, worse, block.
		return Launcher{}, ErrNoBrowser
	}
	if appWindow {
		// A real application window first, where this machine has one: it needs no browser
		// at all, and it is what "an app rather than a tab" means. See window.go.
		if launcher, found := NativeWindow(url, goos); found {
			return launcher, nil
		}
		if launcher, found := appLauncher(url, goos, look); found {
			return launcher, nil
		}
	}
	return plainLauncher(url, goos, look, getenv)
}

func appLauncher(url, goos string, look LookPath) (Launcher, bool) {
	switch goos {
	case "darwin":
		// The bundle is checked before it is chosen, and the earlier note here claiming
		// there was "no cheap way to ask first" was simply wrong — the app is a directory.
		//
		// It mattered: `open -na "Google Chrome"` fails *after* the process has started, so
		// exec.Cmd.Start had already returned success and nothing could fall back. On a Mac
		// with no Chromium-family browser, `pkgcache widget` printed an address and opened
		// nothing at all.
		if _, err := look("open"); err != nil {
			return Launcher{}, false
		}
		for _, bundle := range chromiumNames["darwin"] {
			if !appBundleExists(bundle) {
				continue
			}
			return Launcher{
				Program:   "open",
				Args:      []string{"-na", bundle, "--args", appWindowFlag + url},
				AppWindow: true,
			}, true
		}
		return Launcher{}, false
	default:
		for _, name := range chromiumNames[goos] {
			if _, err := look(name); err != nil {
				continue
			}
			return Launcher{
				Program:   name,
				Args:      []string{appWindowFlag + url, "--window-size=420,660"},
				AppWindow: true,
			}, true
		}
	}
	return Launcher{}, false
}

func plainLauncher(
	url, goos string, look LookPath, getenv func(string) string,
) (Launcher, error) {
	// $BROWSER first on Unix: somebody who set it means it, and a cache is not the
	// program to overrule them.
	if goos != "windows" {
		if chosen := strings.TrimSpace(getenv("BROWSER")); chosen != "" {
			// A BROWSER holding flags or a %s placeholder is a shell convention this
			// does not implement; the plain program name is the common case.
			if _, err := look(chosen); err == nil {
				return Launcher{Program: chosen, Args: []string{url}}, nil
			}
		}
	}
	switch goos {
	case "darwin":
		if _, err := look("open"); err == nil {
			return Launcher{Program: "open", Args: []string{url}}, nil
		}
	case "windows":
		// No LookPath check: cmd.exe is the shell that resolves `start`, and `start` is
		// a builtin rather than a program on PATH. The empty "" is start's title
		// argument, without which a quoted URL becomes the window title.
		return Launcher{Program: "cmd", Args: []string{"/c", "start", "", url}}, nil
	default:
		for _, name := range []string{"xdg-open", "gio", "sensible-browser", "firefox"} {
			if _, err := look(name); err != nil {
				continue
			}
			if name == "gio" {
				return Launcher{Program: name, Args: []string{"open", url}}, nil
			}
			return Launcher{Program: name, Args: []string{url}}, nil
		}
	}
	return Launcher{}, ErrNoBrowser
}

// appBundleExists reports whether a macOS application is installed.
//
// A variable so a test can answer for a platform it is not running on: the three search
// paths are where macOS keeps applications, and on any other OS this is never called.
var appBundleExists = func(bundle string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	for _, root := range []string{"/Applications", "/System/Applications", home + "/Applications"} {
		if info, statErr := os.Stat(root + "/" + bundle + ".app"); statErr == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// hasDisplay reports whether this Linux session has somewhere to put a window.
func hasDisplay(getenv func(string) string) bool {
	return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}

// OpenBrowser starts the launcher and does not wait for it.
//
// Not waited on deliberately: a browser is a long-lived process, and `pkgcache widget`
// returning only when somebody closes their browser would be surprising. Its output goes
// nowhere for the same reason — a GTK warning on stderr is not this command's news.
func OpenBrowser(ctx context.Context, launcher Launcher) error {
	if launcher.Program == "" {
		return ErrNoBrowser
	}
	// #nosec G204 -- the program is one of a fixed list or the user's own $BROWSER, and
	// the URL is this daemon's loopback address.
	cmd := exec.CommandContext(ctx, launcher.Program, launcher.Args...)
	// stderr is passed through, not discarded. A browser's own warnings are noise, and
	// they are worth the noise: swallowing them is what made "opened nothing, said nothing"
	// impossible to diagnose from the outside.
	cmd.Stdout, cmd.Stdin = nil, nil
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", launcher.Program, err)
	}
	// Reaped in the background so a browser that exits immediately does not linger as a
	// zombie for as long as this process lives.
	go func() { _ = cmd.Wait() }()
	return nil
}
