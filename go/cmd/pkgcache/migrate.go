package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

func runMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	keep := fs.Bool("keep", false,
		"leave the old copy in place instead of removing it once the new one is verified")
	dryRun := fs.Bool("dry-run", false, "say what would happen, and change nothing")
	fromWindow := fs.Bool("from-window", false,
		"leave the outcome where the window that asked for this move reads it, and start "+
			"the cache again afterwards")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache migrate — move this cache to another disk

usage: pkgcache migrate <directory> [flags]

Moves everything the cache holds — blobs, catalog, projects, checkpoints, your limit —
to another directory, which is usually on another partition because the one it is on is
full. What is left behind is a note saying where it went, so every later pkgcache finds
it without anything being reconfigured: this shell, the socket unit, the tray app and
`+"`docker build`"+` all read it.

It stops the cache first, verifies the copy arrived whole before removing the old one,
and refuses a destination that cannot hold a cache — one without hardlinks, which is
exFAT, FAT32 and most network shares, or one without the space.

  pkgcache migrate /mnt/big/pkgcache
  pkgcache migrate -dry-run /mnt/big/pkgcache    what it would do, and whether it fits

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Flags before the directory, for the reason projectFlags gives: Go's flag package
	// stops at the first non-flag, so `migrate /mnt/x -keep` would silently not keep.
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return fmt.Errorf("migrate: where to? `pkgcache migrate <directory>`")
	case len(rest) > 1:
		return fmt.Errorf(
			"migrate: unexpected argument %q; flags come before the directory", rest[1])
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	to, err := filepath.Abs(rest[0])
	if err != nil {
		return err
	}

	// Where the signpost belongs, and whether one is any use. It is the default
	// location for this user — not the cache's current one, which on a second move is
	// already somewhere the signpost points at.
	fixed := strings.TrimSpace(os.Getenv(config.LocalEnvPrefix + "DATA_DIR"))
	signpost := ""
	if fixed == "" {
		base, err := config.LocalDefaultDataDir()
		if err != nil {
			return err
		}
		if base != to {
			signpost = base
		}
	}

	if *dryRun {
		_, err := local.Migrate(ctx, local.MigrateOptions{
			From: snap.DataDir, To: to, Keep: *keep, DryRun: true, Signpost: signpost,
			Out: os.Stdout,
		})
		return err
	}

	started := time.Now()
	// Socket activation before the daemon, not after. A window that asked for this move
	// polls every couple of seconds to learn when it is over, and between stopping the
	// daemon and stopping the socket that poll would wake a new daemon on the very
	// directory about to be moved.
	suspended, err := local.SuspendService() //nolint:contextcheck // runSystemctl and runLaunchctl bound themselves; see serviceManagerTimeout
	var result local.MigrateResult
	if err == nil {
		err = stopCache(ctx, snap.DataDir)
	}
	if err == nil {
		result, err = local.Migrate(ctx, local.MigrateOptions{
			From:     snap.DataDir,
			To:       to,
			Keep:     *keep,
			Signpost: signpost,
			Out:      os.Stdout,
		})
	}

	// Wherever the cache is now: the new place if it got there, the old one if not. A
	// failure leaves the machine pointed at the cache it still has.
	home := snap.DataDir
	if err == nil {
		home = result.To
	}
	if suspended {
		restoreService(home) //nolint:contextcheck // as above
	}
	if *fromWindow {
		if recordErr := local.FinishMigration(snap.DataDir, to, started, result, err); recordErr != nil {
			fmt.Fprintf(os.Stderr, "pkgcache: the window will not hear how this ended: %v\n", recordErr)
		}
		// Socket activation brings the cache back on the window's next request. Without
		// it nothing would, and the window would wait on an address nobody answers.
		if !suspended {
			restartCache(ctx, snap, home)
		}
	}
	if err != nil {
		return err
	}
	reportMigration(result, fixed)
	return nil
}

// stopCache stops the daemon, if there is one, and says so.
func stopCache(ctx context.Context, dataDir string) error {
	stopped, err := local.Stop(ctx, dataDir, 30*time.Second)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Println("pkgcache: stopped the cache")
	}
	return nil
}

// restartCache starts the daemon again on the address the window was using.
func restartCache(ctx context.Context, snap *config.Snapshot, dataDir string) {
	restarted, err := config.LoadLocal(config.LocalFlags{DataDir: dataDir, Addr: snap.LocalAddr()})
	if err == nil {
		_, err = local.Ensure(ctx, local.EnsureOptions{Snapshot: restarted, Notes: os.Stdout})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache: the cache was not started again: %v\n", err)
	}
}

// restoreService puts socket activation back, naming the cache's directory.
//
// Run whether or not the move worked: the unit was stopped by this command, and a machine
// left without the activation it had is a machine whose persisted .npmrc names a port
// nothing answers on any more.
func restoreService(dataDir string) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache: socket activation was not reinstalled: %v\n", err)
		return
	}
	if _, err := local.InstallService(executable, dataDir, false, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr,
			"pkgcache: socket activation was not reinstalled: %v\n"+
				"  put it back with:  pkgcache persist\n", err)
	}
}

// reportMigration says what happened and, where the answer is not "everything now finds
// it", what is left for the person to do.
func reportMigration(result local.MigrateResult, fixed string) {
	fmt.Printf("pkgcache: the cache is now %s\n", result.To)
	fmt.Printf("          %s in %d files\n",
		local.FormatSize(result.Bytes), result.Files)
	if result.Kept {
		fmt.Printf("          the old copy is still at %s; remove it to reclaim the space\n",
			result.From)
	}
	switch {
	case result.Signpost != "":
		fmt.Printf("          %s says where it went, so nothing needs reconfiguring\n",
			result.Signpost)
	case fixed != "":
		// An environment variable is the one thing this command cannot fix from here:
		// it is set in a file this process does not own and cannot know the name of.
		fmt.Printf("\n%sDATA_DIR is set to %s, and it wins over everything else.\n",
			config.LocalEnvPrefix, fixed)
		fmt.Printf("Point it at %s wherever it is set, or unset it.\n", result.To)
	default:
		fmt.Println("          this is the default location, so nothing needs reconfiguring")
	}
}
