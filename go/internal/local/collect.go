package local

import (
	"context"
	"fmt"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// openIdle opens the store for this process alone, once any daemon has let go of it.
//
// Stop reports a daemon stopped when it stops answering, and a daemon that has closed its
// listener can still be draining its transfers — and holding the store — for its whole
// shutdown grace. Opening the store in that window made two writers of a single-writer
// store, and the newcomer's startup sweep deleted the staging files of downloads the old
// daemon was still writing: `pkgcache setup` on a sibling that was serving another
// machine removed nine of them. The daemon holds its lock until it has exited, so taking
// the same lock is what turns "stopped answering" into "gone"; holding it until the store
// is closed also keeps a new daemon from starting underneath. migrate waits the same way.
func openIdle(ctx context.Context, snap *config.Snapshot) (*app.App, func(), error) {
	lock, err := acquireIdle(ctx, snap.DataDir, snap.Server.ShutdownGrace+30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	instance, err := app.Open(snap) //nolint:contextcheck // single-writer storage; its lifetime is the process's, not a request's
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	return instance, func() {
		_ = instance.Close()
		_ = lock.Close()
	}, nil
}

// Collect reclaims space, in this process, because the user asked for it.
//
// It opens the store directly rather than asking a running daemon, and the caller is
// expected to have stopped one first. That ordering is the point: the store is
// single-writer, and a collection is the one operation where "who owns the store right
// now" must not be ambiguous. The cost is a restart on the next command, which is
// invisible because the next command starts a daemon anyway.
func Collect(ctx context.Context, snap *config.Snapshot) error {
	instance, release, err := openIdle(ctx, snap)
	if err != nil {
		return err
	}
	defer release()

	record, err := instance.Jobs.Submit("", "gc", "pkgcache", map[string]any{})
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := instance.Jobs.Get(record.ID)
		if err != nil {
			return err
		}
		switch current.Status {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("local: prune %s: %s", current.Status, current.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
