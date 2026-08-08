package oci

import (
	"net/http"
	"testing"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/router"
)

// Registry-mirror mode: a daemon configured with registry-mirrors asks for
// /v2/library/alpine, knowing nothing of this cache's per-upstream namespace. These
// tests pin the fallback that reconciles the two, and — more importantly — pin that
// enabling it cannot change the meaning of a path that already worked.

func mirrorCtx(t *testing.T, mirror string) *eco.Ctx {
	t.Helper()
	snapshot := config.Defaults()
	snapshot.Server.RegistryMirror = mirror
	request, err := http.NewRequest(http.MethodGet, "http://cache/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return eco.NewCtx(nil, request, config.GlobalProject, "", router.Params{},
		nil, &snapshot, New().Descriptor())
}

func TestNamespacedPathStillWinsWhenTheMirrorIsOn(t *testing.T) {
	c, name := mirrorCtx(t, "dockerhub"), "ghcr/astral-sh/uv"
	route, ok := resolveName(c, name)
	if !ok {
		t.Fatal("an explicit upstream stopped resolving once the mirror was enabled")
	}
	if route.alias != "ghcr" || route.repo != "astral-sh/uv" {
		t.Fatalf("route = %+v, want the ghcr upstream", route)
	}
}

func TestBareRepositoryResolvesThroughTheMirror(t *testing.T) {
	c, name := mirrorCtx(t, "dockerhub"), "library/alpine"
	route, ok := resolveName(c, name)
	if !ok {
		t.Fatal("mirror-style path was refused")
	}
	if route.alias != "dockerhub" || route.repo != "library/alpine" {
		t.Fatalf("route = %+v", route)
	}
}

// A daemon asking for an official image sends a single segment.
func TestSingleSegmentRepositoryGetsTheLibraryPrefix(t *testing.T) {
	c, name := mirrorCtx(t, "dockerhub"), "alpine"
	route, ok := resolveName(c, name)
	if !ok {
		t.Fatal("single-segment repository was refused")
	}
	if route.repo != "library/alpine" {
		t.Fatalf("repo = %q, want library/alpine", route.repo)
	}
}

// The default. An instance that has not opted in must not start answering for
// repositories it was never asked about.
func TestWithoutTheMirrorAnUnknownPrefixIsStillRefused(t *testing.T) {
	c, name := mirrorCtx(t, ""), "library/alpine"
	if _, ok := resolveName(c, name); ok {
		t.Fatal("an unprefixed repository resolved with no mirror configured")
	}
}

func TestAMirrorNamingAnUnknownUpstreamResolvesNothing(t *testing.T) {
	c, name := mirrorCtx(t, "not-a-registry"), "library/alpine"
	if _, ok := resolveName(c, name); ok {
		t.Fatal("resolved against an upstream that does not exist")
	}
}

func TestTraversalIsRefusedInMirrorMode(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "library/../../x", ""} {
		c := mirrorCtx(t, "dockerhub")
		if _, ok := resolveName(c, name); ok {
			t.Errorf("%q resolved", name)
		}
	}
}

// An upstream alias with no repository after it is not a repository.
func TestBareUpstreamAliasDoesNotResolve(t *testing.T) {
	c := mirrorCtx(t, "")
	if _, ok := resolveName(c, "dockerhub"); ok {
		t.Fatal("a bare upstream alias resolved as a repository")
	}
}
