package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/buildinfo"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// startTimeout bounds how long a client waits for a daemon it started. Opening two
// SQLite databases and sweeping interrupted downloads is the slow part, and on a cold
// cache with a large store it is still seconds, not tens of seconds.
const startTimeout = 30 * time.Second

// EnsureOptions configures auto-start.
type EnsureOptions struct {
	Snapshot *config.Snapshot
	// Notes receives progress a person should see — "starting the cache" — which is
	// worth saying because the first command after a reboot pauses for a second and an
	// unexplained pause reads as a hang.
	Notes io.Writer
	// Executable overrides the binary re-executed as the daemon. Tests set it; a real
	// run re-executes itself, so a client and its daemon can never be different builds.
	Executable string
	// NoStart returns ErrNoDaemon rather than starting one. `pkgcache status` uses it:
	// asking whether the cache is running should not be the thing that starts it.
	NoStart bool
	// DifferentBinary declares that Executable is a different program from the caller,
	// so a daemon whose version does not match this build is normal rather than stale.
	//
	// This exists because the version check below assumes what spawn's comment states:
	// that a client and its daemon are the same build, because the client re-executes
	// itself. The desktop app broke that — it is its own binary and starts pkgcache — and
	// without this the check fires on every single call. On the NoStart path that meant
	// stopping the daemon every two seconds, restarting it, and stopping it again, which
	// looks from the outside like a cache that will not stay up.
	//
	// Keeping the daemon's version honest is then pkgcache's own business: any pkgcache
	// command will notice a stale daemon and replace it. An app that is only watching
	// should not be policing it.
	DifferentBinary bool
}

// Ensure returns a daemon that is serving this cache, starting one if necessary.
//
// Concurrency is the whole difficulty. Twenty `pkgcache run` invocations in a Makefile
// all reach this at once, and all of them find no daemon. Without the start lock,
// twenty processes spawn twenty daemons, nineteen of which fail on the directory lock
// after paying for two database opens — noisy, slow, and alarming in a log. With it,
// one starts a daemon and nineteen wait and then use it.
func Ensure(ctx context.Context, o EnsureOptions) (State, error) {
	dataDir := o.Snapshot.DataDir

	if state, err := ReadState(dataDir); err == nil {
		switch {
		case !state.Alive(ctx):
			// Stale: the daemon died without tidying up. Nothing to do here — the file
			// is replaced by whoever starts the next one.
		case !o.DifferentBinary && state.Version != buildinfo.Get().Version:
			// An upgraded binary talking to a daemon from the previous version is the
			// one way this design can serve yesterday's behaviour indefinitely, since
			// nothing else ever restarts it.
			note(o.Notes, "pkgcache: replacing the daemon from version %s\n", state.Version)
			if _, err := Stop(ctx, dataDir, 10*time.Second); err != nil {
				return State{}, err
			}
		default:
			return state, nil
		}
	} else if !errors.Is(err, ErrNoDaemon) {
		return State{}, err
	}

	if o.NoStart {
		return State{}, ErrNoDaemon
	}
	// Checked here, not only in the daemon. The daemon refuses to start without a
	// budget and exits immediately; a client that did not look first would spawn it,
	// wait out the readiness timeout and then report "the cache did not start", which
	// is true and useless. The answer the person needs is one file read away.
	if _, err := ReadBudget(o.Snapshot.DataDir); err != nil {
		return State{}, err
	}
	if err := o.Snapshot.EnsureDirs(); err != nil {
		return State{}, err
	}

	lock, err := Acquire(StartLockPath(dataDir), true)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = lock.Close() }()

	// Someone else may have started one while we waited for the lock. This second look
	// is what turns twenty spawns into one.
	if state, err := ReadState(dataDir); err == nil && state.Alive(ctx) &&
		(o.DifferentBinary || state.Version == buildinfo.Get().Version) {
		return state, nil
	}

	note(o.Notes, "pkgcache: starting the cache\n")
	if err := spawn(o); err != nil {
		return State{}, err
	}
	return waitReady(ctx, dataDir)
}

