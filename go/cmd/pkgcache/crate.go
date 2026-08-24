package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/clientbuild"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache crate` — run crate with its builds and pulls served from the cache.
//
// crate is a container orchestrator: `crate prepare` builds or pulls an image for every
// service in a manifest, and it does that by shelling out to real `docker build` and
// `docker pull`. That is what makes it wrappable at all, and it is also what decides the
// shape of the wrapper — there is no file to rewrite the way `pkgcache compose` rewrites a
// compose file, because crate owns its manifests and builds from them directly.
//
// So this wraps the environment rather than the input, which is the same promise from the
// other end: nothing outside the cache directory changes, and nothing outlives the
// command. Specifically, it writes a Docker client configuration in a temporary directory
// and points crate at it with DOCKER_CONFIG. Docker reads the build proxy from that file,
// so every RUN in every Dockerfile crate builds gets apt and apk from the cache — without
// `docker-build-setup` having edited anything permanent.
//
// The existing configuration is copied into the temporary one first, so registry logins
// and everything else the person has set up still apply. A wrapper that quietly logged
// somebody out of their private registry would not be worth using.
//
// What this does not do is reroute `docker pull`. crate pulls images by the names its
// manifest gives them, and short of a registry mirror there is no per-invocation way to
// change where the daemon fetches them. `pkgcache pull` those images first, or turn on
// `pkgcache docker-setup -mirror`, and crate's pull-if-missing finds them already present.

func runCrate(ctx context.Context, args []string) error {
	ours, theirs := splitAtDoubleDash(args)
	snap, state, environment, rest, err := startSession(ctx, "crate", ours,
		`pkgcache crate — run crate with its builds served from this cache

usage:
  pkgcache crate -- prepare -c .manifests/app.yaml
  pkgcache crate -- deploy -c .manifests/app.yaml

Runs crate with a Docker client configuration that sends apt and apk through this
machine's cache, so every image crate builds gets its packages from here. Your own Docker
configuration is copied first, so registry logins still work, and nothing outside the
cache directory is changed — the configuration lives in a temporary directory and goes
away with the command.

Image pulls are not rerouted: crate pulls the names its manifest gives. Pull them through
the cache first with `+"`pkgcache pull`"+`, and crate finds them already present.

flags:
`)
	if err != nil {
		return err
	}
	command := append(rest, theirs...)
	if len(command) == 0 {
		return errors.New(
			"crate: no arguments given; use `pkgcache crate -- prepare -c <manifest>`")
	}

	binary, err := exec.LookPath("crate")
	if err != nil {
		return errors.New(
			"crate is not on PATH.\n" +
				"  This wraps the crate orchestrator; install it first, or run the underlying\n" +
				"  docker commands through `pkgcache build` and `pkgcache pull` instead.")
	}

	configDir, cleanup, err := buildProxyConfig(state, os.Getenv("DOCKER_CONFIG"))
	if err != nil {
		return err
	}
	defer cleanup()

	// DOCKER_CONFIG is how every docker subprocess crate starts finds this, including the
	// ones it starts for services this command never sees.
	environment = append(environment, "DOCKER_CONFIG="+configDir)

	// #nosec G204 -- the arguments are the ones the caller typed.
	child := exec.CommandContext(ctx, binary, command...)
	child.Env = environment
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child gets the terminal's signals from the terminal; killing it here as well
	// would race the shutdown it is already performing.
	child.Cancel = func() error { return nil }

	runErr := child.Run()
	closeBridges()
	full := reportFull(snap.DataDir)
	if runErr == nil {
		if full != nil {
			return full
		}
		return nil
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		// crate has already said whatever it wanted to say.
		return &exitError{code: exit.ExitCode()}
	}
	return fmt.Errorf("crate: %w", runErr)
}

// buildProxyConfig writes a throwaway Docker client configuration carrying the cache's
// build proxy, and returns the directory to point DOCKER_CONFIG at.
//
// A copy of the existing configuration rather than a fresh one: it holds registry
// credentials, and a wrapper that silently logged somebody out of their private registry
// would be worse than no wrapper.
func buildProxyConfig(state local.State, existing string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "pkgcache-docker-")
	if err != nil {
		return "", func() {}, fmt.Errorf("crate: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }

	source := existing
	if source == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			source = filepath.Join(home, ".docker")
		}
	}
	if source != "" {
		// #nosec G304 -- a path derived from the caller's own Docker configuration.
		if body, err := os.ReadFile(filepath.Join(source, "config.json")); err == nil {
			if err := os.WriteFile(
				filepath.Join(directory, "config.json"), body, 0o600); err != nil {
				cleanup()
				return "", func() {}, fmt.Errorf("crate: %w", err)
			}
		}
	}

	// The gateway name, not loopback: the proxy address goes into a build, and a
	// container's loopback is its own.
	_, port, found := strings.Cut(state.Addr, ":")
	if !found {
		cleanup()
		return "", func() {}, fmt.Errorf("crate: cannot read a port from %q", state.Addr)
	}
	// The gateway name rather than loopback, because this address goes into a build and
	// a container's loopback is its own.
	gateway := clientbuild.DefaultHostGateway + ":" + port

	if err := local.ApplyDockerBuildProxy(local.DockerBuildProxy{
		Address:    gateway,
		ConfigPath: filepath.Join(directory, "config.json"),
		Out:        io.Discard,
	}); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return directory, cleanup, nil
}
