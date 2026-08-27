package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// reportFull writes the cache's state to stderr and says what exit status it earns.
//
// This is the stderr channel of the four, and it runs after every run and shell: the
// moment somebody is most able to act on "nothing is being cached" is the moment they
// have just finished waiting for a download they will have to repeat.
func reportFull(dataDir string) *exitError {
	usage, _, found := local.ReadUsage(dataDir)
	if !found || !usage.Full {
		return nil
	}
	fmt.Fprintf(os.Stderr, "\npkgcache: NOTHING WAS CACHED — %s\n", usage.Reason)
	return &exitError{code: local.FullExitCode}
}

func runLimit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("limit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	minFree := fs.String("min-free", "", "free-disk floor to keep underneath the limit")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache limit — set how much disk this cache may use

usage:
  pkgcache limit                 show the current limit
  pkgcache limit 25G             cap the cache at 25 GiB
  pkgcache limit none            no cap; the free-space floor still applies
  pkgcache limit -min-free 10G 25G

pkgcache never deletes anything on its own. When the limit is reached it keeps serving
and stops storing, and says so on four channels. Reclaim space deliberately with
`+"`pkgcache prune`"+`.

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

	current, readErr := local.ReadBudget(snap.DataDir)
	if fs.NArg() == 0 {
		if errors.Is(readErr, local.ErrNoLimit) {
			fmt.Println("no limit set")
			fmt.Println("  pkgcache limit 25G     cap the cache at 25 GiB")
			fmt.Println("  pkgcache limit none    no cap, disk floor only")
			return &exitError{code: 1}
		}
		if readErr != nil {
			return readErr
		}
		printBudget(current)
		return nil
	}

	limit, err := local.ParseSize(fs.Arg(0))
	if err != nil {
		return err
	}
	budget := local.Budget{LimitBytes: limit, MinFreeBytes: current.MinFreeBytes}
	if *minFree != "" {
		floor, err := local.ParseSize(*minFree)
		if err != nil {
			return fmt.Errorf("-min-free: %w", err)
		}
		if floor == local.NoLimit {
			return errors.New("-min-free needs a size; pkgcache always keeps some disk free")
		}
		budget.MinFreeBytes = floor
	}
	if budget.MinFreeBytes <= 0 {
		budget.MinFreeBytes = local.DefaultMinFree
	}
	if err := local.WriteBudget(snap.DataDir, budget); err != nil {
		return err
	}
	// The previous measurement described a cache with a different budget; keeping it
	// would report a cache full that has just been given room.
	local.ClearUsage(snap.DataDir)
	printBudget(budget)
	return nil
}

func printBudget(budget local.Budget) {
	if budget.LimitBytes == local.NoLimit {
		fmt.Println("limit      none")
	} else {
		fmt.Printf("limit      %s\n", local.FormatSize(budget.LimitBytes))
	}
	floor := budget.MinFreeBytes
	if floor <= 0 {
		floor = local.DefaultMinFree
	}
	fmt.Printf("disk floor %s free\n", local.FormatSize(floor))
}

func runPrune(ctx context.Context, args []string) error {
	snap, err := loadSnapshot("prune", args,
		`pkgcache prune — reclaim space, now, because you asked

usage: pkgcache prune [flags]

Removes cached content nothing refers to any more. pkgcache never does this on its
own: the packages in this cache are the ones your current work depends on, and a
background process deleting them to hold a number nobody chose is the wrong default on
a machine you are sitting in front of.

The daemon is stopped first, because the store has one writer. The next command starts
it again.

flags:
`)
	if err != nil {
		return err
	}
	stopped, err := local.Stop(ctx, snap.DataDir, 30*time.Second)
	if err != nil {
		return err
	}
	before := directorySizeOrZero(snap.BlobRoot())
	if err := local.Collect(ctx, snap); err != nil {
		return err
	}
	after := directorySizeOrZero(snap.BlobRoot())
	local.ClearUsage(snap.DataDir)

	if reclaimed := before - after; reclaimed > 0 {
		fmt.Printf("pkgcache: reclaimed %s\n", local.FormatSize(reclaimed))
	} else {
		fmt.Println("pkgcache: nothing to reclaim")
	}
	if stopped {
		fmt.Println("pkgcache: the daemon was stopped; the next command starts it again")
	}
	return nil
}

func directorySizeOrZero(root string) int64 {
	size, err := directorySize(root)
	if err != nil {
		return 0
	}
	return size
}
