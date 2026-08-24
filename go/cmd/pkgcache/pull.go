package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/clientbuild"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/dockerfile"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// `pkgcache pull` — docker pull through the cache, without rerouting the machine.
//
// The two ways to pull through a cache were both unsatisfying. Writing the address by hand
// means saying `127.0.0.1:41780/dockerhub/library/alpine:3.20` and then living with an
// image whose name says where it came from rather than what it is. `docker-setup -mirror`
// fixes the name but reroutes every pull on the machine, which is a decision about the
// machine and not about this command.
//
// This is the third thing, and it is what `pkgcache build` already does for a Dockerfile:
// rewrite the reference, fetch through the cache, and leave the result named the way the
// person wrote it. Nothing outside the cache directory changes, and nothing is rerouted
// after the command exits.
//
// The rewrite is dockerfile.MapImage, the same function a FROM line goes through. An image
// pulled by hand and the same image pulled by a build have to resolve to the same bytes,
// and two implementations of that would eventually disagree.

func runPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	hostAddress := fs.Bool("host-address", false,
		"reach the cache through "+clientbuild.DefaultHostGateway+" instead of loopback, "+
			"for a daemon that cannot see this terminal's network: Docker Desktop, WSL, CI")
	keep := fs.Bool("keep-cache-tag", false,
		"leave the cache-addressed tag in place as well as the original name")
	dryRun := fs.Bool("print", false, "print what would be pulled, and pull nothing")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `pkgcache pull — docker pull through the cache

usage: pkgcache pull [flags] <image>...

Pulls each image through this machine's cache and leaves it under the name you asked for,
so `+"`pkgcache pull alpine:3.20`"+` ends with an `+"`alpine:3.20`"+` in docker images,
not a 127.0.0.1:41780/dockerhub/library/alpine:3.20.

Unlike `+"`docker-setup -mirror`"+` this reroutes nothing: it changes where these images
come from, once, and leaves the Docker daemon's configuration alone.

Images from Docker Hub, ghcr.io and quay.io are served from the cache. Anything else is
passed to docker untouched and pulled directly, which is said rather than done silently.

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

	snap, err := config.LoadLocal(config.LocalFlags{})
	if err != nil {
		return err
	}
	// The cache has to be running before an image can be pulled from it, and this is
	// somebody asking for packages rather than asking a question — so it starts.
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
	if err != nil {
		return err
	}

	registry := state.Addr
	if *hostAddress {
		_, port, found := strings.Cut(state.Addr, ":")
		if !found {
			return fmt.Errorf("pull: cannot read a port from %q", state.Addr)
		}
		registry = clientbuild.DefaultHostGateway + ":" + port
	}

	for _, image := range fs.Args() {
		mapped := dockerfile.MapImage(image, registry)
		if mapped == "" {
			// Not a registry this cache serves. Said out loud: somebody running this
			// expects the cache to be involved, and silently pulling from the internet
			// would leave them believing something that is not true.
			fmt.Fprintf(os.Stderr,
				"pkgcache: %s is not served by this cache; pulling it directly\n", image)
			mapped = image
		}
		if *dryRun {
			fmt.Printf("%s\n  <- %s\n", image, mapped)
			continue
		}
		if err := pullThrough(ctx, image, mapped, *keep); err != nil {
			return err
		}
	}
	if full := reportFull(snap.DataDir); full != nil {
		return full
	}
	return nil
}

// pullThrough fetches one image and puts it back under the name that was asked for.
func pullThrough(ctx context.Context, image, mapped string, keep bool) error {
	if err := docker(ctx, "pull", mapped); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	if mapped == image {
		return nil
	}
	// Tagged back, because the cache's address is how this image was fetched and not what
	// it is. A Compose file or a Makefile naming alpine:3.20 has to find it afterwards.
	if err := docker(ctx, "tag", mapped, image); err != nil {
		return fmt.Errorf("tag %s: %w", image, err)
	}
	if keep {
		return nil
	}
	// The cache-addressed tag is removed rather than left beside it: two names for one
	// image is a puzzle in `docker images`, and the address is the less useful of them.
	// Untagging cannot delete the image itself — the original name still refers to it.
	if err := docker(ctx, "rmi", mapped); err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache: %s is pulled, but %s is still tagged: %v\n",
			image, mapped, err)
	}
	return nil
}

// docker runs one docker command, quietly unless it fails.
func docker(ctx context.Context, args ...string) error {
	// #nosec G204 -- the arguments are an image reference this program derived.
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
