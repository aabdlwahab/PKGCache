package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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
		"reach the cache through "+clientbuild.DefaultHostGateway+" instead of loopback "+
			"(default: whichever this Docker daemon can actually reach)")
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

Images from any registry the reference names — Docker Hub, ghcr.io, nvcr.io, gcr.io and
the rest — are served from the cache. What is passed to docker untouched is a registry
only this machine can reach, such as localhost:5000 or an address on the build network,
which is said rather than done silently.

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

	// Derived unless somebody said otherwise. On macOS and Windows the daemon is always
	// a virtual machine and loopback is never right, which made -host-address a flag you
	// had to know about before your first pull could work — a fact about the platform,
	// asked as a question.
	gateway := *hostAddress
	if !flagGiven(fs, "host-address") {
		gateway = clientbuild.GatewayDefault(ctx, "")
	}
	registry := state.Addr
	if gateway {
		_, port, found := strings.Cut(state.Addr, ":")
		if !found {
			return fmt.Errorf("pull: cannot read a port from %q", state.Addr)
		}
		registry = clientbuild.DefaultHostGateway + ":" + port
		if !flagGiven(fs, "host-address") {
			fmt.Fprintln(os.Stderr, clientbuild.GatewayNote(registry))
		}
	}

	for _, image := range fs.Args() {
		if *dryRun {
			mapped := dockerfile.MapImage(image, registry)
			if mapped == "" {
				fmt.Fprintf(os.Stderr,
					"pkgcache: %s is not served by this cache; pulling it directly\n", image)
				mapped = image
			}
			fmt.Printf("%s\n  <- %s\n", image, mapped)
			continue
		}
		if err := clientbuild.Pull(ctx, image, clientbuild.PullOptions{
			Registry: registry, Keep: *keep, Notes: os.Stderr,
		}); err != nil {
			return err
		}
	}
	if full := reportFull(snap.DataDir); full != nil {
		return full
	}
	return nil
}