// spawn re-executes this binary as a detached daemon, with its output appended to the
// cache's log.
//
// Re-executing os.Executable rather than looking up "pkgcache" on PATH means a client
// and the daemon it starts are always the same build, which is what makes the version
// check above a rare event rather than a routine one.
func spawn(o EnsureOptions) error {
	executable := o.Executable
	if executable == "" {
		found, err := os.Executable()
		if err != nil {
			return fmt.Errorf("local: locate this program: %w", err)
		}
		executable = found
	}
	snap := o.Snapshot
	args := []string{
		"serve",
		"-data-dir", snap.DataDir,
		"-addr", snap.LocalAddr(),
		"-idle-timeout", snap.Local.IdleTimeout.String(),
	}
	if snap.Upstream.Offline {
		args = append(args, "-offline")
	}
	logFile, err := os.OpenFile(
		LogPath(snap.DataDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("local: open daemon log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- the executable is this program and every argument is derived from
	// a validated snapshot, not from user input.
	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Inherit nothing that would redirect the daemon somewhere else. The client has
	// already resolved every setting and passes them as arguments.
	cmd.Env = append(os.Environ(), config.LocalEnvPrefix+"DATA_DIR="+snap.DataDir)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("local: start the cache: %w", err)
	}
	// Waited on in the background rather than released, which covers both directions of
	// a race that Release does not.
	//
	// The daemon is this process's child until this process exits. In the normal case —
	// a short `pkgcache run` — the client goes first and the daemon is reparented to
	// init, which reaps it. But a client can outlive the daemon: a `pkgcache shell` open
	// for hours will still be here when a 15-minute idle timeout fires. Nobody would
	// then reap it, and the exited daemon would linger as a zombie whose pid still
	// answers a liveness signal — so a later start would consult the process table and
	// be told a dead cache is running.
	go func() { _ = cmd.Wait() }()
	return nil
}

// waitReady polls for the state file the daemon publishes once it is serving.
func waitReady(ctx context.Context, dataDir string) (State, error) {
	deadline := time.Now().Add(startTimeout)
	for {
		if state, err := ReadState(dataDir); err == nil && state.Alive(ctx) {
			return state, nil
		}
		if ctx.Err() != nil {
			return State{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return State{}, fmt.Errorf(
				"local: the cache did not start within %s; see %s",
				startTimeout, LogPath(dataDir))
		}
		select {
		case <-ctx.Done():
			return State{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Stop shuts a daemon down and waits for it to go. It reports whether one was running.
//
// SIGTERM first, always: the daemon drains in-flight downloads and flushes the
// catalog's batched writes on that path. A daemon that ignores it is killed, because a
// stop command that can hang forever is a stop command people stop using.
func Stop(ctx context.Context, dataDir string, wait time.Duration) (bool, error) {
	state, err := ReadState(dataDir)
	if err != nil {
		if errors.Is(err, ErrNoDaemon) {
			return false, nil
		}
		return false, err
	}
	if !state.Alive(ctx) {
		RemoveState(dataDir)
		return false, nil
	}
	if err := terminate(state.PID); err != nil {
		return false, fmt.Errorf("local: stop the cache: %w", err)
	}
	// Waiting on Alive rather than on the pid alone. What matters is whether the cache
	// is still serving, and a pid outlives that in two ways: a daemon draining a large
	// download has closed its listener but not yet exited, and a daemon whose parent has
	// not reaped it stays in the process table as a zombie that still answers a signal.
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !state.Alive(ctx) {
			RemoveState(dataDir)
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := kill(state.PID); err != nil && state.Alive(ctx) {
		return true, fmt.Errorf("local: the cache did not stop: %w", err)
	}
	RemoveState(dataDir)
	return true, nil
}

func note(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format, args...)
}
