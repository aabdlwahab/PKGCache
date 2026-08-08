package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/brightskies/pkgreg/internal/clientbuild"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/local"
)

// build and compose are the same commands pkgreg-client has, with the same flags and
// the same promise: the Dockerfile in the repository never mentions a cache, the
// rewrite happens in memory, and everything this does not recognise goes to docker
// untouched.
//
// What differs is only where the cache is. pkgreg-client points a build at a bridge or
// at a server's HTTPS address; pkgcache points it at a cache on this machine, over
// loopback where the daemon shares it and through host.docker.internal where it does
// not.

func runBuild(ctx context.Context, args []string) error {
	return dockerCommand(ctx, args, false)
}

func runCompose(ctx context.Context, args []string) error {
	return dockerCommand(ctx, args, true)
}

func dockerCommand(ctx context.Context, argv []string, compose bool) error {
	name := "build"
	if compose {
		name = "compose"
	}
	fs := flag.NewFlagSet("pkgcache "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	options := clientbuild.Options{}
	project := fs.String("project", "global", "project to scope URLs to")
	fs.BoolVar(&options.Print, "print", false,
		"write the rewritten Dockerfile or Compose file and build nothing")
	fs.BoolVar(&options.HostAddress, "host-address", false,
		"reach the cache through "+clientbuild.DefaultHostGateway+" instead of loopback, "+
			"for a daemon that cannot see this terminal's network: Docker Desktop, a "+
			"remote daemon, CI")
	// pkgreg-client spells this -cache-address. Both work, because every existing
	// instruction, Makefile and CI job says the old one, and a merge that silently
	// broke them would be a migration rather than a merge.
	cacheAddress := fs.Bool("cache-address", false,
		"deprecated alias for -host-address")
	fs.BoolVar(&options.SkipFrom, "keep-images", false,
		"leave image names alone, for a daemon that already resolves them through the cache")
	gitHosts := fs.String("git-host", strings.Join(defaultGitHosts, ","),
		"comma-separated hosts whose clones are served from the cache")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `pkgcache %s — run docker %s through the cache

usage:
  pkgcache %s [flags] [docker %s arguments]

Starts the cache if it is not running, then runs docker against a Dockerfile rewritten
in memory. Your Dockerfile and Compose file are never modified, and the generated file
is written outside the build context so a COPY cannot pick it up.

Everything this does not recognise is passed to docker untouched.

On Docker Desktop, a remote daemon or in CI, add -host-address: the daemon does not
share this terminal's loopback. That needs `+"`pkgcache docker-setup`"+` once, so the
daemon accepts a plain-HTTP registry at this machine's address.

flags:
`, name, name, name, name)
		fs.PrintDefaults()
	}

	ours, theirs := splitKnownFlags(fs, argv)
	if err := fs.Parse(ours); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *cacheAddress {
		fmt.Fprintln(os.Stderr,
			"pkgcache: -cache-address is now -host-address; the old name still works")
		options.HostAddress = true
	}

	// The cache has to be running before its address can be put into a build.
	snap, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return err
	}
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
	if err != nil {
		return err
	}
	options.Bridge = state.BaseURL()
	options.Registry = state.Addr
	options.Project = *project
	options.AptProxy = state.BaseURL()
	options.GitHosts = splitList(*gitHosts)
	options = clientbuild.FromEnvironment(options)

	arguments := append(fs.Args(), theirs...)
	if compose {
		err = clientbuild.Compose(ctx, options, arguments)
	} else {
		err = clientbuild.Build(ctx, options, arguments)
	}
	if err != nil {
		return err
	}
	if full := reportFull(snap.DataDir); full != nil {
		return full
	}
	return nil
}

// splitKnownFlags divides our flags from docker's at the first argument we do not own.
//
// Go's flag package stops at the first non-flag, which would silently hand `-t app:dev`
// to us as a positional argument and then complain about it. Same rule as
// pkgreg-client's, deliberately: a wrapper that diverges here stops being a wrapper.
func splitKnownFlags(fs *flag.FlagSet, argv []string) (ours, theirs []string) {
	known := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = true })
	for i := range argv {
		argument := argv[i]
		trimmed := strings.TrimLeft(argument, "-")
		base, _, _ := strings.Cut(trimmed, "=")
		if strings.HasPrefix(argument, "-") && known[base] {
			ours = append(ours, argument)
			continue
		}
		return ours, argv[i:]
	}
	return ours, nil
}
