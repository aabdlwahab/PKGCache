package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/brightskies/pkgreg/internal/local"
)

func runStop(ctx context.Context, args []string) error {
	snap, err := loadSnapshot("stop", args, `pkgcache stop — stop the cache daemon

usage: pkgcache stop [flags]

Asks the daemon to finish what it is doing and exit. In-flight downloads complete and
queued catalog writes are flushed first.

flags:
`)
	if err != nil {
		return err
	}
	stopped, err := local.Stop(ctx, snap.DataDir, 15*time.Second)
	if err != nil {
		return err
	}
	if !stopped {
		fmt.Println("pkgcache: no daemon was running")
		return nil
	}
	fmt.Println("pkgcache: stopped")
	return nil
}

func runStatus(ctx context.Context, args []string) error {
	snap, err := loadSnapshot("status", args, `pkgcache status — is the cache running, and what is in it

usage: pkgcache status [flags]

Reports without starting anything: asking whether the cache is running should not be
the thing that starts it.

flags:
`)
	if err != nil {
		return err
	}
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, NoStart: true})
	if err != nil && !errors.Is(err, local.ErrNoDaemon) {
		return err
	}
	fmt.Printf("cache      %s\n", snap.DataDir)
	if errors.Is(err, local.ErrNoDaemon) {
		fmt.Printf("daemon     not running\n")
		fmt.Printf("address    %s (when started)\n", snap.LocalBaseURL())
		return reportBudget(snap.DataDir, snap.BlobRoot())
	}
	fmt.Printf("daemon     running, pid %d, up %s\n",
		state.PID, state.Uptime().Round(time.Second))
	fmt.Printf("address    %s\n", state.BaseURL())
	fmt.Printf("version    %s\n", state.Version)
	return reportBudget(snap.DataDir, snap.BlobRoot())
}

// reportBudget prints what the cache holds against what it is allowed to hold, and is
// the second of the four channels: a non-zero status while the cache is full, from a
// command whose whole job is to answer "is this healthy?".
func reportBudget(dataDir, blobRoot string) error {
	budget, err := local.ReadBudget(dataDir)
	if errors.Is(err, local.ErrNoLimit) {
		fmt.Println("limit      not set — pkgcache will not serve until it is")
		fmt.Println("           pkgcache limit 25G     or     pkgcache limit none")
		return &exitError{code: 1}
	}
	if err != nil {
		return err
	}
	if size, err := directorySize(blobRoot); err == nil {
		if budget.LimitBytes == local.NoLimit {
			fmt.Printf("size       %s of no limit\n", local.FormatSize(size))
		} else {
			fmt.Printf("size       %s of %s\n",
				local.FormatSize(size), local.FormatSize(budget.LimitBytes))
		}
	}
	usage, sampled, found := local.ReadUsage(dataDir)
	if found && usage.Full {
		fmt.Printf("\nCACHE FULL — %s\n", usage.Reason)
		fmt.Printf("(measured %s ago)\n", time.Since(sampled).Round(time.Second))
		return &exitError{code: local.FullExitCode}
	}
	return nil
}

// directorySize adds up what the cache is holding.
//
// A walk rather than the store's own accounting, because status runs in a client
// process and the daemon owns the databases. It is honest but not free on a large
// store; status is a command someone runs deliberately, and the catalog-backed figure
// arrives with the disk policy.
func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A file removed mid-walk is normal on a live cache and not worth failing
			// a status report over.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
