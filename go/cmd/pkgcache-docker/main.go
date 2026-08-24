// Command pkgcache-docker is docker, with build and pull served from this machine's cache.
//
// It exists for tools that shell out to a container command and let you choose which one.
// crate is the one this was written for — `crate prepare --runtime pkgcache-docker` — but
// nothing here knows about crate, and anything that runs `<command> build` and
// `<command> pull` gets the same benefit.
//
// The alternative was to teach those tools about pkgcache, and it was the wrong shape.
// An orchestrator that knows about a specific cache cannot run where the cache is not
// installed, has to be changed whenever the cache's flags change, and is two products on
// two release cycles pretending to be one. A drop-in container command inverts that: the
// tool learns nothing, the knowledge lives here, and it is versioned with the cache whose
// behaviour it depends on.
//
// Three verbs, and only two of them are interesting:
//
//	build   the Dockerfile is rewritten in memory — pip, uv, npm and git come from the
//	        cache, and apt and apk come through its proxy. Exactly `pkgcache build`.
//	pull    fetched through the cache and tagged back to the name that was asked for,
//	        so the image is named what the manifest calls it. Exactly `pkgcache pull`.
//	*       handed to docker untouched. run, ps, save, load, image inspect, compose —
//	        everything else is not this program's business and is not slowed down by it.
//
// On Docker Desktop, WSL, or any daemon that cannot see this terminal's network, set
// PKGCACHE_HOST_ADDRESS=1 so builds reach the cache by host.docker.internal instead of
// loopback. There is no flag for it: the caller is another program's --runtime setting,
// and an environment variable is the only channel that reaches here without that program
// having to know about it.
//
// It is deliberately not a general docker proxy. It does not parse docker's flags beyond
// finding the verb, and it never rewrites a command it does not recognise.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aabdlwahab/PKGCache/internal/clientbuild"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	// The real container command. PKGCACHE_DOCKER lets this sit in front of podman or
	// nerdctl, which is the same substitution this program is itself performing.
	docker := os.Getenv("PKGCACHE_DOCKER")
	if docker == "" {
		docker = "docker"
	}

	verb := ""
	for _, argument := range args {
		// The verb is the first thing that is not a global flag or its value. Docker's
		// globals that take a value are few and known; anything else beginning with a
		// dash is a boolean.
		if !strings.HasPrefix(argument, "-") {
			verb = argument
			break
		}
	}

	switch verb {
	case "build", "pull":
	default:
		// Not ours. Straight through, with no cache started and nothing parsed — the
		// overwhelming majority of what a tool like crate runs is `image inspect`,
		// `save`, `load` and `run`, and none of it should pay for this program existing.
		return passthrough(ctx, docker, args)
	}

	snapshot, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return fail(err)
	}
	// The daemon is pkgcache, not this program. Without naming it, local.Ensure
	// re-executes whoever is calling — and this program forwards what it does not
	// recognise to docker, so the daemon's own arguments arrive at docker as
	// `docker -data-dir …` and the cache never starts.
	daemon, found := local.DaemonPath()
	if !found {
		fmt.Fprintf(os.Stderr,
			"pkgcache-docker: cannot find the pkgcache binary, so there is no cache to use\n"+
				"  running `%s %s` directly\n", docker, verb)
		return passthrough(ctx, docker, args)
	}
	state, err := local.Ensure(ctx, local.EnsureOptions{
		Snapshot: snapshot, Notes: os.Stderr,
		Executable: daemon,
		// A daemon of a different version is normal here: this is a different binary.
		DifferentBinary: true,
	})
	if err != nil {
		// A cache that will not start is not a reason to fail a build somebody asked
		// another program for. Docker can still do this itself, and saying so is more
		// use than stopping.
		fmt.Fprintf(os.Stderr,
			"pkgcache-docker: %v\n  running `%s %s` directly\n", err, docker, verb)
		return passthrough(ctx, docker, args)
	}

	switch verb {
	case "build":
		return runBuild(ctx, snapshot, state, docker, args)
	default:
		return runPull(ctx, state, docker, args)
	}
}

