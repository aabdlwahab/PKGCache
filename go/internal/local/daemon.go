package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/brightskies/pkgreg/internal/app"
	"github.com/brightskies/pkgreg/internal/buildinfo"
	"github.com/brightskies/pkgreg/internal/config"
)

// ErrAlreadyRunning reports that a daemon already owns this cache directory.
var ErrAlreadyRunning = errors.New("local: a pkgcache daemon is already running for this cache")

// RunOptions configures one daemon.
type RunOptions struct {
	Snapshot *config.Snapshot
	// Notes receives the few things a person needs to be told rather than logged, such
	// as a port fallback. Nil discards them.
	Notes io.Writer
	// Ready, when non-nil, is closed once the daemon is serving. Tests use it; nothing
	// in production needs it, because readiness is published in the state file.
	Ready chan<- struct{}
}

// Run serves until the context is cancelled or the daemon has been idle long enough.
//
// It holds the cache directory's lock for its whole life. That lock, not the state
// file, is what guarantees one writer: the catalog and the blob store are
// single-writer, so a second daemon on the same directory is corruption rather than
// contention.
func Run(ctx context.Context, o RunOptions) error {
	snap := o.Snapshot
	if err := snap.EnsureDirs(); err != nil {
		return err
	}
	lock, err := Acquire(LockPath(snap.DataDir), false)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return fmt.Errorf("%w: %s", ErrAlreadyRunning, snap.DataDir)
		}
		return err
	}
	defer func() { _ = lock.Close() }()

	// A socket handed to us by systemd or launchd is already bound, and its address —
	// not the configured one — is what clients must be told. Where there is none, the
	// port is chosen before the engine opens: a busy port then costs nothing, rather
	// than two database opens discovered to be wasted.
	activated, err := ActivationListener()
	if err != nil {
		return err
	}
	if activated != nil {
		if err := snapSetAddr(snap, activated.Addr().String()); err != nil {
			return err
		}
	} else if err := resolveAddr(snap, o.Notes); err != nil {
		return err
	}

	// The budget is read before anything opens, so a cache with no limit refuses at
	// startup rather than on the first download. See ErrNoLimit: choosing a size is a
	// decision pkgcache asks for once, rather than a default it picks for somebody
	// else's disk.
	budget, budgetErr := ReadBudget(snap.DataDir)
	if budgetErr != nil {
		return budgetErr
	}
	guard := NewGuard(nil, snap.DataDir, budget, func(reason string) {
		Notify("pkgcache: the cache is full", reason)
	})

	openOptions := []app.Option{app.WithStoreGuard(guard)}
	if activated != nil {
		openOptions = append(openOptions, app.WithListener(activated))
	}
	a, err := app.Open(snap, openOptions...)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	guard.attach(a.Blobs)

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	a.Activity = func() { lastActivity.Store(time.Now().UnixNano()) }

	for _, issue := range snap.Posture(a.Accounts.Enabled()) {
		a.Log.Info(issue.Summary, "issue", issue.ID)
	}

	runtime, err := a.StartListeners()
	if err != nil {
		return err
	}
	// The bound address, not the requested one: it differs whenever the fixed port was
	// taken and an ephemeral one was used instead, and every client reads this.
	bound := runtime.Addresses()["single"]
	if bound == "" {
		bound = snap.LocalAddr()
	}
	state := State{
		PID:     os.Getpid(),
		Addr:    bound,
		Version: buildinfo.Get().Version,
		Started: time.Now(),
	}
	if err := WriteState(snap.DataDir, state); err != nil {
		_ = runtime.Shutdown(context.Background())
		return err
	}
	defer RemoveState(snap.DataDir)

	a.Log.Info("ready", "address", state.BaseURL(), "cache", snap.DataDir,
		"idle_timeout", snap.Local.IdleTimeout, "activated", activated != nil)
	if o.Ready != nil {
		close(o.Ready)
	}

	idle := watchIdle(ctx, a, &lastActivity, snap.Local.IdleTimeout)

	var serveErr error
	select {
	case serveErr = <-runtime.Errors():
	case <-ctx.Done():
	case <-idle:
		a.Log.Info("idle; exiting", "after", snap.Local.IdleTimeout)
	}

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
		return fmt.Errorf("local: serve: %w", serveErr)
	}
	return nil
}

// resolveAddr keeps the fixed port when it is free and falls back to an ephemeral one
// when it is not.
//
// The fallback is announced rather than silent, because it is exactly the case that
// persistent client settings cannot follow: a .npmrc naming :41780 will not reach a
// daemon that had to move. Clients started through `pkgcache run` read the real
// address from the state file and are unaffected.
func resolveAddr(snap *config.Snapshot, notes io.Writer) error {
	address := snap.LocalAddr()
	listener, err := net.Listen("tcp", address)
	if err == nil {
		// Closed immediately and re-bound by the listener layer a moment later. The gap
		// is a race in principle; losing it produces a clear bind error rather than a
		// wrong result, and the alternative is threading a pre-bound socket through a
		// composition root that has no other reason to accept one.
		_ = listener.Close()
		return nil
	}
	fallback, fallbackErr := net.Listen("tcp", net.JoinHostPort(config.LocalLoopback, "0"))
	if fallbackErr != nil {
		return fmt.Errorf("local: cannot bind %s, and no loopback port is available: %w",
			address, err)
	}
	chosen := fallback.Addr().String()
	_ = fallback.Close()
	if notes != nil {
		fmt.Fprintf(notes,
			"pkgcache: %s is in use by another program; serving on %s instead.\n"+
				"  Settings written by `pkgcache persist` name the fixed port and will not\n"+
				"  reach this daemon. `pkgcache run` and `pkgcache shell` are unaffected.\n",
			address, chosen)
	}
	return snapSetAddr(snap, chosen)
}

func snapSetAddr(snap *config.Snapshot, address string) error {
	snap.Server.UnifiedAddr = address
	snap.Server.ProxyAddr = address
	snap.Server.AdminAddr = address
	return snap.Validate()
}

// watchIdle closes its channel once nothing has used this cache for the timeout and no
// fetch is still running.
//
// The in-flight check is not belt and braces. A 2.5 GB wheel takes minutes during
// which no new request arrives, and exiting underneath it would abandon a download
// that other readers are attached to — the one thing the engine's detached fetch
// context exists to prevent.
func watchIdle(
	ctx context.Context, a *app.App, last *atomic.Int64, timeout time.Duration,
) <-chan struct{} {
	done := make(chan struct{})
	if timeout <= 0 {
		// Zero means stay up until stopped, which is what persistent client settings
		// need: a .npmrc pointing at a daemon that exits is worse than no cache at all.
		return done
	}
	go func() {
		interval := timeout / 4
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				idleFor := time.Since(time.Unix(0, last.Load()))
				if idleFor >= timeout && a.Engine.Inflight().Len() == 0 {
					close(done)
					return
				}
			}
		}
	}()
	return done
}
