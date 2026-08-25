package oci

import (
	"net/http"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// Registry discovery: the first segment names the registry, so a registry nobody
// configured is still a registry. These tests pin the two halves of that — that a host
// resolves on its own, and that what a host is allowed to be is bounded.

func discoveryCtx(t *testing.T, configure func(*config.Snapshot)) *eco.Ctx {
	t.Helper()
	snapshot := config.Defaults()
	if configure != nil {
		configure(&snapshot)
	}
	request, err := http.NewRequest(http.MethodGet, "http://cache/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return eco.NewCtx(nil, request, config.GlobalProject, "", router.Params{},
		nil, &snapshot, New().Descriptor())
}

// The whole point: no configuration, no operator, no restart.
func TestAnUnconfiguredRegistryResolvesFromItsName(t *testing.T) {
	for _, tc := range []struct{ name, base, repo, display string }{
		{"nvcr.io/nvidia/pytorch", "https://nvcr.io", "nvidia/pytorch", "nvcr.io/nvidia/pytorch"},
		{"gcr.io/distroless/static", "https://gcr.io", "distroless/static", "gcr.io/distroless/static"},
		{"public.ecr.aws/lambda/python", "https://public.ecr.aws", "lambda/python", "public.ecr.aws/lambda/python"},
	} {
		route, ok := resolveName(discoveryCtx(t, nil), tc.name)
		if !ok {
			t.Errorf("%q did not resolve", tc.name)
			continue
		}
		if route.base != tc.base || route.repo != tc.repo || route.display != tc.display {
			t.Errorf("%q resolved to %+v", tc.name, route)
		}
	}
}

// A discovered registry never gains Docker Hub's library/ convention: only Docker Hub
// has one, and a single-segment repository elsewhere is a repository.
func TestOnlyDockerHubGetsTheLibraryPrefix(t *testing.T) {
	route, ok := resolveName(discoveryCtx(t, nil), "nvcr.io/cuda")
	if !ok || route.repo != "cuda" {
		t.Fatalf("route = %+v, want the repository untouched", route)
	}
}

// Docker Hub is spelled several ways and must be one cache namespace, not four. Pulling
// docker.io/library/alpine has to land on the same rows, the same blobs and the same
// inventory name as pulling dockerhub/library/alpine.
func TestEveryDockerHubSpellingFoldsOntoOneNamespace(t *testing.T) {
	for _, spelling := range []string{
		"dockerhub/library/alpine", "docker.io/library/alpine",
		"index.docker.io/library/alpine", "registry-1.docker.io/library/alpine",
	} {
		route, ok := resolveName(discoveryCtx(t, nil), spelling)
		if !ok {
			t.Errorf("%q did not resolve", spelling)
			continue
		}
		if route.alias != "dockerhub" || route.display != "dockerhub/library/alpine" ||
			route.base != "https://registry-1.docker.io" {
			t.Errorf("%q resolved to %+v", spelling, route)
		}
	}
	// And the alias keeps the convention that makes `docker pull alpine` work.
	route, _ := resolveName(discoveryCtx(t, nil), "docker.io/alpine")
	if route.repo != "library/alpine" {
		t.Fatalf("repo = %q, want library/alpine", route.repo)
	}
}

// Configuration outranks discovery, so an operator who has pointed an alias at a mirror,
// a credentialled origin or a team cache keeps it.
func TestAConfiguredUpstreamWinsOverTheHostItIsNamedFor(t *testing.T) {
	c := discoveryCtx(t, func(s *config.Snapshot) {
		s.ProjectUpstreams = map[string]map[string]map[string][]config.Endpoint{
			config.GlobalProject: {ID: {
				"dockerhub": {{URL: "https://mirror.internal/v2/dockerhub"}},
				"nvcr.io":   {{URL: "https://mirror.internal/v2/nvcr.io"}},
			}},
		}
	})
	for _, tc := range []struct{ name, base string }{
		{"docker.io/library/alpine", "https://mirror.internal/v2/dockerhub"},
		{"nvcr.io/nvidia/pytorch", "https://mirror.internal/v2/nvcr.io"},
	} {
		route, ok := resolveName(c, tc.name)
		if !ok || route.base != tc.base {
			t.Errorf("%q resolved to %+v, want the configured origin %s", tc.name, route, tc.base)
		}
	}
}

// The first path segment is chosen by whoever runs the pull, and 169.254.169.254 is a
// path segment. With no allowlist, discovery reaches public registries and nothing else.
func TestDiscoveryDoesNotReachAddressesOnlyThisHostCanRoute(t *testing.T) {
	for _, name := range []string{
		"169.254.169.254/latest/meta-data",
		"127.0.0.1/v2/secrets",
		"10.4.0.9:5000/team/app",
		"localhost:5000/team/app",
		"localhost/team/app",
		"registry.internal:5000/team/app",
	} {
		if route, ok := resolveName(discoveryCtx(t, nil), name); ok {
			t.Errorf("%q resolved to %+v", name, route)
		}
	}
}

// A non-empty allowlist is exhaustive: it is how discovery is narrowed to the registries
// an organisation permits, and how a private one is admitted.
func TestTheAllowlistNarrowsAndWidensDiscovery(t *testing.T) {
	narrowed := discoveryCtx(t, func(s *config.Snapshot) {
		s.Server.RegistryAllowlist = []string{"nvcr.io", "*.internal", "registry.local:5000"}
	})
	for _, name := range []string{"nvcr.io/nvidia/pytorch", "registry.local:5000/team/app", "hub.internal/team/app"} {
		if _, ok := resolveName(narrowed, name); !ok {
			t.Errorf("%q was refused by an allowlist that names it", name)
		}
	}
	for _, name := range []string{"gcr.io/distroless/static", "hub.example.com/team/app"} {
		if _, ok := resolveName(narrowed, name); ok {
			t.Errorf("%q resolved through an allowlist that does not name it", name)
		}
	}

	// "*" is the deliberate "anywhere", the same opt-in the apt relay allowlist uses,
	// and is what a single-developer cache on loopback runs with.
	anywhere := discoveryCtx(t, func(s *config.Snapshot) {
		s.Server.RegistryAllowlist = []string{config.ProxyRelaysAnywhere}
	})
	if _, ok := resolveName(anywhere, "localhost:5000/team/app"); !ok {
		t.Fatal(`a registry was refused under ["*"]`)
	}
}

// A segment that names a registry is a registry, even when it resolves to nothing. The
// mirror must not turn a refused registry into a repository on Docker Hub, and a bare
// registry with no repository after it is not a pull.
func TestARegistrySegmentIsNeverReadAsARepository(t *testing.T) {
	mirrored := discoveryCtx(t, func(s *config.Snapshot) { s.Server.RegistryMirror = "dockerhub" })
	for _, name := range []string{"169.254.169.254/latest/meta-data", "localhost:5000/team/app"} {
		if route, ok := resolveName(mirrored, name); ok {
			t.Errorf("%q fell through to the mirror as %+v", name, route)
		}
	}
	if _, ok := resolveName(mirrored, "nvcr.io"); ok {
		t.Error("a registry with no repository after it resolved")
	}
	// Discovery still happens with the mirror on: a path that names a registry means
	// that registry, and only a path that names none is the mirror's.
	route, ok := resolveName(mirrored, "nvcr.io/nvidia/pytorch")
	if !ok || route.base != "https://nvcr.io" {
		t.Fatalf("route = %+v, want nvcr.io", route)
	}
}

// The wildcard is the name a chain files its discovery rule under, not a registry.
func TestTheWildcardCannotBePulledFrom(t *testing.T) {
	c := discoveryCtx(t, func(s *config.Snapshot) {
		s.ProjectUpstreams = map[string]map[string]map[string][]config.Endpoint{
			config.GlobalProject: {ID: {ociname.AnyRegistry: {{URL: "https://team:8443/v2"}}}},
		}
	})
	if route, ok := resolveName(c, ociname.AnyRegistry+"/team/app"); ok {
		t.Fatalf("the wildcard resolved as a registry: %+v", route)
	}
}

// A cache that sits behind another one must not answer discovery by going around it.
// This is what makes `pkgcache setup -no-direct` hold for a registry nobody named.
func TestDiscoveryResolvesThroughTheCacheInFront(t *testing.T) {
	c := discoveryCtx(t, func(s *config.Snapshot) {
		s.ProjectUpstreams = map[string]map[string]map[string][]config.Endpoint{
			config.GlobalProject: {ID: {ociname.AnyRegistry: {{URL: "https://team:8443/v2"}}}},
		}
	})
	route, ok := resolveName(c, "nvcr.io/nvidia/pytorch")
	if !ok {
		t.Fatal("a discovered registry did not resolve with a chain configured")
	}
	if route.base != "https://team:8443/v2/nvcr.io" {
		t.Fatalf("base = %q, want the team cache's path for the registry", route.base)
	}
	// And the composed URL keeps the API root it already names rather than gaining a
	// second one.
	if got := manifestURL(route, "24.01"); got !=
		"https://team:8443/v2/nvcr.io/nvidia/pytorch/manifests/24.01" {
		t.Fatalf("manifestURL = %q", got)
	}
}
