package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/brightskies/pkgreg/internal/app"
	"github.com/brightskies/pkgreg/internal/config"
)

// bindLocalFlags wires the flags every pkgcache command that can start a daemon
// accepts. Kept in one place so `serve` and the commands that auto-start it in later
// milestones cannot disagree about what -addr means.
func bindLocalFlags(fs *flag.FlagSet) func() config.LocalFlags {
	var (
		dataDir     = fs.String("data-dir", "", "cache directory (default: this user's)")
		addr        = fs.String("addr", "", "loopback address or port to serve on")
		logLevel    = fs.String("log-level", "", "debug|info|warn|error")
		offline     = fs.Bool("offline", false, "serve from cache only; never contact an upstream")
		idleTimeout = fs.Duration("idle-timeout", 0, "exit after this long with nothing to do (0 stays up)")
	)
	return func() config.LocalFlags {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		out := config.LocalFlags{DataDir: *dataDir, Addr: *addr, LogLevel: *logLevel}
		if set["offline"] {
			out.Offline = offline
		}
		if set["idle-timeout"] {
			out.IdleTimeout = idleTimeout
		}
		return out
	}
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache serve — run the cache in the foreground

usage: pkgcache serve [flags]

Binds one loopback port serving pypi, npm, oci, git, files and the apt/apk forward
proxy, plus the console and the API. Refuses any address another machine can reach.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	a, err := app.Open(snap)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	for _, issue := range snap.Posture(a.Accounts.Enabled()) {
		a.Log.Info(issue.Summary, "issue", issue.ID)
	}

	runtime, err := a.StartListeners()
	if err != nil {
		return err
	}
	a.Log.Info("ready", "address", snap.LocalBaseURL(), "cache", snap.DataDir)

	select {
	case serveErr := <-runtime.Errors():
		return shutdown(ctx, a, runtime, snap, serveErr)
	case <-ctx.Done():
		return shutdown(ctx, a, runtime, snap, nil)
	}
}

// shutdown drains listeners and in-flight fetches. The context passed in is already
// cancelled on the signal path, so the grace period gets a fresh one.
func shutdown(
	_ context.Context, a *app.App, runtime *app.Runtime,
	snap *config.Snapshot, serveErr error,
) error {
	a.Log.Info("stopping", "grace", snap.Server.ShutdownGrace)
	grace := snap.Server.ShutdownGrace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		a.Log.Warn("listeners did not drain cleanly", "error", err)
		if serveErr == nil {
			serveErr = err
		}
	}
	if serveErr != nil {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
