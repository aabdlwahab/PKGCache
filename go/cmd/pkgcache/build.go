package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/clientbuild"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// build and compose are the commands pkgreg-client had, with the same flags and the
// same promise: the Dockerfile in the repository never mentions a cache, the rewrite
// happens in memory, and everything this does not recognise goes to docker untouched.
//
// What differs is only where the cache is. pkgreg-client pointed a build at a bridge or
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
	project := fs.String("project", "",
		"project to scope URLs to (default: the current one, from pkgcache project use)")
	fs.BoolVar(&options.Print, "print", false,
		"write the rewritten Dockerfile or Compose file and build nothing")
	fs.BoolVar(&options.HostAddress, "host-address", false,
		"reach the cache through "+clientbuild.DefaultHostGateway+" instead of loopback "+
			"(default: whichever this Docker daemon can actually reach)")
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
		_, _ = fmt.Fprintf(fs.Output(), `pkgcache %s — run docker %s through the cache

usage:
  pkgcache %s [flags] [docker %s arguments]

Starts the cache if it is not running, then runs docker against a Dockerfile rewritten
in memory. Your Dockerfile and Compose file are never modified, and the rewrite is fed
to docker on stdin rather than written out — so a COPY cannot pick it up, and it works
with a Docker client that cannot read this process's files, such as the snap.

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
	// Derived unless somebody said otherwise; see the same decision in pull.go.
	autoGateway := !options.HostAddress && !flagGiven(fs, "host-address")
	if autoGateway {
		options.HostAddress = clientbuild.GatewayDefault(ctx, "")
	}

	// The cache has to be running before its address can be put into a build.
	snap, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return err
	}
	state, err := ensureCaching(ctx, snap, "pkgcache "+name, "for the build to reach")
	if err != nil {
		return err
	}
	options.Bridge = state.BaseURL()
	options.Registry = state.Addr
	options.Project = *project
	if options.Project == "" {
		// Resolved here rather than left to FromEnvironment, which knows the
		// environment but not the stored choice.
		options.Project = local.CurrentProject(snap.DataDir)
	}
	options.AptProxy = state.BaseURL()
	if autoGateway && options.HostAddress {
		fmt.Fprintln(os.Stderr, clientbuild.GatewayNote(clientbuild.GatewayAuthority(state.Addr)))
	}
	// A base image that already exists here is not rewritten: it may be one this build
	// or a previous one produced, with no upstream to be fetched from.
	options.LocalImage = clientbuild.DefaultLocalImage(ctx, "")
	// Indexes this cache serves, so a Dockerfile naming one directly — an
	// --extra-index-url for a CUDA torch build, say — is served from here too.
	options.Indexes = clientbuild.DiscoverIndexes(ctx, state.BaseURL(), options.Project)
	options.GitHosts = splitList(*gitHosts)
	options = clientbuild.FromEnvironment(options)

	arguments := append(fs.Args(), theirs...)
	release := local.HoldDaemon(ctx, state.BaseURL())
	if compose {
		err = clientbuild.Compose(ctx, options, arguments)
	} else {
		err = clientbuild.Build(ctx, options, arguments)
	}
	release()
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
//
// Two things the rule has to know beyond the names. -h and -help are flag's own, never
// registered, so without naming them here `pkgcache build -h` went to docker — after the
// daemon had been started for a build that was never going to happen. And a flag that
// takes a value owns the next argument when it is not written with =: `-project web`
// split after -project handed `web` to docker as the build context.
func splitKnownFlags(fs *flag.FlagSet, argv []string) (ours, theirs []string) {
	known := map[string]*flag.Flag{}
	fs.VisitAll(func(f *flag.Flag) { known[f.Name] = f })
	for i := 0; i < len(argv); i++ {
		argument := argv[i]
		if !strings.HasPrefix(argument, "-") {
			return ours, argv[i:]
		}
		trimmed := strings.TrimLeft(argument, "-")
		base, _, inline := strings.Cut(trimmed, "=")
		if base == "h" || base == "help" {
			ours = append(ours, argument)
			continue
		}
		defined := known[base]
		if defined == nil {
			return ours, argv[i:]
		}
		ours = append(ours, argument)
		if !inline && !isBoolFlag(defined) && i+1 < len(argv) {
			i++
			ours = append(ours, argv[i])
		}
	}
	return ours, nil
}

// isBoolFlag is how flag itself decides whether a flag may stand alone.
func isBoolFlag(f *flag.Flag) bool {
	boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}
