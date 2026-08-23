package local

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The launcher is the one piece of this program whose correctness depends on which
// machine it runs on, so every input that varies is a parameter and all three platforms
// are tested here — on whichever one is running the tests.
func TestResolveLauncherPrefersAnAppWindow(t *testing.T) {
	found := installed("chromium", "xdg-open")
	display := environment(map[string]string{"DISPLAY": ":0"})

	launcher, err := ResolveLauncher("http://127.0.0.1:41780/widget", true, "linux", found, display)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.Program != "chromium" || !launcher.AppWindow {
		t.Fatalf("resolved %+v", launcher)
	}
	if !strings.Contains(strings.Join(launcher.Args, " "), "--app=http://127.0.0.1:41780/widget") {
		t.Fatalf("the app flag did not carry the URL: %v", launcher.Args)
	}
}

// Firefox has no app-window flag, and -kiosk is fullscreen rather than the same request.
// A machine with only Firefox gets an ordinary window, and the caller is told so rather
// than being handed something that looks like a failure to honour the request.
func TestResolveLauncherFallsBackToAnOrdinaryWindow(t *testing.T) {
	launcher, err := ResolveLauncher("http://cache/widget", true, "linux",
		installed("firefox", "xdg-open"), environment(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}))
	if err != nil {
		t.Fatal(err)
	}
	if launcher.AppWindow {
		t.Fatalf("claimed an app window from %+v", launcher)
	}
	if launcher.Program != "xdg-open" {
		t.Fatalf("resolved %+v, want the desktop's own opener", launcher)
	}
}

// $BROWSER is somebody's stated preference, and a cache is not the program to overrule
// it. Only for an ordinary window: it says which browser, not which window shape.
func TestResolveLauncherHonoursBROWSER(t *testing.T) {
	launcher, err := ResolveLauncher("http://cache/console", false, "linux",
		installed("surf", "xdg-open"),
		environment(map[string]string{"DISPLAY": ":0", "BROWSER": "surf"}))
	if err != nil {
		t.Fatal(err)
	}
	if launcher.Program != "surf" {
		t.Fatalf("resolved %+v, want the browser the environment names", launcher)
	}
}

// No display is the common case, not an error: there is none over SSH, in CI, or in a
// container. The caller prints the URL instead, which is why this is a sentinel.
func TestResolveLauncherReportsNoDisplay(t *testing.T) {
	_, err := ResolveLauncher("http://cache/widget", true, "linux",
		installed("chromium"), environment(nil))
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("a headless Linux session gave %v", err)
	}
}

func TestResolveLauncherReportsNoBrowser(t *testing.T) {
	_, err := ResolveLauncher("http://cache/widget", true, "linux",
		installed(), environment(map[string]string{"DISPLAY": ":0"}))
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("a machine with no browser gave %v", err)
	}
}

// The other two platforms, which cannot be run here and are therefore worth asserting
// the shape of: the flags are the part that goes wrong silently.
func TestResolveLauncherOnDarwinAndWindows(t *testing.T) {
	// macOS app mode now requires the bundle to be installed, so the darwin cases below
	// have to say that it is. Before this check they passed on any machine and described
	// behaviour that opened nothing on a Mac without Chrome.
	restore := appBundleExists
	appBundleExists = func(bundle string) bool { return bundle == "Google Chrome" }
	t.Cleanup(func() { appBundleExists = restore })

	cases := []struct {
		name, goos string
		app        bool
		found      LookPath
		program    string
		contains   string
		appWindow  bool
	}{
		{
			name: "darwin app window", goos: "darwin", app: true, found: installed("open"),
			program: "open", contains: "--app=http://cache/widget", appWindow: true,
		},
		{
			name: "darwin ordinary", goos: "darwin", app: false, found: installed("open"),
			program: "open", contains: "http://cache/widget",
		},
		{
			// cmd resolves `start`, which is a shell builtin rather than a program on
			// PATH, so this one is deliberately not gated on a lookup.
			name: "windows", goos: "windows", app: false, found: installed(),
			program: "cmd", contains: "start",
		},
		{
			name: "windows app window", goos: "windows", app: true, found: installed("chrome.exe"),
			program: "chrome.exe", contains: "--app=http://cache/widget", appWindow: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			launcher, err := ResolveLauncher(
				"http://cache/widget", c.app, c.goos, c.found, environment(nil))
			if err != nil {
				t.Fatal(err)
			}
			if launcher.Program != c.program {
				t.Fatalf("program %q, want %q", launcher.Program, c.program)
			}
			if !strings.Contains(strings.Join(launcher.Args, " "), c.contains) {
				t.Fatalf("args %v, want something containing %q", launcher.Args, c.contains)
			}
			if launcher.AppWindow != c.appWindow {
				t.Fatalf("AppWindow = %v, want %v", launcher.AppWindow, c.appWindow)
			}
		})
	}
}

