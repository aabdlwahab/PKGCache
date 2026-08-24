package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aabdlwahab/PKGCache/internal/appcore"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/tray"
)

// `pkgcache tray` — the window, kept in the status bar.
//
// The one rule this obeys, and the reason it reads the way it does: the icon never keeps
// the cache alive. It reads the state files when nothing is running and says "asleep",
// because a status icon that held a daemon up would quietly remove the idle exit that
// makes this polite to leave installed. Every menu item that needs a running cache is
// greyed out rather than starting one — except opening the window, which is allowed to,
// because that is somebody asking for it.

func runTray(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache tray — keep the cache in your status bar

usage: pkgcache tray

Puts one icon in the status bar: the notification area on Windows, a StatusNotifierItem on
Linux, the menu bar on macOS. It shows whether the cache is working, says so when it has
filled up and stopped storing, and its menu opens the window.

It never keeps the cache running. When nothing has used the cache for a while the daemon
exits as usual and the icon says "asleep" — the items that need it are greyed until
something starts it again.

Runs until you quit it from the menu, so it belongs in a login entry rather than a
terminal: `+"`pkgcache tray -on-login`"+`.

flags:
`)
		fs.PrintDefaults()
	}
	onLogin := fs.Bool("on-login", false, "start the icon when you log in")
	offLogin := fs.Bool("off-login", false, "stop starting it on login")
	dryRun := fs.Bool("dry-run", false, "print what -on-login would write, and write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	if *onLogin || *offLogin {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate this program: %w", err)
		}
		return local.InstallAutostart(local.AutostartOptions{
			Executable: executable, Command: "tray",
			Remove: *offLogin, DryRun: *dryRun, Out: os.Stdout,
		})
	}

	core := appcore.New(snap)
	err = tray.Run(ctx, tray.Options{
		Read:  func() tray.State { return core.State(ctx) },
		Do:    func(action tray.Action) error { return trayDo(ctx, core, action) },
		Notes: func(line string) { fmt.Fprintln(os.Stderr, line) },
		// Only macOS uses this, and it starts the cache: opening the window is somebody
		// asking to look at it, which is the one action allowed to wake a sleeping daemon.
		Window: func() string {
			url, err := core.WindowURL(ctx, tray.ActionWidget)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
				return ""
			}
			return url
		},
	})
	if errors.Is(err, tray.ErrUnsupported) {
		// Not a failure: plenty of sessions have no status bar, and the window is the
		// whole feature anyway. The address is the useful half of the answer.
		fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		fmt.Println(core.FallbackURL())
		return nil
	}
	return err
}

// trayDo performs a menu choice on behalf of the command-line tray.
//
// Everything that acts on the cache is appcore's; what is left here is how *this* surface
// shows a page, which is a browser. The desktop app answers the same two actions with a
// native window, and that difference is the whole reason appcore does not decide it.
func trayDo(ctx context.Context, core *appcore.Core, action tray.Action) error {
	switch action {
	case tray.ActionWidget, tray.ActionConsole:
		url, err := core.WindowURL(ctx, action)
		if err != nil {
			return err
		}
		launcher, err := local.ResolveLauncher(
			url, action == tray.ActionWidget, local.GOOS(), nil, nil)
		if err != nil {
			return err
		}
		return local.OpenBrowser(ctx, launcher)
	}
	return core.Do(ctx, action)
}
