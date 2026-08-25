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

// Alpine's package index. The build proxy is plain HTTP, and every Alpine image since 3.x
// ships https:// in /etc/apk/repositories, so apk went straight past the cache — silently,
// with the build succeeding and nothing stored.
func TestApkRepositoriesAreRewrittenForTheBuildAndPutBack(t *testing.T) {
	result, err := Rewrite([]byte("FROM alpine:3.20\nRUN apk add --no-cache curl\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41780",
		Registry: "127.0.0.1:41780", AptProxy: "http://127.0.0.1:41780",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Content)
	if !strings.Contains(body, "sed -i 's|^https://|http://|' /etc/apk/repositories") {
		t.Errorf("apk was left on https, so the proxy never sees it:\n%s", body)
	}
	// Restored, and restored from a copy: leaving http in the shipped image would change
	// what `apk add` does for everybody who later runs the container.
	if !strings.Contains(body, "mv "+apkBackup+" /etc/apk/repositories") {
		t.Errorf("the repositories file is never put back:\n%s", body)
	}
	if strings.Index(body, "apk add") > strings.Index(body, "mv "+apkBackup) {
		t.Error("the restore runs before the build's own apk step")
	}
}

func TestApkRewriteIsSkippedWithoutAProxy(t *testing.T) {
	// Nothing to send apk to, so the layers would buy nothing and still cost two.
	result, err := Rewrite([]byte("FROM alpine:3.20\nRUN apk add curl\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41780",
		Registry: "127.0.0.1:41780", Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Content), "/etc/apk/repositories") {
		t.Errorf("apk was rewritten with no proxy configured:\n%s", result.Content)
	}
}

// Every stage gets its own pair, and no stage ends still rewritten — ARG is stage-scoped
// and so is a file edit, and a multi-stage build that restored only once would ship the
// change in whichever stage happened to be last.
func TestApkRewriteIsPerStage(t *testing.T) {
	source := "FROM alpine:3.20 AS build\nRUN apk add curl\n\nFROM alpine:3.20\nRUN apk add git\n"
	result, err := Rewrite([]byte(source), Options{
		Project: "global", Base: "http://127.0.0.1:41780",
		Registry: "127.0.0.1:41780", AptProxy: "http://127.0.0.1:41780",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Content)
	if got := strings.Count(body, "sed -i 's|^https://|http://|'"); got != 2 {
		t.Errorf("rewrite appears %d times, want one per stage", got)
	}
	if got := strings.Count(body, "mv "+apkBackup); got != 2 {
		t.Errorf("restore appears %d times, want one per stage", got)
	}
	// The last thing in the file is a restore, not a rewrite.
	if strings.LastIndex(body, "sed -i") > strings.LastIndex(body, "mv "+apkBackup) {
		t.Error("the final stage ships with its repositories still rewritten")
	}
}

// A Debian image has no /etc/apk/repositories, and the guard is what keeps the injected
// layers from failing the build there.
func TestApkRewriteIsGuardedForImagesWithoutApk(t *testing.T) {
	result, err := Rewrite([]byte("FROM debian:bookworm-slim\nRUN apt-get update\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41780",
		Registry: "127.0.0.1:41780", AptProxy: "http://127.0.0.1:41780",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Content)
	if !strings.Contains(body, "if [ -f /etc/apk/repositories ]") {
		t.Errorf("the rewrite is unguarded and would fail on an image without apk:\n%s", body)
	}
}

// A base image that already exists on this machine is left alone, because it may have no
// upstream at all.
//
// This is crate's prebuilds: a shared base built from a Dockerfile in the tree, which
// every service then builds FROM. `FROM mold:latest` looks exactly like a Docker Hub
// reference and is nothing of the kind — it was rewritten to dockerhub/library/mold, and
// every service in the manifest failed on an image that has never been published.
func TestLocalBaseImageIsNotRewritten(t *testing.T) {
	local := map[string]bool{"mold:latest": true}
	options := Options{
		Project: "global", Base: "http://127.0.0.1:41780", Registry: "127.0.0.1:41780",
		Mode:       Bridge,
		LocalImage: func(ref string) bool { return local[ref] },
	}

	result, err := Rewrite([]byte("FROM mold:latest\nRUN true\n"), options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "FROM mold:latest") {
		t.Errorf("a locally built base was rewritten:\n%s", result.Content)
	}
	if len(result.Changes) != 0 {
		t.Errorf("a skipped FROM was reported as a change: %+v", result.Changes)
	}

	// And the other direction, or the check would be a way to disable the rewrite.
	result, err = Rewrite([]byte("FROM alpine:3.20\nRUN true\n"), options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "127.0.0.1:41780/dockerhub/library/alpine:3.20") {
		t.Errorf("an image that is not local should still go through the cache:\n%s",
			result.Content)
	}
}

func TestLocalImageCheckIsOptional(t *testing.T) {
	// Nil means "assume nothing is local", which is what a caller with no cheap way to
	// ask should get — and what every caller got before the check existed.
	result, err := Rewrite([]byte("FROM mold:latest\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41780", Registry: "127.0.0.1:41780",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "dockerhub/library/mold") {
		t.Errorf("with no LocalImage set the rewrite should be unchanged:\n%s", result.Content)
	}
}

// A variable is only disqualifying where it lands.
//
// The rule was "any $ means leave it alone", which is right for `FROM ${BASE_IMAGE}` and
// wrong for `FROM nvidia/cuda:${CUDA_VER}-runtime` — a very common shape, and in one real
// manifest the shape of every CUDA image in the build. Multi-gigabyte pulls went straight
// past the cache while everything around them was served from it.
func TestVariableInTheTagStillGoesThroughTheCache(t *testing.T) {
	const registry = "127.0.0.1:41780"
	for _, testCase := range []struct{ ref, want string }{
		{"nvidia/cuda:${CUDA_VER}-cudnn-runtime-ubuntu24.04",
			registry + "/dockerhub/nvidia/cuda:${CUDA_VER}-cudnn-runtime-ubuntu24.04"},
		{"python:${PY_VERSION}", registry + "/dockerhub/library/python:${PY_VERSION}"},
		{"ghcr.io/org/tool:${TAG}", registry + "/ghcr/org/tool:${TAG}"},
		// A digest is carried through the same way.
		{"alpine@sha256:abc", registry + "/dockerhub/library/alpine@sha256:abc"},
	} {
		if got := MapImage(testCase.ref, registry); got != testCase.want {
			t.Errorf("MapImage(%q) = %q, want %q", testCase.ref, got, testCase.want)
		}
	}
}

func TestVariableInTheNameIsStillLeftAlone(t *testing.T) {
	// There is no way to know which repository these become, so prefixing them would
	// produce a reference to something that may not exist.
	for _, ref := range []string{
		"${BASE_IMAGE}",
		"${REGISTRY}/app:1",
		"myorg/${APP}:1",
	} {
		if got := MapImage(ref, "127.0.0.1:41780"); got != "" {
			t.Errorf("MapImage(%q) = %q, want it left alone", ref, got)
		}
	}
}

// The last colon is a tag separator only after the last slash. In "registry:5000/image"
// it is a port, and cutting there would invent a repository.
func TestSplitTagKnowsAPortFromATag(t *testing.T) {
	for _, testCase := range []struct{ ref, name, tag string }{
		{"alpine:3.20", "alpine", ":3.20"},
		{"registry:5000/image", "registry:5000/image", ""},
		{"registry:5000/image:2", "registry:5000/image", ":2"},
		{"alpine@sha256:abc", "alpine", "@sha256:abc"},
		{"alpine", "alpine", ""},
	} {
		name, tag := splitTag(testCase.ref)
		if name != testCase.name || tag != testCase.tag {
			t.Errorf("splitTag(%q) = %q, %q; want %q, %q",
				testCase.ref, name, tag, testCase.name, testCase.tag)
		}
	}
}

// An index the Dockerfile names itself. PIP_INDEX_URL covers the default index and
// nothing else, so `--extra-index-url https://download.pytorch.org/whl/cu130` named a
// second one that went straight past the cache — several gigabytes for a CUDA torch
// wheel, the largest single download in that build.
func TestDirectlyNamedIndexesArePointedAtTheCache(t *testing.T) {
	options := Options{
		Project: "global", Base: "http://127.0.0.1:41777", Registry: "127.0.0.1:41777",
		Mode: Bridge,
		Indexes: map[string]string{
			"https://download.pytorch.org/whl/cu130": "root/pytorch-cu130",
			"https://pypi.org/simple":                "root/pypi",
		},
	}
	source := "FROM python:3.12-slim\n" +
		"ARG TORCH_CUDA_INDEX=https://download.pytorch.org/whl/cu130\n" +
		"RUN uv pip install --extra-index-url \"${TORCH_CUDA_INDEX}\" torch\n"

	result, err := Rewrite([]byte(source), options)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Content)
	if strings.Contains(body, "https://download.pytorch.org") {
		t.Errorf("the pytorch index still points upstream:\n%s", body)
	}
	if !strings.Contains(body,
		"http://127.0.0.1:41777/global/pypi/root/pytorch-cu130/+simple") {
		t.Errorf("the pytorch index was not pointed at the cache:\n%s", body)
	}
	// Reported, because a tool that silently alters what gets built is one people stop
	// trusting — the same rule the FROM rewrite follows.
	var reported bool
	for _, change := range result.Changes {
		if strings.Contains(change.From, "download.pytorch.org") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the substitution was not reported: %+v", result.Changes)
	}
}

func TestUnknownIndexesAreLeftUpstream(t *testing.T) {
	// Rewriting an index the cache does not serve would point the build at a path that
	// 404s, which is worse than letting it go upstream.
	result, err := Rewrite([]byte(
		"FROM python:3.12-slim\nRUN pip install --extra-index-url https://wheels.example.com/x y\n"),
		Options{
			Project: "global", Base: "http://127.0.0.1:41777", Registry: "127.0.0.1:41777",
			Mode:    Bridge,
			Indexes: map[string]string{"https://download.pytorch.org/whl/cu130": "root/pytorch-cu130"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "https://wheels.example.com/x") {
		t.Errorf("an index the cache does not serve was rewritten:\n%s", result.Content)
	}
}

// An origin that is a prefix of another must not claim its URL.
func TestLongerIndexOriginWins(t *testing.T) {
	result, err := Rewrite([]byte(
		"FROM python:3.12-slim\nARG I=https://download.pytorch.org/whl/cu130\n"),
		Options{
			Project: "global", Base: "http://c", Registry: "c", Mode: Bridge,
			Indexes: map[string]string{
				"https://download.pytorch.org/whl":       "root/pytorch",
				"https://download.pytorch.org/whl/cu130": "root/pytorch-cu130",
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "root/pytorch-cu130/+simple") {
		t.Errorf("the shorter origin claimed the URL:\n%s", result.Content)
	}
}

// A cold cache is slower than the CDN the client defaults were chosen for. uv allows a
// request 30 seconds; a 366 MB CUDA wheel took 142 through this cache on first fetch and
// 0.1 on the next. At the default the first build times out, retries ten times, and
// reports a server error for a cache that was working correctly.
func TestClientTimeoutsAllowForAColdCache(t *testing.T) {
	result, err := Rewrite([]byte("FROM python:3.12-slim\nRUN pip install x\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41780", Registry: "127.0.0.1:41780",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Content)
	for _, want := range []string{"ARG UV_HTTP_TIMEOUT=", "ARG PIP_TIMEOUT="} {
		if !strings.Contains(body, want) {
			t.Errorf("%s is missing, so a slow first fetch fails:\n%s", want, body)
		}
	}
	// Bounded, not disabled: a request that hangs forever is its own kind of failure.
	if strings.Contains(body, "TIMEOUT=0") {
		t.Error("the timeout was disabled rather than raised")
	}
}

// TestApkRestoreRunsBeforePrivilegesAreDropped is the bug a user hit building a Node
// service: the restore was appended at the end of the stage, which is after `USER node`,
// so `mv` ran unprivileged and the build died with
//
//	mv: can't rename '/etc/apk/repositories.pkgcache': Permission denied
//
// on a Dockerfile that had nothing to do with apk. The cache borrowed a file and has to
// hand it back while it still can.
func TestApkRestoreRunsBeforePrivilegesAreDropped(t *testing.T) {
	const source = `FROM node:22-alpine AS runtime
WORKDIR /app
RUN npm ci --omit=dev && npm cache clean --force
RUN mkdir -p /app/data && chown node:node /app/data
USER node
EXPOSE 7000
CMD ["node", "dist/server.js"]
`
	body, _ := rewrite(t, source, bridge())

	restore := strings.Index(body, "mv "+apkBackup)
	user := strings.Index(body, "USER node")
	if restore < 0 {
		t.Fatalf("the repositories file is never put back:\n%s", body)
	}
	if user < 0 {
		t.Fatalf("the USER line went missing:\n%s", body)
	}
	if restore > user {
		t.Errorf("the restore runs after privileges are dropped, so it cannot write:\n%s", body)
	}
	// It must still come after the work that needed the rewritten repositories.
	if setup := strings.Index(body, "sed -i 's|^https://|http://|'"); setup > restore {
		t.Errorf("the restore runs before the rewrite it undoes:\n%s", body)
	}
}

func TestApkRestoreStaysAtTheEndWhenRootIsKept(t *testing.T) {
	// USER root is not a privilege drop, and a stage that never switches keeps the
	// restore last so everything before it still sees the rewritten repositories.
	const source = `FROM alpine:3.20
USER root
RUN apk add --no-cache curl
`
	body, _ := rewrite(t, source, bridge())
	if strings.Index(body, "apk add") > strings.Index(body, "mv "+apkBackup) {
		t.Errorf("the restore ran before apk could use the rewrite:\n%s", body)
	}
}

func TestApkRestorePlacedPerStage(t *testing.T) {
	// A builder that drops privileges and a runtime that does not: each stage has to be
	// closed on its own terms, since a stage's file is its own.
	const source = `FROM node:22-alpine AS builder
RUN npm ci
USER node
RUN npm run build

FROM node:22-alpine AS runtime
RUN apk add --no-cache nginx
CMD ["nginx"]
`
	body, _ := rewrite(t, source, bridge())
	if got := strings.Count(body, "mv "+apkBackup); got != 2 {
		t.Fatalf("want one restore per stage, got %d:\n%s", got, body)
	}
	lines := strings.Split(body, "\n")
	var restores, userNode, apkAdd int
	for i, line := range lines {
		switch {
		case strings.Contains(line, "mv "+apkBackup) && restores == 0:
			restores = i
		case strings.Contains(line, "USER node"):
			userNode = i
		case strings.Contains(line, "apk add"):
			apkAdd = i
		}
	}
	if restores > userNode {
		t.Errorf("the builder's restore is after its USER switch:\n%s", body)
	}
	if apkAdd < restores {
		t.Errorf("the runtime's apk add landed in the wrong stage:\n%s", body)
	}
}

func TestApkSetupCannotFailABuildItCannotHelp(t *testing.T) {
	// Where /etc/apk is not writable the copy fails, and the whole thing has to become a
	// no-op rather than an error: not caching apk is a cost, breaking the build is not.
	body, _ := rewrite(t, "FROM node:22-alpine\nRUN npm ci\n", bridge())
	if !strings.Contains(body, "cp /etc/apk/repositories "+apkBackup+" 2>/dev/null; then") {
		t.Errorf("the setup is not guarded on the copy succeeding:\n%s", body)
	}
}
