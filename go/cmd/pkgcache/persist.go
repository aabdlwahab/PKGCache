package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// installService is local.InstallService, held in a variable so a test can prove a dry run
// never reaches it: the real one registers a launchd agent or a systemd unit in the
// session of whoever runs the test.
var installService = local.InstallService

func runPersist(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("persist", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	project := fs.String("project", "",
		"project to scope URLs to (default: the current one, from pkgcache project use)")
	gitHosts := fs.String("git-host", strings.Join(defaultGitHosts, ","),
		"comma-separated hosts whose clones are served from the cache")
	noGit := fs.Bool("no-git", false, "leave git configuration alone")
	showOnly := fs.Bool("print", false, "print the settings instead of installing them")
	dryRun := fs.Bool("dry-run", false, "print every change without applying it")
	uninstall := fs.Bool("uninstall", false, "reverse a previous run")
	anyway := fs.Bool("anyway", false,
		"install even though the cache's address can stop answering when it goes idle")
	noService := fs.Bool("no-service", false,
		"do not install socket activation; settings only")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache persist — settings that outlive the session

usage: pkgcache persist [flags]

Writes user-level settings so that pip, uv, npm and git use the cache without being
wrapped in `+"`pkgcache run`"+`. That reaches what a wrapper cannot: a Makefile, an IDE,
a colleague's script, a terminal opened before anybody thought about caching.

Everything is written under your home, fenced by markers, and removed exactly by
-uninstall. No root, no machine-wide trust store, nothing under /etc.

It also installs socket activation, because settings that outlive a process are the one
way this can leave a machine worse than it found it: a .npmrc naming a port nothing is
listening on fails npm rather than slowing it. With activation, the port is held open
by systemd and the cache still exits when idle.

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

	availability := local.AvailabilityUnknown
	if *anyway {
		availability = local.AvailabilityAccepted
	}
	switch {
	case *noService || *showOnly:
	case *dryRun:
		// -dry-run promises to apply nothing, and the service is the one change here that
		// reaches outside the home directory — a unit registered with launchd or systemd.
		// It used to be installed regardless, so the preview made the change it described.
		// Said instead, and assumed for the rest of the preview, since the real run would
		// have installed it before writing a single setting.
		verb := "install"
		if *uninstall {
			verb = "remove"
		}
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			fmt.Printf("would %s socket activation for %s\n", verb, snap.DataDir)
			if availability == local.AvailabilityUnknown {
				availability = local.AvailabilitySocket
			}
		} else {
			fmt.Printf("no socket activation on %s; the real run would need -anyway\n", runtime.GOOS)
		}
	default:
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		got, err := installService(executable, snap.DataDir, *uninstall, os.Stdout) //nolint:contextcheck // runSystemctl and runLaunchctl bound themselves; see serviceManagerTimeout
		if err != nil {
			if !*anyway {
				return fmt.Errorf("%w\n  or pass -anyway to install the settings regardless", err)
			}
			fmt.Fprintf(os.Stderr, "pkgcache: %v\n", err)
		} else if got != local.AvailabilityUnknown {
			availability = got
		}
	}

	hosts := splitList(*gitHosts)
	if *noGit {
		hosts = nil
	}
	// These files outlive the shell that wrote them, so the project is resolved now and
	// recorded literally. A persisted .npmrc that followed a later `project use` would
	// silently redirect an IDE nobody has reopened.
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	// The address the settings name is the configured one, not a running daemon's:
	// these files outlive every daemon, and a fixed port is the whole reason the
	// activation socket binds one.
	return local.ApplyPersist(local.PersistOptions{
		BaseURL:   snap.LocalBaseURL(),
		Project:   scope,
		DataDir:   snap.DataDir,
		GitHosts:  hosts,
		DryRun:    *dryRun,
		Uninstall: *uninstall,
		Print:     *showOnly,
		Available: availability,
		Out:       os.Stdout,
	})
}
