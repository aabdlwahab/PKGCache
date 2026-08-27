package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// A window onto the cache.
//
// The console is already a complete UI, embedded in this binary and served on the
// loopback port. What it lacked on a laptop was a window: nobody keeps a browser tab
// open to watch a cache.
//
// That window is now pkgcache-app, so `widget` starts it. Where the app is not installed —
// a server, a container, a machine that took the daemon package and not the desktop one —
// the browser paths below are still here and still work: a Chromium-family app-mode window
// first, an ordinary tab second, and the printed address last.
//
// The daemon is started if it is not running, and then left alone. Neither command holds
// it open: the page reads the cache, it is not a reason for the cache to exist, and a
// window that defeated the idle timeout would quietly undo the politeness that makes this
// pleasant to leave installed.

func runWidget(ctx context.Context, args []string) error {
	return openWindow(ctx, args, "widget", "/widget",
		`pkgcache widget — a small window that watches this cache

usage: pkgcache widget [flags]

Starts the desktop app, which shows what is downloading, how much of your limit is used,
how much is being served from here, and says so when the cache has filled up and stopped
storing.

Where the app is not installed the same page opens in a browser instead: a window with no
tabs and no address bar where a Chromium-family browser supports one, an ordinary window
where it does not. -tab asks for the browser even when the app is there.

Starts the cache if it is not running, then leaves it to its own idle timeout.

-on-login starts it with your session, and -off-login stops that again. It watches the
cache; it never keeps it running.

flags:
`)
}

func runConsole(ctx context.Context, args []string) error {
	return openWindow(ctx, args, "console", "/console",
		`pkgcache console — open the full console in your browser

usage: pkgcache console [flags]

The same console a pkgreg server serves, for this machine's cache: inventory, sources,
transfers, statistics and the jobs behind them. `+"`pkgcache widget`"+` is the small
always-open version of it.

flags:
`)
}

func openWindow(ctx context.Context, args []string, name, path, usage string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	print := fs.Bool("print", false, "print the address and open nothing")
	plain := fs.Bool("tab", false, "open an ordinary browser window rather than an app window")
	var onLogin, offLogin *bool
	if name == "widget" {
		onLogin = fs.Bool("on-login", false, "open this window when you log in")
		offLogin = fs.Bool("off-login", false, "stop opening it on login")
	}
	dryRun := fs.Bool("dry-run", false, "print what -on-login would write, and write nothing")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}

	// The login entry is a desktop file, not a cache operation: it starts no daemon and
	// opens no window. Handled before anything else for that reason.
	if onLogin != nil && (*onLogin || *offLogin) {
		// The same entry `pkgcache tray -on-login` writes, deliberately. Both flags mean
		// "have this waiting for me when I log in", the thing that waits is the app, and
		// two entries starting one program would be two icons.
		app, found := local.AppPath()
		if !found {
			return fmt.Errorf(
				"the desktop app is not installed, so there is nothing to start at login.\n" +
					"  The window lives in `pkgcache-app` — see packaging/README.md")
		}
		return local.InstallAutostart(local.AutostartOptions{
			Executable: app, Command: "app",
			Remove: *offLogin, DryRun: *dryRun, Out: os.Stdout,
		})
	}

	// -print does not start anything. It is what a script or a tiling window manager
	// wants, and starting a daemon as a side effect of being asked for a URL would be a
	// surprise in both.
	url := snap.LocalBaseURL() + path
	if *print {
		state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
		if err == nil {
			url = state.BaseURL() + path
		} else if !errors.Is(err, local.ErrNoDaemon) {
			return err
		}
		fmt.Println(url)
		return nil
	}

	// The app first, and only for the widget: `console` is the full operator console
	// and belongs in a real browser with tabs and a URL bar, which is where somebody
	// reading statistics wants it. -tab is the way out for anybody who would rather
	// have the page than the app.
	if name == "widget" && !*plain {
		if app, found := local.AppPath(); found {
			return local.StartApp(ctx, app, false)
		}
	}

	state, err := reachRegistry(ctx, snap)
	if err != nil {
		return err
	}
	url = state.BaseURL() + path

	launcher, err := local.ResolveLauncher(
		url, name == "widget" && !*plain, local.GOOS(), nil, nil)
	if err != nil {
		if errors.Is(err, local.ErrNoBrowser) {
			// Not a failure worth an error: there is no browser over SSH, in CI or in a
			// container, and the address is the useful half of the answer anyway.
			fmt.Fprintf(os.Stderr, "pkgcache: no browser here — open this yourself:\n")
			fmt.Println(url)
			return nil
		}
		return err
	}
	if err := local.OpenBrowser(ctx, launcher); err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		fmt.Println(url)
		return nil
	}
	if !launcher.AppWindow && name == "widget" {
		// Said rather than silently delivered: somebody who asked for a widget and got a
		// browser tab should know why, and which browser would give them the window.
		fmt.Fprintln(os.Stderr,
			"pkgcache: opened in an ordinary window — a Chromium-family browser\n"+
				"  (Chrome, Chromium, Edge, Brave) gives one with no tabs or address bar")
	}
	fmt.Println(url)
	return nil
}
