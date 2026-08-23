package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/trust"
)

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	server := fs.String("server", "", "team cache origin, e.g. https://cache.internal:8443")
	fingerprint := fs.String("ca-sha256", "",
		"the team cache's CA fingerprint, given to you out of band (colons optional)")
	caFile := fs.String("ca-file", "", "trusted CA file; also supplies the expected fingerprint")
	project := fs.String("project", "",
		"the local project this applies to (default: the current one)")
	teamProject := fs.String("team-project", config.GlobalProject,
		"the project name on the team's side")
	limit := fs.String("limit", "", "cache size budget, e.g. 25G, or none")
	minFree := fs.String("min-free", "", "free-disk floor to keep underneath the limit")
	noDirect := fs.Bool("no-direct", false,
		"never reach a public registry: use the team cache or fail")
	noCache := fs.Bool("no-cache", false,
		"do not cache locally; use the team cache through a verified loopback bridge")
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

Configuration is per project. -project chooses which of this machine's projects it
applies to, and the global project's is the fallback for every project without its own.
-team-project is the name on the team's side, which defaults to their global project
because assuming a name exists on somebody else's server is not this program's call.

Nothing is written outside this cache's own directory.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	if *noCache && scope != config.GlobalProject {
		return fmt.Errorf(
			"-no-cache is a promise about this machine — no store, no database — so it\n"+
				"  cannot hold for one project and not another. Drop -project %s", scope)
	}

	// Chain configuration opens the store, and the store has one writer. The daemon has
	// to go anyway: the CA bundle it trusts is read when it starts.
	if _, err := local.Stop(ctx, snap.DataDir, 30*time.Second); err != nil {
		return err
	}

	if *limit != "" {
		if err := applyLimit(snap.DataDir, *limit, *minFree); err != nil {
			return err
		}
	}

	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}

	switch {
	case *uninstall && given["project"]:
		set.Remove(scope)
		if err := local.WriteTeams(snap.DataDir, set); err != nil {
			return err
		}
		if err := applyChains(ctx, snap, set); err != nil {
			return err
		}
		fmt.Printf("pkgcache: %s no longer has a team cache of its own\n", scope)
		return describe(snap)

	case *uninstall:
		local.ClearTeam(snap.DataDir)
		if err := applyChains(ctx, snap, local.TeamSet{}); err != nil {
			return err
		}
		fmt.Println("pkgcache: the team cache is no longer configured")
		return describe(snap)

	case *server != "":
		verified, err := trust.Fetch(ctx, trust.Options{
			Server: *server, ExpectedSHA256: *fingerprint, CAFile: *caFile,
		})
		if err != nil {
			return err
		}
		set.Set(scope, local.Team{
			Server:      verified.Base.String(),
			Fingerprint: verified.Fingerprint,
			Project:     *teamProject,
			Direct:      !*noDirect,
			NoCache:     *noCache,
			CAPEM:       string(verified.CAPEM),
		})
		if err := local.WriteTeams(snap.DataDir, set); err != nil {
			return err
		}
		// No chain to configure when nothing is cached here: with -no-cache this
		// machine is a bridge, and opening the store to write upstream rows would
		// create the databases the mode promises not to.
		if !*noCache {
			if err := applyChains(ctx, snap, set); err != nil {
				return err
			}
		}
		fmt.Printf("pkgcache: verified %s\n  fingerprint %s\n",
			verified.Base.String(), verified.Fingerprint)

	case *noDirect || *noCache:
		return errors.New(
			"-no-direct and -no-cache only mean something with a team cache; pass -server")
	}

	return describe(snap)
}

// applyChains writes every project's chain and says what it could not apply.
//
// A team cache configured for a project that does not exist here is inert until the
// project is created. Reported rather than refused, because refusing would make the
// order of two commands matter for no reason.
func applyChains(ctx context.Context, snap *config.Snapshot, set local.TeamSet) error {
	unknown, err := local.ConfigureChains(ctx, snap, set)
	if err != nil {
		return err
	}
	for _, name := range unknown {
		fmt.Fprintf(os.Stderr,
			"pkgcache: there is no project named %q on this cache, so its team cache is not\n"+
				"  in use yet. `pkgcache project create %s`, or check the spelling.\n", name, name)
	}
	return nil
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
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}
	if team, bridged := set.Bridged(); bridged {
		fmt.Println("local      disabled")
		fmt.Printf("team       %s (project %s)\n", team.Server, team.Project)
		fmt.Println("direct     never — this machine caches nothing itself")
		return nil
	}
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

	current := local.CurrentProject(snap.DataDir)
	fmt.Printf("project    %s\n", current)
	describeTier(set, current)

	// Every other project, so a machine with two chains shows both. Printed only when
	// there is more than the one just described: a single-project laptop should not
	// have to read a table.
	others := sortedProjects(set, current)
	if len(others) > 0 {
		fmt.Println()
		for _, project := range others {
			team, _ := set.Own(project)
			fmt.Printf("%-10s %s (project %s)\n", project, team.Server, team.Project)
		}
	}
	return nil
}

// describeTier says where a miss from this project goes next, and whether that was
// chosen for it or inherited.
func describeTier(set local.TeamSet, project string) {
	team, has := set.For(project)
	if !has {
		fmt.Println("team       none")
		fmt.Println("direct     always")
		return
	}
	inherited := ""
	if _, own := set.Own(project); !own {
		inherited = fmt.Sprintf(" — inherited from %s", config.GlobalProject)
	}
	fmt.Printf("team       %s (project %s)%s\n", team.Server, team.Project, inherited)
	if team.Direct {
		fmt.Println("direct     when the team cache is unreachable")
	} else {
		fmt.Println("direct     never — the team cache or nothing")
	}
}

// sortedProjects returns the projects with a team cache of their own, except the one
// already described.
func sortedProjects(set local.TeamSet, except string) []string {
	var names []string
	for name, team := range set.Projects {
		if name == except || team.Server == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