// runBuild rewrites the Dockerfile and hands the build to docker.
func runBuild(
	ctx context.Context, snapshot *config.Snapshot, state local.State, docker string,
	args []string,
) int {
	options := clientbuild.Options{
		Bridge:   state.BaseURL(),
		Registry: state.Addr,
		Project:  local.CurrentProject(snapshot.DataDir),
		AptProxy: state.BaseURL(),
		GitHosts: []string{"github.com", "gitlab.com"},
		// Loopback by default, which is what a native Linux daemon can reach and what
		// `pkgcache build` also defaults to.
		//
		// host.docker.internal is the opposite trade and not a safe default: Docker
		// Desktop and WSL resolve it and a native Linux daemon does not, so choosing it
		// here fails with a DNS error on the commonest setup. There is no flag to pass —
		// the caller is another program's --runtime — so the switch is an environment
		// variable, which is the one channel that reaches this without going through
		// crate.
		HostAddress: truthy(os.Getenv("PKGCACHE_HOST_ADDRESS")),
		// A base that is already here is left alone. This is the case the shim exists
		// for: an orchestrator that builds a shared base image first and then builds
		// every service FROM it. Rewriting that name sends the build to a registry for
		// something never published.
		LocalImage: clientbuild.DefaultLocalImage(docker),
		Indexes: clientbuild.DiscoverIndexes(ctx, state.BaseURL(),
			local.CurrentProject(snapshot.DataDir)),
	}
	// clientbuild names "docker" when it runs the build. A Runner that substitutes the
	// command is how this program can sit in front of podman or nerdctl, which is the
	// same substitution it is itself performing one level up.
	if docker != "docker" {
		options.Runner = func(ctx context.Context, _ string, args []string) error {
			// #nosec G204 -- the command is the operator's PKGCACHE_DOCKER choice.
			command := exec.CommandContext(ctx, docker, args...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			command.Cancel = func() error { return nil }
			return command.Run()
		}
	}
	options = clientbuild.FromEnvironment(options)
	// The verb itself is not an argument to the builder: clientbuild adds it.
	if err := clientbuild.Build(ctx, options, without(args, "build")); err != nil {
		return fail(err)
	}
	return 0
}

// runPull fetches each image through the cache, leaving docker's own flags alone.
func runPull(ctx context.Context, state local.State, docker string, args []string) int {
	rest := without(args, "pull")
	var images, flags []string
	for _, argument := range rest {
		if strings.HasPrefix(argument, "-") {
			flags = append(flags, argument)
			continue
		}
		images = append(images, argument)
	}
	if len(images) != 1 || len(flags) > 0 {
		// `docker pull -a`, a digest pin, or several images at once. Each has a meaning
		// this program would have to reproduce exactly, and reproducing it approximately
		// is worse than not trying: the image somebody gets would differ from the image
		// they asked for.
		fmt.Fprintf(os.Stderr,
			"pkgcache-docker: this pull has flags or several images; running it directly\n")
		return passthrough(ctx, docker, args)
	}
	if err := clientbuild.Pull(ctx, images[0], clientbuild.PullOptions{
		Registry: state.Addr, Docker: docker, Notes: os.Stderr,
	}); err != nil {
		return fail(err)
	}
	return 0
}

// passthrough runs the real container command with the arguments untouched.
func passthrough(ctx context.Context, docker string, args []string) int {
	// #nosec G204 -- forwarding the caller's own command line is the entire purpose.
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child has the terminal's signals already; killing it here would race the
	// shutdown it is performing.
	cmd.Cancel = func() error { return nil }
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// Docker's status is the honest answer and callers branch on it.
			return exit.ExitCode()
		}
		return fail(err)
	}
	return 0
}

// without removes the first occurrence of the verb, which the caller supplies separately.
func without(args []string, verb string) []string {
	out := make([]string, 0, len(args))
	removed := false
	for _, argument := range args {
		if !removed && argument == verb {
			removed = true
			continue
		}
		out = append(out, argument)
	}
	return out
}

// truthy reads an environment flag the way a person expects to be able to write one.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "pkgcache-docker: %v\n", err)
	return 1
}
