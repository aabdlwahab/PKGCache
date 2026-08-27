package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/local"
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
	project := local.CurrentProject(snap.DataDir)
	if local.HasCurrentProject(snap.DataDir) {
		fmt.Printf("project    %s\n", project)
	} else {
		fmt.Printf("project    %s (the default; pkgcache project use <name> to change it)\n", project)
	}
	if errors.Is(err, local.ErrNoDaemon) {
		fmt.Printf("daemon     not running\n")
		fmt.Printf("address    %s (when started)\n", snap.LocalBaseURL())
		return reportBudget(ctx, snap.DataDir, snap.BlobRoot(), project)
	}
	fmt.Printf("daemon     running, pid %d, up %s\n",
		state.PID, state.Uptime().Round(time.Second))
	fmt.Printf("address    %s\n", state.BaseURL())
	fmt.Printf("version    %s\n", state.Version)
	if err := reportBudget(ctx, snap.DataDir, snap.BlobRoot(), project); err != nil {
		// The per-project table is still worth printing under a full cache, which is
		// exactly when somebody wants to know which project is holding what — so it goes
		// out before the exit status this returns.
		reportProjects(ctx, state)
		return err
	}
	reportProjects(ctx, state)
	return nil
}

// reportProjects prints what each project holds and how much of it was served from here.
//
// Only with a daemon running: the figures come from the catalog, the daemon owns it, and
// `status` starting one to answer a question about whether one is running would be its
// own kind of wrong. Failures are printed, not returned — a status report that dies on
// its last section is worse than one that says a section is missing.
func reportProjects(ctx context.Context, state local.State) {
	reports, err := local.ProjectReports(ctx, state)
	if err != nil {
		fmt.Printf("projects   unavailable: %v\n", err)
		return
	}
	if len(reports) == 0 {
		fmt.Println("projects   nothing cached yet")
		return
	}
	label := "projects  "
	for _, report := range reports {
		served := "     —"
		if rate, known := report.Served(); known {
			served = fmt.Sprintf("%5.0f%%", rate*100)
		}
		fmt.Printf("%s %-16s %9s  %6d objects  %s served from here\n",
			label, report.Project, local.FormatSize(report.Bytes), report.Objects, served)
		label = "          "
	}
}

// reportBudget prints what the cache holds against what it is allowed to hold, and is
// the second of the four channels: a non-zero status while the cache is full, from a
// command whose whole job is to answer "is this healthy?".
func reportBudget(ctx context.Context, dataDir, blobRoot, project string) error {
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
	if err := reportTiers(ctx, dataDir, project); err != nil {
		return err
	}
	if record, found := local.ReadPersisted(dataDir); found {
		// Which project the persisted files name, because they outlive the shell that
		// wrote them and a `project use` afterwards does not move them.
		fmt.Printf("persisted  %s, %d files (%s)\n",
			record.Project, len(record.Files), record.BaseURL)
	}
	usage, sampled, found := local.ReadUsage(dataDir)
	if found && usage.Full {
		fmt.Printf("\nCACHE FULL — %s\n", usage.Reason)
		fmt.Printf("(measured %s ago)\n", time.Since(sampled).Round(time.Second))
		return &exitError{code: local.FullExitCode}
	}
	return nil
}

// reportTiers says where a miss goes next.
//
// Tiering is only worth having if it is visible: a team cache that has been down for a
// week is otherwise just "builds got slower", noticed by nobody.
func reportTiers(ctx context.Context, dataDir, project string) error {
	team, has, err := local.ReadTeam(dataDir, project)
	if err != nil {
		return err
	}
	if !has {
		fmt.Println("team       none")
		fmt.Println("direct     always")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	reachable := local.ReachableTeam(ctx, dataDir, team)
	if reachable {
		fmt.Printf("team       %s  reachable, %s\n",
			team.Server, time.Since(started).Round(time.Millisecond))
	} else {
		fmt.Printf("team       %s  UNREACHABLE\n", team.Server)
	}
	if team.Direct {
		fmt.Println("direct     when the team cache is unreachable")
	} else {
		fmt.Println("direct     never — the team cache or nothing")
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
