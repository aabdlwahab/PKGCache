package clientbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/aabdlwahab/PKGCache/internal/dockerfile"
)

// Pulling an image through a cache and leaving it under its own name.
//
// Shared by `pkgcache pull` and by the pkgcache-docker shim, which are the same operation
// reached two ways: one somebody types, one another program invokes without knowing a
// cache exists. Two implementations of "fetch this through the cache and put the name
// back" would eventually disagree about a name, and the disagreement would look like a
// missing image.

// PullOptions configures one image pull.
type PullOptions struct {
	// Registry is the cache's authority — host:port — that images are fetched through.
	Registry string
	// Keep leaves the cache-addressed tag in place beside the original name.
	Keep bool
	// Docker is the container command to drive. Empty means "docker".
	Docker string
	// Out and Err receive the container command's output.
	Out, Err io.Writer
	// Notes receives a line about anything skipped. Nil discards them.
	Notes io.Writer
}

// Pull fetches one image through the cache and tags it back to the name asked for.
//
// An image from a registry the cache does not serve is pulled directly and said so:
// somebody expecting the cache to be involved should not be quietly sent to the internet.
func Pull(ctx context.Context, image string, o PullOptions) error {
	mapped := dockerfile.MapImage(image, o.Registry)
	if mapped == "" {
		if o.Notes != nil {
			fmt.Fprintf(o.Notes,
				"pkgcache: %s is not served by this cache; pulling it directly\n", image)
		}
		mapped = image
	}
	if err := runDocker(ctx, o, "pull", mapped); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	if mapped == image {
		return nil
	}
	// Tagged back, because the cache's address is how this was fetched and not what it
	// is. A compose file or a manifest naming alpine:3.20 has to find it afterwards.
	if err := runDocker(ctx, o, "tag", mapped, image); err != nil {
		return fmt.Errorf("tag %s: %w", image, err)
	}
	if o.Keep {
		return nil
	}
	// Two names for one image is a puzzle in `docker images`, and the address is the less
	// useful of them. Untagging cannot delete the image: the real name still refers to it.
	if err := runDocker(ctx, o, "rmi", mapped); err != nil && o.Notes != nil {
		fmt.Fprintf(o.Notes, "pkgcache: %s is pulled, but %s is still tagged: %v\n",
			image, mapped, err)
	}
	return nil
}

func runDocker(ctx context.Context, o PullOptions, args ...string) error {
	command := o.Docker
	if command == "" {
		command = "docker"
	}
	// #nosec G204 -- the arguments are an image reference this package derived.
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout, cmd.Stderr = o.Out, o.Err
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}