// An empty launcher must never be handed to exec: it would report a confusing error from
// deep inside os/exec rather than the sentinel every caller already handles.
func TestOpenBrowserRefusesAnEmptyLauncher(t *testing.T) {
	if err := OpenBrowser(nil, Launcher{}); !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("an empty launcher gave %v", err)
	}
}

// installed builds a LookPath that finds exactly these names.
func installed(names ...string) LookPath {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// The bug this pins: `open -na "Google Chrome"` fails *after* the process has started, so
// nothing could fall back, and a Mac with no Chromium-family browser got an address and no
// window — silently, because the launcher's stderr was discarded too.
//
// The bundle is a directory, so it can simply be checked first.
func TestDarwinFallsBackWhenNoChromiumIsInstalled(t *testing.T) {
	installed := map[string]bool{}
	restore := appBundleExists
	appBundleExists = func(bundle string) bool { return installed[bundle] }
	t.Cleanup(func() { appBundleExists = restore })

	// Nothing Chromium-family: an ordinary window through `open`, and it says so by not
	// claiming to be an app window.
	launcher, err := ResolveLauncher("http://127.0.0.1:41780/widget", true, "darwin",
		installed2("open"), environment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if launcher.AppWindow {
		t.Fatalf("claimed an app window with no browser to give one: %+v", launcher)
	}
	if launcher.Program != "open" || len(launcher.Args) != 1 {
		t.Fatalf("resolved %+v, want a plain `open <url>`", launcher)
	}

	// With one installed, app mode — and the bundle that exists, not the first name.
	installed["Brave Browser"] = true
	launcher, err = ResolveLauncher("http://127.0.0.1:41780/widget", true, "darwin",
		installed2("open"), environment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.AppWindow {
		t.Fatalf("an installed browser did not give an app window: %+v", launcher)
	}
	joined := strings.Join(launcher.Args, " ")
	if !strings.Contains(joined, "Brave Browser") {
		t.Fatalf("args %q name a browser that is not installed", joined)
	}
	if !strings.Contains(joined, "--app=http://127.0.0.1:41780/widget") {
		t.Fatalf("args %q carry no app flag", joined)
	}
}

// installed2 is installed() under another name, so this file's helper and the bundle stub
// above cannot be confused for one another.
func installed2(names ...string) LookPath { return installed(names...) }

// The escalation, which is the whole point of the change: a real application window where
// this machine has one, a chromeless browser window where it does not, an ordinary window
// after that, and the address printed when there is nothing at all.
func TestAnApplicationWindowIsPreferredToABrowser(t *testing.T) {
	helpers := map[string]string{}
	restore := helperPathFor
	helperPathFor = func(name string) (string, bool) {
		path, found := helpers[name]
		return path, found
	}
	t.Cleanup(func() { helperPathFor = restore })

	bundles := map[string]bool{"Google Chrome": true}
	restoreBundle := appBundleExists
	appBundleExists = func(bundle string) bool { return bundles[bundle] }
	t.Cleanup(func() { appBundleExists = restoreBundle })

	display := environment(map[string]string{"DISPLAY": ":0"})

	// With no helper, the behaviour that existed before: a Chromium browser in app mode.
	launcher, err := ResolveLauncher("http://cache/widget", true, "linux",
		installed("chromium", "xdg-open"), display)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.Native || launcher.Program != "chromium" {
		t.Fatalf("with no helper installed, resolved %+v", launcher)
	}

	// With one, it wins — even though a browser is available.
	helpers["pkgcache-window"] = "/usr/local/bin/pkgcache-window"
	launcher, err = ResolveLauncher("http://cache/widget", true, "linux",
		installed("chromium", "xdg-open"), display)
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.Native || !launcher.AppWindow {
		t.Fatalf("the application window did not win: %+v", launcher)
	}
	if launcher.Program != "/usr/local/bin/pkgcache-window" ||
		len(launcher.Args) != 1 || launcher.Args[0] != "http://cache/widget" {
		t.Fatalf("resolved %+v, want the helper with just the URL", launcher)
	}

	// macOS runs both jobs from one helper, so it has to be told which is wanted.
	helpers["pkgcache-menubar"] = "/usr/local/bin/pkgcache-menubar"
	launcher, err = ResolveLauncher("http://cache/widget", true, "darwin",
		installed("open"), environment(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.Native || strings.Join(launcher.Args, " ") != "-window http://cache/widget" {
		t.Fatalf("darwin resolved %+v", launcher)
	}

	// And -tab still asks for a browser, helper or no helper: somebody who said tab meant
	// tab.
	launcher, err = ResolveLauncher("http://cache/widget", false, "linux",
		installed("chromium", "xdg-open"), display)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.Native {
		t.Fatalf("-tab was given an application window: %+v", launcher)
	}
}
