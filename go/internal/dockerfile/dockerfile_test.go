package dockerfile

import (
	"strings"
	"testing"
)

// Every test here is a trap a naive rewriter falls into. Two of them were found by a
// prototype getting them wrong against real builds, which is why they are asserted
// rather than assumed.

func bridge() Options {
	return Options{
		Mode: Bridge, Registry: "127.0.0.1:41999",
		Base: "http://127.0.0.1:41999", Project: "global",
		AptProxy: "http://cache:3142", GitHosts: []string{"github.com"},
	}
}

func rewrite(t *testing.T, source string, o Options) (string, Result) {
	t.Helper()
	result, err := Rewrite([]byte(source), o)
	if err != nil {
		t.Fatal(err)
	}
	return string(result.Content), result
}

func TestOfficialImageIsNamespacedUnderLibrary(t *testing.T) {
	// The tag colon must not be mistaken for a registry port. A rewriter that gets
	// this wrong silently leaves the most common base image in the world untouched,
	// and the only symptom is that the cache stays empty.
	out, result := rewrite(t, "FROM python:3.12-alpine\n", bridge())
	if !strings.Contains(out, "FROM 127.0.0.1:41999/dockerhub/library/python:3.12-alpine") {
		t.Fatalf("official image not rewritten:\n%s", out)
	}
	if len(result.Changes) != 1 || result.Changes[0].From != "python:3.12-alpine" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestReferencesThatMustBeLeftAlone(t *testing.T) {
	for name, source := range map[string]string{
		// A FROM naming an earlier stage is not an image.
		"a stage name":        "FROM alpine:3.20 AS base\nFROM base AS build\n",
		"scratch":             "FROM scratch\n",
		"an unknown registry": "FROM registry.internal.example/team/app:1\n",
		"a build arg":         "ARG BASE\nFROM ${BASE}\n",
	} {
		out, _ := rewrite(t, source, bridge())
		for _, line := range strings.Split(out, "\n") {
			if !isFrom(line) {
				continue
			}
			if strings.Contains(line, "127.0.0.1:41999") && !strings.Contains(source, "alpine:3.20") {
				t.Errorf("%s was rewritten: %q", name, line)
			}
		}
		if name == "a stage name" && strings.Contains(out, "/dockerhub/library/base") {
			t.Errorf("stage reference rewritten as an image:\n%s", out)
		}
	}
}

func TestOtherRegistriesMapToTheirUpstreamAlias(t *testing.T) {
	out, _ := rewrite(t, "FROM ghcr.io/astral-sh/uv:latest\nFROM quay.io/prometheus/busybox:latest\n", bridge())
	for _, want := range []string{
		"127.0.0.1:41999/ghcr/astral-sh/uv:latest",
		"127.0.0.1:41999/quay/prometheus/busybox:latest",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPlatformFlagAndDigestSurvive(t *testing.T) {
	out, _ := rewrite(t, "FROM --platform=linux/arm64 alpine@sha256:abc123 AS b\n", bridge())
	want := "FROM --platform=linux/arm64 127.0.0.1:41999/dockerhub/library/alpine@sha256:abc123 AS b"
	if !strings.Contains(out, want) {
		t.Fatalf("flags or digest lost:\n%s", out)
	}
}

// ARG is scoped to its stage. Declaring the block once would work in the first stage
// and quietly do nothing in the rest — the failure people report as "works locally".
func TestArgBlockIsRepeatedInEveryStage(t *testing.T) {
	out, result := rewrite(t, "FROM alpine AS a\nRUN true\nFROM alpine AS b\nRUN true\n", bridge())
	if result.Stages != 2 {
		t.Fatalf("stages = %d, want 2", result.Stages)
	}
	if got := strings.Count(out, "ARG PIP_INDEX_URL="); got != 2 {
		t.Fatalf("PIP_INDEX_URL declared %d times, want one per stage:\n%s", got, out)
	}
}

func TestHeredocBodyIsNotTreatedAsInstructions(t *testing.T) {
	source := "FROM alpine\nRUN <<EOF\nFROM this is shell text, not an instruction\nEOF\nRUN true\n"
	out, result := rewrite(t, source, bridge())
	if strings.Contains(out, "dockerhub/library/this") {
		t.Fatalf("rewrote inside a heredoc:\n%s", out)
	}
	if result.Stages != 1 {
		t.Fatalf("stages = %d, want 1 — a heredoc line counted as FROM", result.Stages)
	}
}

func TestCommentsAndSyntaxDirectiveAreUntouched(t *testing.T) {
	source := "# syntax=docker/dockerfile:1\n# FROM alpine is only a comment\nFROM alpine\n"
	out, result := rewrite(t, source, bridge())
	if !strings.HasPrefix(out, "# syntax=docker/dockerfile:1\n") {
		t.Fatalf("syntax directive moved off line 1:\n%s", out)
	}
	if result.Stages != 1 {
		t.Fatalf("a comment was parsed as an instruction: stages = %d", result.Stages)
	}
}

func TestLineContinuationsAreRejoinedNotSplit(t *testing.T) {
	source := "FROM alpine\nRUN apk add --no-cache \\\n    curl \\\n    git\n"
	out, _ := rewrite(t, source, bridge())
	if !strings.Contains(out, "RUN apk add --no-cache \\\n    curl \\\n    git") {
		t.Fatalf("continuation mangled:\n%s", out)
	}
	// The ARG block must not land in the middle of the continued command.
	if strings.Contains(out, "curl \\\nARG ") {
		t.Fatalf("ARG injected inside a continuation:\n%s", out)
	}
}

// Bridge mode is the whole reason a Linux build needs no certificate: the bridge is
// plain HTTP, so there is no TLS to verify.
func TestBridgeModeAddsNoCertificateAnywhere(t *testing.T) {
	out, result := rewrite(t, "FROM alpine\nRUN pip install six\n", bridge())
	if result.NeedsSecret {
		t.Fatal("bridge mode asked for a secret")
	}
	for _, forbidden := range []string{"type=secret", "PIP_CERT", "NODE_EXTRA_CA_CERTS"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("bridge mode emitted %q:\n%s", forbidden, out)
		}
	}
}

// CacheAddress mode is for daemons that cannot see the loopback bridge, so the CA has
// to reach each RUN — mounted for the step, never written into a layer.
func TestCacheAddressModeMountsTheCAIntoEveryRun(t *testing.T) {
	options := bridge()
	options.Mode = CacheAddress
	options.Registry = "cache:8443"
	options.Base = "https://cache:8443"

	out, result := rewrite(t, "FROM alpine\nRUN pip install six\nRUN npm ci\n", options)
	if !result.NeedsSecret {
		t.Fatal("cache-address mode did not report needing the secret")
	}
	if got := strings.Count(out, "--mount=type=secret,id="+SecretID); got != 2 {
		t.Fatalf("mounted on %d RUN steps, want 2:\n%s", got, out)
	}
	// node ignores the OS trust store, so pointing it at the mount is not optional.
	if !strings.Contains(out, "ARG NODE_EXTRA_CA_CERTS="+SecretTarget) {
		t.Fatalf("node was left without a CA path:\n%s", out)
	}
	if strings.Contains(out, "COPY") {
		t.Fatalf("cache-address mode copied something into the image:\n%s", out)
	}
}

func TestExistingSecretMountIsNotDuplicated(t *testing.T) {
	options := bridge()
	options.Mode = CacheAddress
	source := "FROM alpine\nRUN --mount=type=secret,id=" + SecretID + " pip install six\n"
	out, _ := rewrite(t, source, options)
	if got := strings.Count(out, "type=secret"); got != 1 {
		t.Fatalf("mount duplicated %d times:\n%s", got, out)
	}
}

func TestSkipFromLeavesImagesToTheDaemon(t *testing.T) {
	options := bridge()
	options.SkipFrom = true
	out, result := rewrite(t, "FROM python:3.12-alpine\nRUN true\n", options)
	if len(result.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", result.Changes)
	}
	if !strings.Contains(out, "FROM python:3.12-alpine") {
		t.Fatalf("FROM rewritten despite SkipFrom:\n%s", out)
	}
	if !strings.Contains(out, "ARG PIP_INDEX_URL=") {
		t.Fatal("SkipFrom also skipped the build arguments")
	}
}

func TestGitRewritingIsDeclaredOnlyWhenHostsAreGiven(t *testing.T) {
	options := bridge()
	options.GitHosts = nil
	out, _ := rewrite(t, "FROM alpine\n", options)
	if strings.Contains(out, "GIT_CONFIG_COUNT") {
		t.Fatalf("git config declared with no hosts:\n%s", out)
	}

	out, _ = rewrite(t, "FROM alpine\n", bridge())
	if !strings.Contains(out, "ARG GIT_CONFIG_COUNT=1") ||
		!strings.Contains(out, "ARG GIT_CONFIG_VALUE_0=https://github.com/") {
		t.Fatalf("git rewriting not declared:\n%s", out)
	}
}

func TestAptProxyIsOmittedRatherThanDeclaredEmpty(t *testing.T) {
	options := bridge()
	options.AptProxy = ""
	out, _ := rewrite(t, "FROM alpine\n", options)
	if strings.Contains(out, "http_proxy") {
		t.Fatalf("an empty proxy was declared, which is not the same as unset:\n%s", out)
	}
}

func TestRewriteRefusesIncompleteOptions(t *testing.T) {
	if _, err := Rewrite([]byte("FROM alpine\n"), Options{Project: "global"}); err == nil {
		t.Fatal("accepted options with no base URL")
	}
	if _, err := Rewrite([]byte("FROM alpine\n"), Options{Base: "http://x"}); err == nil {
		t.Fatal("accepted options with no project")
	}
}

// pip refuses a plain-HTTP index unless the host is loopback, and it refuses it by
// *ignoring the index*: the error is "no matching distribution", which reads as a missing
// package rather than a rejected repository.
//
// That is the HostGateway case this mode exists for. On Docker Desktop the base image and
// apt went through the cache perfectly and every `pip install` failed, because
// host.docker.internal is not loopback. Reported from a real Mac.
func TestNonLoopbackCacheIsMarkedTrustedForPip(t *testing.T) {
	gateway := Options{
		Mode: HostGateway, Registry: "host.docker.internal:41780",
		Base: "http://host.docker.internal:41780", Project: "global",
		AptProxy: "http://host.docker.internal:41780", GitHosts: []string{"github.com"},
	}
	out, _ := rewrite(t, "FROM python:3.12-slim\nRUN pip install requests\n", gateway)
	for _, want := range []string{
		"ARG PIP_TRUSTED_HOST=host.docker.internal",
		"ARG UV_INSECURE_HOST=host.docker.internal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rewrite is missing %q:\n%s", want, out)
		}
	}
	// The port is deliberately absent: pip reads a bare hostname as "trust it on any
	// port", and the cache's port moves when the fixed one is taken.
	if strings.Contains(out, "PIP_TRUSTED_HOST=host.docker.internal:41780") {
		t.Error("the trusted host carries a port, which pins it to one")
	}
}

