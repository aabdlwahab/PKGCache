package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	snap, err := config.Load(collect())
	if err != nil {
		return err
	}
	a, err := app.Open(snap)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	// Before binding anything: say what this configuration exposes. An operator who is
	// about to serve an unauthenticated control plane or an open relay on every
	// interface should learn it from the process that is doing it, at the moment it
	// starts, rather than from a security review months later.
	logPosture(a, snap)

	runtime, err := a.StartListeners()
	if err != nil {
		return err
	}
	a.Log.Info("listeners up", "addresses", runtime.Addresses(),
		"single_port", snap.Server.SinglePort, "console", a.Console.Enabled())

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	var serveErr error
running:
	for {
		select {
		case serveErr = <-runtime.Errors():
			break running
		case <-ctx.Done():
			break running
		case <-hup:
			if err := runtime.ReloadTLS(); err != nil {
				a.Log.Warn("TLS reload rejected; keeping current certificate", "error", err)
			} else {
				a.Log.Info("TLS certificate reloaded")
			}
		}
	}

	a.Log.Info("shutting down", "grace", snap.Server.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), snap.Server.ShutdownGrace)
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

// logPosture emits one line per security-relevant weakness in this configuration.
//
// At Error level for the critical ones, deliberately. They are not runtime errors, but
// every log pipeline in existence surfaces Error and samples Info, and "an anonymous
// caller can administer this instance" needs to reach the surface on the first start,
// not on the day someone reads the config file closely.
//
// serve reports and continues; `pkgreg doctor` fails on the same findings. That split
// is intentional: refusing to start would turn a hardening change into an outage for
// every existing deployment, while a diagnostic is free to be strict.
func logPosture(a *app.App, snap *config.Snapshot) {
	for _, issue := range snap.Posture(a.Accounts.Enabled()) {
		switch issue.Severity {
		case config.SeverityCritical:
			a.Log.Error("INSECURE CONFIGURATION: "+issue.Summary,
				"issue", issue.ID, "remedy", issue.Remedy)
		case config.SeverityWarn:
			a.Log.Warn(issue.Summary, "issue", issue.ID, "remedy", issue.Remedy)
		default:
			a.Log.Info(issue.Summary, "issue", issue.ID)
		}
	}
}
