//go:build !windows

package local

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// A daemon on its way out has stopped answering but still holds the cache's lock while it
// closes its databases, and whatever is starting its replacement has already decided it
// is gone. The replacement used to refuse at once with "already running", and the client
// that started it waited out its whole start timeout for a cache that was never coming.
// It now waits for the lock, and serves once it has it.
func TestADaemonWaitsForTheOneItReplaces(t *testing.T) {
	snap := testSnapshot(t, 0)
	if err := snap.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	outgoing, err := Acquire(LockPath(snap.DataDir), false)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second*startBudgetFactor)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Run(ctx, RunOptions{Snapshot: snap, Notes: os.Stderr, Ready: ready}) }()

	// Long enough that a daemon refusing at once has certainly returned.
	select {
	case err := <-done:
		t.Fatalf("the daemon gave up while the lock was still held: %v", err)
	case <-ready:
		t.Fatal("the daemon served while another process held the cache's lock")
	case <-time.After(500 * time.Millisecond):
	}
	if err := outgoing.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the daemon did not start once the lock was released: %v", err)
	case <-ctx.Done():
		t.Fatal("the daemon did not start once the lock was released")
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("the daemon did not stop cleanly: %v", err)
	}
}

// The wait is for a holder on its way out, so one that stays is still refused once it
// is over: two daemons on one directory is corruption, not contention.
func TestWaitingForALockThatStaysHeldGivesUp(t *testing.T) {
	path := LockPath(t.TempDir())
	held, err := Acquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	started := time.Now()
	if _, err := waitForLock(t.Context(), path, 200*time.Millisecond); !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if waited := time.Since(started); waited < 200*time.Millisecond {
		t.Fatalf("gave up after %s, before the wait was over", waited)
	}
}
