package dockerfile

import (
	"strings"
	"testing"
)

func inProject(project string) Options {
	o := bridge()
	o.Project = project
	o.AptProxy = "http://127.0.0.1:41999"
	return o
}

// The bug this file exists for: a build in a project fetched its wheels from that
// project and its base images and its apt packages from the global one. Every line the
// rewriter emits has to name the same project, or the cache silently splits a build's
// content across two of them.
func TestEveryRewrittenLineNamesTheSameProject(t *testing.T) {
	out, _ := rewrite(t,
		"FROM python:3.12-alpine\nRUN apt-get install -y curl\nRUN pip install requests\n",
		inProject("work"))

	for _, want := range []string{
		// The project rides the image name, because Docker cannot be given a base path.
		"FROM 127.0.0.1:41999/work/dockerhub/library/python:3.12-alpine",
		// ...and the proxy username, because a proxied request's URL is the upstream's.
		"ARG http_proxy=http://work@127.0.0.1:41999",
		// The two that were already right, asserted so a change to one is a change to all.
		"ARG PIP_INDEX_URL=http://127.0.0.1:41999/work/pypi/root/pypi/+simple/",
		"ARG NPM_CONFIG_REGISTRY=http://127.0.0.1:41999/work/npm/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// The global project is the absence of a label, not the name of one. A prefix here would
// ask the registry for a repository called "global/dockerhub/..." that nobody has, and a
// proxy username of "global" is ignored on the way back in — so neither is written.
func TestGlobalProjectIsUnchanged(t *testing.T) {
	out, _ := rewrite(t,
		"FROM python:3.12-alpine\nRUN apt-get install -y curl\n", inProject("global"))

	if !strings.Contains(out, "FROM 127.0.0.1:41999/dockerhub/library/python:3.12-alpine") {
		t.Errorf("the global project gained a path segment:\n%s", out)
	}
	if !strings.Contains(out, "ARG http_proxy=http://127.0.0.1:41999\n") {
		t.Errorf("the global project gained a proxy username:\n%s", out)
	}
}

// A registry with an alias of its own keeps it, with the project in front — the segment
// says which upstream, the project says whose copy.
func TestNamedRegistryKeepsItsSegmentUnderTheProject(t *testing.T) {
	out, _ := rewrite(t, "FROM ghcr.io/astral-sh/uv:latest\n", inProject("work"))
	if !strings.Contains(out, "127.0.0.1:41999/work/ghcr/astral-sh/uv:latest") {
		t.Errorf("named registry not scoped:\n%s", out)
	}
}
