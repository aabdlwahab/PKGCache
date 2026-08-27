package dockerfile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Compose rewriting works on the output of `docker compose config`, never on the
// file the developer wrote.
//
// That is the whole trick. Compose files have extends, include, multiple -f layers,
// profiles, .env interpolation and paths relative to whichever file declared them —
// and `docker compose config` resolves every one of those into a single canonical
// document with absolute paths. Reimplementing that would be a second Compose
// implementation, permanently one release behind the real one. Asking Compose to do
// it costs one subprocess.
//
// The rewritten document then goes back to Compose on stdin rather than through a
// temporary file, because the rendered configuration contains interpolated
// environment values, which can include credentials. Nothing about this needs to
// touch the disk, so nothing does — and that includes each service's rewritten
// Dockerfile, which travels inside the document as `dockerfile_inline`.

// ComposeResult is a rewritten configuration and what was done to it.
type ComposeResult struct {
	Content []byte
	Changes []Change
	// Dockerfiles maps a service name to the rewritten Dockerfile its build needs.
	// Content is already inlined into the document; this is what the caller reports
	// on, and how it learns whether any service needs the CA as a build secret.
	Dockerfiles map[string]ComposeBuild
}

// ComposeBuild is one service's build, after rewriting.
type ComposeBuild struct {
	Content     []byte
	NeedsSecret bool
	Source      string
}

// RewriteCompose rewrites a rendered Compose configuration.
//
// read supplies a service's Dockerfile given its path, so the caller controls file
// access and this stays testable without a filesystem.
func RewriteCompose(
	rendered []byte, options Options, read func(path string) ([]byte, error),
) (ComposeResult, error) {
	var document map[string]any
	if err := yaml.Unmarshal(rendered, &document); err != nil {
		return ComposeResult{}, fmt.Errorf("compose: parse rendered configuration: %w", err)
	}
	result := ComposeResult{Dockerfiles: map[string]ComposeBuild{}}

	services, _ := document["services"].(map[string]any)
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		build, hasBuild := service["build"].(map[string]any)

		// `image` on a service that also builds names the *output* tag, not something
		// to pull. Rewriting it would rename the image the developer just built, and
		// they would find out when a later `docker run` could not find it.
		if image, ok := service["image"].(string); ok && !hasBuild && !options.SkipFrom {
			if mapped := mapImage(image, options.Registry); mapped != "" {
				service["image"] = mapped
				result.Changes = append(result.Changes, Change{From: image, To: mapped})
			}
		}
		if !hasBuild {
			continue
		}

		// RUN steps reach the bridge over the host's loopback; in cache-address mode
		// the network is ordinary and this is merely harmless.
		if options.Mode == Bridge {
			build["network"] = "host"
		}

		path, source, err := buildDockerfile(build, read)
		if err != nil {
			return ComposeResult{}, fmt.Errorf("compose: service %s: %w", name, err)
		}
		rewritten, err := Rewrite(source, options)
		if err != nil {
			return ComposeResult{}, fmt.Errorf("compose: service %s: %w", name, err)
		}
		result.Changes = append(result.Changes, rewritten.Changes...)
		result.Dockerfiles[name] = ComposeBuild{
			Content: rewritten.Content, NeedsSecret: rewritten.NeedsSecret, Source: path,
		}
		// The rewrite rides inside the document rather than being written out and
		// named by path: nothing to clean up, and nothing for a Docker client that
		// cannot read this process's filesystem — a snap, with its private /tmp — to
		// fail to open. An inline Dockerfile and a dockerfile path are mutually
		// exclusive, so the rewrite replaces whichever was there.
		build["dockerfile_inline"] = string(rewritten.Content)
		delete(build, "dockerfile")
	}

	out, err := yaml.Marshal(document)
	if err != nil {
		return ComposeResult{}, fmt.Errorf("compose: render: %w", err)
	}
	result.Content = out
	return result, nil
}

// buildDockerfile returns where a service's Dockerfile came from and what is in it.
//
// A build that already used `dockerfile_inline` has no file to read: `compose config`
// renders it back out as itself, and reading the Dockerfile that is not there would
// fail a build that works without pkgcache.
func buildDockerfile(
	build map[string]any, read func(path string) ([]byte, error),
) (path string, content []byte, err error) {
	if inline, ok := build["dockerfile_inline"].(string); ok && inline != "" {
		return "", []byte(inline), nil
	}
	path = dockerfilePath(build)
	source, err := read(path)
	return path, source, err
}

// dockerfilePath resolves a build's Dockerfile, honouring Compose's defaults.
// `compose config` has already made the context absolute, so this is only ever
// joining a relative dockerfile onto it.
func dockerfilePath(build map[string]any) string {
	name, _ := build["dockerfile"].(string)
	if name == "" {
		name = "Dockerfile"
	}
	if strings.HasPrefix(name, "/") {
		return name
	}
	context, _ := build["context"].(string)
	if context == "" {
		context = "."
	}
	return strings.TrimRight(context, "/") + "/" + name
}
