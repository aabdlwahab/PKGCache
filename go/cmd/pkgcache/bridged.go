package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// ensureCaching starts this machine's cache for a command that can only work against
// one, and refuses on a machine set up with -no-cache.
//
// local.Ensure alone does not know about -no-cache, so every command that called it
// directly broke the mode's promise twice over: it created the store and the database
// that -no-cache says will never exist, and the daemon it started had no chain to the
// team cache, so pull, build and warmlock went straight to the public registries from a
// machine whose owner had chosen to reach nothing but the team. Refusing names what to
// do instead; run and shell already reach the team through a verified bridge.
func ensureCaching(ctx context.Context, snap *config.Snapshot, command, needs string) (local.State, error) {
	set, err := local.ReadTeams(snap.DataDir)
	if err != nil {
		return local.State{}, err
	}
	if team, bridged := set.Bridged(); bridged {
		return local.State{}, fmt.Errorf(
			"%s needs a cache on this machine %s, and this one was set up with -no-cache:\n"+
				"  it stores nothing and reaches %s through a bridge that lives only as long as\n"+
				"  `pkgcache shell` or `pkgcache run` does. Use those for package tools, or cache\n"+
				"  here as well with `pkgcache setup -server %s -ca-sha256 … -team-project %s -limit 25G`",
			command, needs, team.Server, team.Server, team.Project)
	}
	return local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
}
