package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

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

	err = tray.Run(ctx, tray.Options{
		Read:  func() tray.State { return trayState(ctx, snap) },
		Do:    func(action tray.Action) error { return trayDo(ctx, snap, action) },
		Notes: func(line string) { fmt.Fprintln(os.Stderr, line) },
		// Only macOS uses this, and it starts the cache: opening the window is somebody
		// asking to look at it, which is the one action allowed to wake a sleeping daemon.
		Window: func() string {
			daemon, err := reachRegistry(ctx, snap)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
				return ""
			}
			return daemon.BaseURL() + "/widget"
		},
	})
	if errors.Is(err, tray.ErrUnsupported) {
		// Not a failure: plenty of sessions have no status bar, and the window is the
		// whole feature anyway. The address is the useful half of the answer.
		fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		fmt.Println(snap.LocalBaseURL() + "/widget")
		return nil
	}
	return err
}

// trayState reads what the icon shows, without starting anything.
//
// Two sources, and which one answers is the point: a running daemon is asked over the API,
// and a cache that has gone idle is read off disk from the files it published on its way
// out. That is what lets the icon say "asleep, 8.3 GiB held" rather than going blank.
func trayState(ctx context.Context, snap *config.Snapshot) tray.State {
	state := tray.State{Project: local.CurrentProject(snap.DataDir), Served: -1, Limit: -1}
	if budget, err := local.ReadBudget(snap.DataDir); err == nil {
		state.Limit = budget.LimitBytes
	}
	if usage, _, found := local.ReadUsage(snap.DataDir); found {
		state.Used, state.Full, state.Reason = usage.Bytes, usage.Full, usage.Reason
	}

	daemon, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
	if err != nil {
		return state
	}
	state.Running = true
	// The live figures replace the published ones where they are available: usage.json is
	// whatever the daemon last measured, which may be minutes old.
	reports, err := local.ProjectReports(ctx, daemon)
	if err != nil {
		return state
	}
	for _, report := range reports {
		if report.Project != state.Project {
			continue
		}
		if rate, known := report.Served(); known {
			state.Served = rate
		}
	}
	return state
}

// trayDo performs a menu choice.
func trayDo(ctx context.Context, snap *config.Snapshot, action tray.Action) error {
	switch action {
	case tray.ActionWidget, tray.ActionConsole:
		path := "/widget"
		if action == tray.ActionConsole {
			path = "/console"
		}
		// The one action allowed to start the cache, because it is somebody asking to look
		// at it. Everything else is greyed while it sleeps.
		daemon, err := reachRegistry(ctx, snap)
		if err != nil {
			return err
		}
		launcher, err := local.ResolveLauncher(
			daemon.BaseURL()+path, action == tray.ActionWidget, local.GOOS(), nil, nil)
		if err != nil {
			return err
		}
		return local.OpenBrowser(ctx, launcher)

	case tray.ActionOffline:
		daemon, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
		if err != nil {
			return err
		}
		return local.ToggleOffline(ctx, daemon, local.CurrentProject(snap.DataDir))

	case tray.ActionPrune:
		daemon, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
		if err != nil {
			return err
		}
		return local.CollectVia(ctx, daemon)

	case tray.ActionStop:
		_, err := local.Stop(ctx, snap.DataDir, 15*time.Second)
		return err
	}
	return nil
}
