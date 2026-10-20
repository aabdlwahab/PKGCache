//go:build !windows

package local

import (
	"context"
	"testing"
	"time"
)

// A prune that succeeds has to return. Collect waits on the job's status, and waiting for
// a word the job manager never writes is indistinguishable from a slow collection until
// somebody notices their terminal has sat there for an hour: this is that hour, bounded.
func TestCollectReturnsWhenTheCollectionFinishes(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Collect(ctx, snap); err != nil {
		t.Fatalf("a prune of an empty cache did not finish: %v", err)
	}
}
