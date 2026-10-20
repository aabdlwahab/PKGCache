package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aabdlwahab/PKGCache/internal/clientbuild"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

func runPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	team := fs.Bool("team", false,
		"also send the image to the team cache this machine is pointed at")
	as := fs.String("as", "", "push as this name[:tag] under local/ instead of the image's own")
	hostAddress := fs.Bool("host-address", false,
		"reach the cache through "+clientbuild.DefaultHostGateway+" instead of loopback "+
			"(default: whichever this Docker daemon can actually reach)")
	keep := fs.Bool("keep-cache-tag", false,
		"leave the cache-addressed tag in place as well as the original name")
	dryRun := fs.Bool("print", false, "print where each image would go, and push nothing")
	project := fs.String("project", "",
		"push into this project instead of the one this cache is working in")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache push — put an image built here into the cache

usage: pkgcache push [flags] <image>...

Pushes each image into this machine's cache, under local/: `+"`pkgcache push mold:latest`"+`
leaves it at local/mold:latest, which any build or pull through this cache can name.
With -team it then goes on to the team cache this machine is pointed at, so every other
machine pointed at that team can pull it too:

  pkgcache push -team mold:latest
  pkgcache pull local/mold:latest          (on any machine behind the same team)

Docker only ever talks to this cache, on loopback: the team needs no login in Docker and
no certificate in Docker's trust store. The image is sent on with this machine's team
token, which needs scope write. Create one in the team console under Connect, Access
tokens, and hand it to `+"`pkgcache setup ... -token-file -`"+`.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return &exitError{code: 2}
	}
	if *as != "" && fs.NArg() > 1 {
		return fmt.Errorf("-as names one image, and %d were given", fs.NArg())
	}

	snap, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return err
	}
	scope := *project
	if scope == "" {
		scope = local.CurrentProject(snap.DataDir)
	}
	// Checked before anything is pushed, so a machine with no team or no token learns it
	// now rather than after its image has gone halfway.
	var teamRecord local.Team
	if *team {
		record, found, err := local.ReadTeam(snap.DataDir, scope)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("project %s is not pointed at a team cache, so there is nowhere to "+
				"send the image; run pkgcache setup -server first", scope)
		}
		if err := local.CheckTeamPush(record); err != nil {
			return err
		}
		teamRecord = record
	}

	// Pushed images live in the cache's store, so it has to be running and caching.
	state, err := ensureCaching(ctx, snap, "pkgcache push", "for Docker to push to")
	if err != nil {
		return err
	}
	gateway := *hostAddress
	if !flagGiven(fs, "host-address") {
		gateway = clientbuild.GatewayDefault(ctx, "")
	}
	registry := state.Addr
	if gateway {
		registry = clientbuild.GatewayAuthority(state.Addr)
		if registry == "" {
			return fmt.Errorf("push: cannot read a port from %q", state.Addr)
		}
		if !flagGiven(fs, "host-address") {
			fmt.Fprintln(os.Stderr, clientbuild.GatewayNote(registry))
		}
	}

	for _, image := range fs.Args() {
		if *dryRun {
			repository, tag, err := clientbuild.PushName(image, *as)
			if err != nil {
				return err
			}
			fmt.Printf("%s\n  -> %s/%s%s/%s:%s\n", image, registry,
				router.OCIProjectPrefix(scope), ociname.Hosted, repository, tag)
			if *team {
				fmt.Printf("  -> %s, project %s\n", teamRecord.Server, teamProjectName(teamRecord))
			}
			continue
		}
		pushed, err := clientbuild.Push(ctx, image, clientbuild.PushOptions{
			Registry: registry, Project: scope, Name: *as, Keep: *keep, Notes: os.Stderr,
		})
		if err != nil {
			return err
		}
		fmt.Printf("pkgcache: %s is in this cache as %s/%s\n",
			image, ociname.Hosted, pushed.Repository+":"+pushed.Tag)
		if !*team {
			continue
		}
		sent, err := local.PushToTeam(ctx, local.TeamPush{
			Source: state.Addr, Project: scope,
			Repository: pushed.Repository, Tag: pushed.Tag,
			Team: teamRecord, Notes: os.Stderr,
		})
		if err != nil {
			return err
		}
		fmt.Printf("pkgcache: and on the team as %s (%d of %d layers sent, the rest it had)\n",
			sent.Target, sent.Sent, sent.Layers)
	}
	return nil
}

// teamProjectName is the team-side project a record names, the empty name being global.
func teamProjectName(team local.Team) string {
	if team.Project == "" {
		return config.GlobalProject
	}
	return team.Project
}
