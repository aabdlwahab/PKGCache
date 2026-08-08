package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/local"
	"github.com/brightskies/pkgreg/internal/trust"
)

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	server := fs.String("server", "", "team cache origin, e.g. https://cache.internal:8443")
	fingerprint := fs.String("ca-sha256", "",
		"the team cache's CA fingerprint, given to you out of band (colons optional)")
	caFile := fs.String("ca-file", "", "trusted CA file; also supplies the expected fingerprint")
	project := fs.String("project", "global", "project to use on the team cache")
	limit := fs.String("limit", "", "cache size budget, e.g. 25G, or none")
	minFree := fs.String("min-free", "", "free-disk floor to keep underneath the limit")
	noDirect := fs.Bool("no-direct", false,
		"never reach a public registry: use the team cache or fail")
	uninstall := fs.Bool("uninstall", false, "forget the team cache and its chain")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache setup — point this machine at a cache, once

usage:
  pkgcache setup -limit 25G
  pkgcache setup -server https://cache.internal:8443 -ca-sha256 AB:CD:… -limit 25G

With a team cache, lookups go local, then the team, then the registries. The team's CA
is fetched over an unverified connection and refused unless it matches the fingerprint
you were given separately; from then on it is verified normally.

Falling back to a registry when the team cache is unreachable is what -no-direct turns
off, for a machine that must never fetch from the internet itself.

Nothing is written outside this cache's own directory.

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

	// Chain configuration opens the store, and the store has one writer.
	if _, err := local.Stop(ctx, snap.DataDir, 30*time.Second); err != nil {
		return err
	}

	if *limit != "" {
		if err := applyLimit(snap.DataDir, *limit, *minFree); err != nil {
			return err
		}
	}

	if *uninstall {
		local.ClearTeam(snap.DataDir)
		if err := local.ConfigureChains(ctx, snap, local.Team{}, false); err != nil {
			return err
		}
		fmt.Println("pkgcache: the team cache is no longer configured")
		return describe(snap)
	}

	if *server != "" {
		verified, err := trust.Fetch(ctx, trust.Options{
			Server: *server, ExpectedSHA256: *fingerprint, CAFile: *caFile,
		})
		if err != nil {
			return err
		}
		if err := os.WriteFile(
			local.TeamCAPath(snap.DataDir), verified.CAPEM, 0o600); err != nil {
			return err
		}
		team := local.Team{
			Server:      verified.Base.String(),
			Fingerprint: verified.Fingerprint,
			Project:     *project,
			Direct:      !*noDirect,
		}
		if err := local.WriteTeam(snap.DataDir, team); err != nil {
			return err
		}
		if err := local.ConfigureChains(ctx, snap, team, true); err != nil {
			return err
		}
		fmt.Printf("pkgcache: verified %s\n  fingerprint %s\n",
			team.Server, team.Fingerprint)
	} else if *noDirect {
		return errors.New("-no-direct only means something with a team cache; pass -server")
	}

	return describe(snap)
}

// applyLimit is `pkgcache limit` reached from setup, so one command can do the whole
// installation.
func applyLimit(dataDir, limit, minFree string) error {
	bytes, err := local.ParseSize(limit)
	if err != nil {
		return err
	}
	current, _ := local.ReadBudget(dataDir)
	budget := local.Budget{LimitBytes: bytes, MinFreeBytes: current.MinFreeBytes}
	if minFree != "" {
		floor, err := local.ParseSize(minFree)
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
	if err := local.WriteBudget(dataDir, budget); err != nil {
		return err
	}
	local.ClearUsage(dataDir)
	return nil
}

// describe prints what this machine will now do, which is the only way somebody can
// check that setup did what they meant.
func describe(snap *config.Snapshot) error {
	fmt.Println()
	budget, err := local.ReadBudget(snap.DataDir)
	switch {
	case errors.Is(err, local.ErrNoLimit):
		fmt.Println("local      no limit set — pkgcache will not serve until there is one")
		fmt.Println("           pkgcache setup -limit 25G")
	case err != nil:
		return err
	case budget.LimitBytes == local.NoLimit:
		fmt.Println("local      no size limit")
	default:
		fmt.Printf("local      up to %s\n", local.FormatSize(budget.LimitBytes))
	}

	team, has, err := local.ReadTeam(snap.DataDir)
	if err != nil {
		return err
	}
	if !has {
		fmt.Println("team       none")
		fmt.Println("direct     always")
		return nil
	}
	fmt.Printf("team       %s (project %s)\n", team.Server, team.Project)
	if team.Direct {
		fmt.Println("direct     when the team cache is unreachable")
	} else {
		fmt.Println("direct     never — the team cache or nothing")
	}
	return nil
}
