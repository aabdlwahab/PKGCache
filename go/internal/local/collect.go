package local

import (
	"context"
	"fmt"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Collect reclaims space, in this process, because the user asked for it.
//
// It opens the store directly rather than asking a running daemon, and the caller is
// expected to have stopped one first. That ordering is the point: the store is
// single-writer, and a collection is the one operation where "who owns the store right
// now" must not be ambiguous. The cost is a restart on the next command, which is
// invisible because the next command starts a daemon anyway.
func Collect(ctx context.Context, snap *config.Snapshot) error {
	instance, err := app.Open(snap) //nolint:contextcheck // single-writer storage; its lifetime is the process's, not a request's
	if err != nil {
		return err
	}
	defer func() { _ = instance.Close() }()

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