// Loopback already is trusted, so saying so would be noise in every ordinary build — and
// noise in a generated file is how people stop reading it.
func TestLoopbackCacheIsNotMarkedTrusted(t *testing.T) {
	out, _ := rewrite(t, "FROM python:3.12-slim\n", bridge())
	if strings.Contains(out, "PIP_TRUSTED_HOST") || strings.Contains(out, "UV_INSECURE_HOST") {
		t.Errorf("a loopback cache was marked trusted:\n%s", out)
	}
}

// CacheAddress mode reaches the cache over HTTPS with its CA mounted, so there is nothing
// insecure to permit and permitting it would weaken a path that is already verified.
func TestHTTPSCacheIsNotMarkedTrusted(t *testing.T) {
	https := Options{
		Mode: CacheAddress, Registry: "cache.internal:8443",
		Base: "https://cache.internal:8443", Project: "global",
	}
	out, _ := rewrite(t, "FROM python:3.12-slim\n", https)
	if strings.Contains(out, "PIP_TRUSTED_HOST") || strings.Contains(out, "UV_INSECURE_HOST") {
		t.Errorf("an HTTPS cache was marked insecure:\n%s", out)
	}
}

// MapImage is exported for `pkgcache pull`, which needs the rewrite a FROM line gets.
// These are the cases that command meets on a command line rather than in a Dockerfile.
func TestMapImageForPull(t *testing.T) {
	const registry = "127.0.0.1:41780"
	for _, testCase := range []struct{ ref, want string }{
		// The most common image in the world, and the one whose tag colon used to read
		// as a port and leave it pointing at Docker Hub.
		{"alpine:3.20", registry + "/dockerhub/library/alpine:3.20"},
		{"alpine", registry + "/dockerhub/library/alpine"},
		// An org image is not an official one and does not gain library/.
		{"grafana/grafana:11.0.0", registry + "/dockerhub/grafana/grafana:11.0.0"},
		{"ghcr.io/astral-sh/uv:latest", registry + "/ghcr/astral-sh/uv:latest"},
		{"quay.io/oauth2-proxy/oauth2-proxy:v7", registry + "/quay/oauth2-proxy/oauth2-proxy:v7"},
		{"docker.io/library/redis:7", registry + "/dockerhub/library/redis:7"},
		// Left alone, each for its own reason: a registry this cache does not serve,
		// the empty base, and a name the author is choosing themselves.
		{"example.com/private/thing:1", ""},
		{"scratch", ""},
		{"$BASE_IMAGE", ""},
	} {
		if got := MapImage(testCase.ref, registry); got != testCase.want {
			t.Errorf("MapImage(%q) = %q, want %q", testCase.ref, got, testCase.want)
		}
	}
	// No cache address means no rewrite, rather than a reference beginning with a slash.
	if got := MapImage("alpine:3.20", ""); got != "" {
		t.Errorf("with no registry, MapImage = %q, want empty", got)
	}
}
