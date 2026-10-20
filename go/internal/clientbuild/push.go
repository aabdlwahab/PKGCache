package clientbuild

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// Pushing an image built on this machine into the cache's local/ namespace.
//
// Docker pushes to a registry by name, so the image is tagged with the cache's address under
// local/, pushed, and the address tag taken off again: the reverse of Pull, which fetches an
// image under the cache's name and puts the image's own back.

// PushOptions configures one image push.
type PushOptions struct {
	// Registry is the cache's authority — host:port — as the Docker daemon reaches it.
	Registry string
	// Project is the project the image goes into, riding its name. Empty means global.
	Project string
	// Name is the repository[:tag] under local/ to push as. Empty keeps the image's own.
	Name string
	// Keep leaves the cache-addressed tag in place beside the original name.
	Keep bool
	// Docker is the container command to drive. Empty means "docker".
	Docker string
	// Out and Err receive the container command's output.
	Out, Err io.Writer
	// Notes receives a line about anything left behind. Nil discards them.
	Notes io.Writer
}

// Pushed is where an image went.
type Pushed struct {
	// Target is the name Docker pushed: <registry>/<project>/local/<repository>:<tag>.
	Target string
	// Repository and Tag name it under local/.
	Repository, Tag string
}

var (
	pushRepositoryRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	pushTagRE        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	imageIDRE        = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)
)

// PushName is the repository and tag an image is pushed as under local/.
//
// The image's own name, without the registry it came from: ghcr.io/team/tool:1 is pushed
// as local/team/tool:1, and an image already named local/… — one pulled back from a cache —
// keeps its name rather than gaining a second local/. A digest names no tag to push under,
// so it needs a name.
func PushName(image, name string) (repository, tag string, err error) {
	ref := image
	if name != "" {
		ref = name
	}
	if strings.Contains(ref, "@") || imageIDRE.MatchString(ref) {
		return "", "", fmt.Errorf("%s names no tag to push under; give one with -as name:tag", ref)
	}
	repository, tag = ref, ""
	if slash, colon := strings.LastIndex(ref, "/"), strings.LastIndex(ref, ":"); colon > slash {
		repository, tag = ref[:colon], ref[colon+1:]
	}
	if head, rest, found := strings.Cut(repository, "/"); found &&
		(strings.ContainsAny(head, ".:") || head == "localhost") {
		repository = rest
	}
	repository = strings.TrimPrefix(repository, ociname.Hosted+"/")
	if tag == "" {
		tag = "latest"
	}
	if !pushRepositoryRE.MatchString(repository) {
		return "", "", fmt.Errorf("%q is not a repository name a registry accepts: lowercase letters, "+
			"digits and . _ - between slashes", repository)
	}
	if !pushTagRE.MatchString(tag) {
		return "", "", fmt.Errorf("%q is not a tag a registry accepts", tag)
	}
	return repository, tag, nil
}

// Push puts one local image into the cache's local/ namespace.
func Push(ctx context.Context, image string, o PushOptions) (Pushed, error) {
	repository, tag, err := PushName(image, o.Name)
	if err != nil {
		return Pushed{}, err
	}
	target := o.Registry + "/" + router.OCIProjectPrefix(o.Project) + ociname.Hosted + "/" + repository + ":" + tag
	run := PullOptions{Docker: o.Docker, Out: o.Out, Err: o.Err}
	if err := runDocker(ctx, run, nil, "tag", image, target); err != nil {
		return Pushed{}, fmt.Errorf("tag %s: %w", image, err)
	}
	var said boundedBuffer
	pushErr := runDocker(ctx, run, &said, "push", target)
	// Untagged even when the push failed: a stray cache-addressed name in docker images is
	// the same puzzle either way. Not when the image was named by its id, where this tag may
	// be the only one and taking it off would delete the image.
	if !o.Keep && !imageIDRE.MatchString(image) {
		if err := runDocker(ctx, run, nil, "rmi", target); err != nil && o.Notes != nil {
			_, _ = fmt.Fprintf(o.Notes, "pkgcache: %s is still tagged: %v\n", target, err)
		}
	}
	if pushErr != nil {
		return Pushed{}, pushFailure(target, said.String(), pushErr)
	}
	return Pushed{Target: target, Repository: repository, Tag: tag}, nil
}

// pushFailure explains a failed push in terms of something to do about it, where Docker's
// own words make that possible.
func pushFailure(target, said string, err error) error {
	if strings.Contains(said, "HTTP response to HTTPS client") {
		return fmt.Errorf("docker push %s: the Docker daemon does not accept this cache as a "+
			"registry yet; run pkgcache docker-setup once, restart Docker, and push again", target)
	}
	return fmt.Errorf("docker push %s: %w", target, err)
}
