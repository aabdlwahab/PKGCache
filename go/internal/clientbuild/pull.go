package clientbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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
	// Docker's own message goes to stderr and its exit error says only "exit status 1",
	// so the interesting text is captured on the way past rather than inferred from the
	// error. Bounded, because this is a diagnostic and not a log.
	var said boundedBuffer
	if err := runDocker(ctx, o, &said, "pull", mapped); err != nil {
		return pullFailure(image, mapped, said.String(), err)
	}
	if mapped == image {
		return nil
	}
	// Tagged back, because the cache's address is how this was fetched and not what it
	// is. A compose file or a manifest naming alpine:3.20 has to find it afterwards.
	if err := runDocker(ctx, o, nil, "tag", mapped, image); err != nil {
		return fmt.Errorf("tag %s: %w", image, err)
	}
	if o.Keep {
		return nil
	}
	// Two names for one image is a puzzle in `docker images`, and the address is the less
	// useful of them. Untagging cannot delete the image: the real name still refers to it.
	if err := runDocker(ctx, o, nil, "rmi", mapped); err != nil && o.Notes != nil {
		fmt.Fprintf(o.Notes, "pkgcache: %s is pulled, but %s is still tagged: %v\n",
			image, mapped, err)
	}
	return nil
}

// pullFailure explains a failed pull in terms of the image somebody asked for.
//
// The address in Docker's message is the cache's, because that is where the pull was
// sent, and the status is whatever the registry behind it gave — so a repository that
// does not exist arrives as "401 Unauthorized" against a loopback address. That reads as
// a broken cache and is not one: Docker Hub answers 401 rather than 404 for a repository
// it will not confirm, so that it does not leak which private repositories exist.
//
// Without this, the first thing anybody does is investigate the cache.
func pullFailure(image, mapped, said string, err error) error {
	if mapped == image {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	text := said + " " + err.Error()
	// A client that cannot reach its own daemon says so in these words, and it can say
	// "connection refused" while doing it. That is a different failure from the one below
	// — nothing was asked of the cache at all — and offering a different cache address for
	// it would be a wrong guess stated confidently.
	daemonDown := strings.Contains(text, "Cannot connect to the Docker daemon")
	hint := ""
	switch {
	// The daemon never reached the cache at all. It is not a cache error and the cache
	// is not down: the address belongs to whoever is dialling it, and a daemon in a
	// virtual machine dials its own. Detection normally settles this before a pull is
	// attempted, so reaching here means detection was wrong about this daemon.
	case !daemonDown && (strings.Contains(text, "connection refused") ||
		strings.Contains(text, "no such host") ||
		strings.Contains(text, "i/o timeout")):
		hint = "\n  The daemon could not reach that address, which usually means it does" +
			" not share\n  this terminal's network — Docker Desktop, a remote daemon, CI." +
			"\n  Try -host-address, which reaches the cache at " + DefaultHostGateway + "."
	// The daemon found the cache and insisted on TLS. A laptop cache is plain HTTP on
	// purpose, and one file has to say that is acceptable.
	case strings.Contains(text, "server gave HTTP response to HTTPS client"),
		strings.Contains(text, "http: server gave HTTP"):
		hint = "\n  The daemon reached the cache but requires HTTPS from it. A cache on" +
			" this machine\n  is plain HTTP by design; `pkgcache docker-setup` is the one" +
			" file that says so.\n  Restart Docker afterwards — daemon.json is read at" +
			" startup."
	default:
		for _, marker := range []string{"401", "Unauthorized", "404", "not found"} {
			if strings.Contains(text, marker) {
				hint = "\n  That address is this cache, and the status came from the registry" +
					" behind it.\n  Docker Hub answers 401 for a repository that does not exist" +
					" as well as for one that\n  is private, so check the name first:" +
					" `docker pull " + image + "` reports the same\n  thing with no cache in" +
					" the way."
				break
			}
		}
	}
	return fmt.Errorf("pull %s (through %s): %w%s", image, mapped, err, hint)
}

// boundedBuffer keeps the first few KiB of what a command said. A registry error is one
// line; anything longer is a progress display nobody needs a second copy of.
type boundedBuffer struct{ b []byte }

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if room := 8192 - len(w.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.b = append(w.b, p[:room]...)
	}
	return len(p), nil
}

func (w *boundedBuffer) String() string { return string(w.b) }

func runDocker(ctx context.Context, o PullOptions, tap io.Writer, args ...string) error {
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
	if tap != nil {
		// Still shown, and also kept: the person reading the terminal needs it now and
		// pullFailure needs it a moment later.
		cmd.Stderr = io.MultiWriter(cmd.Stderr, tap)
	}
	return cmd.Run()
}
