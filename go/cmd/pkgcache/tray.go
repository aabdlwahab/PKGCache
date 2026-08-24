package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache tray` — start the app, or say why it cannot.
//
// This used to be the status bar item: three platform implementations behind one interface,
// driven from this file. It is now pkgcache-app's job, and what is left here is a way to
// ask for it from a terminal, because a terminal is a reasonable place to ask.
//
// Kept rather than removed. Somebody who has typed `pkgcache tray` for months should get
// the icon, not an unknown-command error.

func runTray(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache tray — keep the cache in your status bar

usage: pkgcache tray

Starts the desktop app, which puts one icon in the status bar: the notification area on
Windows, a StatusNotifierItem on Linux, the menu bar on macOS. It shows whether the cache
is working, says so when it has filled up and stopped storing, and its menu opens the
window.

It never keeps the cache running. When nothing has used the cache for a while the daemon
exits as usual and the icon says "asleep" — the items that need it are greyed until
something starts it again.

The app is a separate program, `+"`pkgcache-app`"+`, installed beside this one. `+
			"`pkgcache-app -h`"+` is its own help.

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
	if _, err := config.LoadLocal(collect()); err != nil {
		return err
	}

	app, found := local.AppPath()
	if !found {
		return fmt.Errorf(
			"the desktop app is not installed on this machine.\n" +
				"  The status bar icon lives in `pkgcache-app`, which installs beside this\n" +
				"  binary — see packaging/README.md. Without it, `pkgcache widget` still\n" +
				"  opens the console in a browser.")
	}

	// The login entry names the app, not this binary: it is the app that has to start.
	if *onLogin || *offLogin {
		return local.InstallAutostart(local.AutostartOptions{
			Executable: app, Command: "app",
			Remove: *offLogin, DryRun: *dryRun, Out: os.Stdout,
		})
	}
	return local.StartApp(ctx, app, true)
}
