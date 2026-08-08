package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/brightskies/pkgreg/internal/clientbuild"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/local"
)

func runDockerSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker-setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	mirror := fs.Bool("mirror", false,
		"also register the cache as a registry mirror, so unmodified image names are cached")
	dryRun := fs.Bool("dry-run", false, "print every change without applying it")
	uninstall := fs.Bool("uninstall", false, "reverse a previous run")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache docker-setup — teach the Docker daemon about this cache

usage: pkgcache docker-setup [flags]

This is the one step in pkgcache that changes something outside your cache directory,
and it exists because Docker's daemon is a separate process that never sees a shell's
environment — and under Docker Desktop runs in a virtual machine whose loopback is not
yours. One file has to say that this machine's cache is an acceptable plain-HTTP
registry.

On Docker Desktop the file is under your home and needs no administrator access. On
native Linux it is /etc/docker/daemon.json and needs root — and there it is usually
unnecessary, because the daemon already accepts loopback.

With -mirror, `+"`docker pull python:3.12`"+` is served from the cache with no rewrite and
no wrapper. That is off by default: it reroutes every pull on this machine, which is
not something a setup command should do to you quietly.

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
	address, err := gatewayAddress(ctx, snap, *uninstall)
	if err != nil {
		return err
	}
	return local.ApplyDockerSetup(local.DockerSetup{
		Address: address, Mirror: *mirror, DryRun: *dryRun, Uninstall: *uninstall,
		Out: os.Stdout,
	})
}

func runDockerBuildSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docker-build-setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print every change without applying it")
	uninstall := fs.Bool("uninstall", false, "reverse a previous run")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache docker-build-setup — cache apt and apk in every build on this machine

usage: pkgcache docker-build-setup [flags]

Sets the build proxy in your Docker client configuration. HTTP_PROXY and its relatives
are predefined build arguments, injected into every RUN with no ARG line to declare
them, so this is the only way to reach a build somebody else wrote — including a
colleague's Makefile, which `+"`pkgcache build`"+` cannot.

It covers apt and apk and nothing else: pip, uv and npm speak HTTPS to their upstreams,
which through a proxy means a CONNECT tunnel this proxy does not offer. Use
`+"`pkgcache build`"+` for those.

Because it applies to every build, the cache has to be running for those builds to
work. Consider `+"`pkgcache persist`"+` alongside it.

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
	address, err := gatewayAddress(ctx, snap, *uninstall)
	if err != nil {
		return err
	}
	return local.ApplyDockerBuildProxy(local.DockerBuildProxy{
		Address: address, DryRun: *dryRun, Uninstall: *uninstall, Out: os.Stdout,
	})
}

// gatewayAddress is the authority a container reaches this cache on.
//
// The daemon is asked to trust an address, so it must be the address a build will
// actually use — host.docker.internal, not loopback, since a container's loopback is
// its own. Uninstalling does not start a daemon to find out: the configured port is
// what a previous run would have written.
func gatewayAddress(ctx context.Context, snap *config.Snapshot, uninstall bool) (string, error) {
	_, port, err := splitHostPort(snap.LocalAddr())
	if err != nil {
		return "", err
	}
	if !uninstall {
		// Starting the cache first means the address is the one actually bound, which
		// differs whenever the fixed port was taken.
		state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
		if err != nil {
			return "", err
		}
		if _, bound, err := splitHostPort(state.Addr); err == nil {
			port = bound
		}
	}
	return clientbuild.DefaultHostGateway + ":" + port, nil
}

func splitHostPort(address string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("pkgcache: %q is not a host:port: %w", address, err)
	}
	return host, port, nil
}
