package ociname

import "testing"

// The rule, stated as cases: a first segment that names a host is a registry, and one
// that does not is the first component of a Docker Hub repository. This is the same
// division Docker's own reference parser makes, and the whole of what discovery rests
// on.
func TestLookupSeparatesRegistriesFromRepositories(t *testing.T) {
	for _, tc := range []struct {
		segment string
		want    Registry
		ok      bool
	}{
		// Discovered: nobody configured any of these, and all of them work.
		{"nvcr.io", Registry{Segment: "nvcr.io", Host: "nvcr.io", Public: true}, true},
		{"gcr.io", Registry{Segment: "gcr.io", Host: "gcr.io", Public: true}, true},
		{"public.ecr.aws", Registry{Segment: "public.ecr.aws", Host: "public.ecr.aws", Public: true}, true},
		{"registry.k8s.io", Registry{Segment: "registry.k8s.io", Host: "registry.k8s.io", Public: true}, true},
		// Case is not part of a host.
		{"MCR.Microsoft.com", Registry{Segment: "mcr.microsoft.com", Host: "mcr.microsoft.com", Public: true}, true},

		// Named before discovery existed, and still spelled that way in Dockerfiles and
		// in this cache's own documentation.
		{"dockerhub", hub, true},
		{"docker.io", hub, true},
		{"index.docker.io", hub, true},
		{"ghcr.io", Registry{Segment: "ghcr", Host: "ghcr.io", Public: true}, true},
		{"quay", Registry{Segment: "quay", Host: "quay.io", Public: true}, true},

		// A registry, but not one this cache may reach on a segment alone: the string
		// means something different here than it does to the caller.
		{"localhost:5000", Registry{Segment: "localhost:5000", Host: "localhost:5000"}, true},
		{"localhost", Registry{Segment: "localhost", Host: "localhost"}, true},
		{"registry.internal:5000", Registry{Segment: "registry.internal:5000", Host: "registry.internal:5000"}, true},
		{"169.254.169.254", Registry{Segment: "169.254.169.254", Host: "169.254.169.254"}, true},
		{"10.4.0.9:5000", Registry{Segment: "10.4.0.9:5000", Host: "10.4.0.9:5000"}, true},

		// Repositories. "nvidia/cuda" is a Docker Hub image, not the nvidia registry.
		{"nvidia", Registry{}, false},
		{"library", Registry{}, false},
		{"astral-sh", Registry{}, false},
		{"", Registry{}, false},

		// Nothing that could carry more than a host into a URL.
		{"user@evil.example", Registry{}, false},
		{"evil.example:80@other.example", Registry{}, false},
		{"nvcr.io:notaport", Registry{}, false},
		{"nvcr.io:0", Registry{}, false},
		{"nvcr..io", Registry{}, false},
		{"-nvcr.io", Registry{}, false},
		{".nvcr.io", Registry{}, false},
		{"nvcr.io/../etc", Registry{}, false},
		{"[::1]:5000", Registry{}, false},
	} {
		got, ok := Lookup(tc.segment)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Lookup(%q) = %+v, %v; want %+v, %v", tc.segment, got, ok, tc.want, tc.ok)
		}
	}
}

// A discovered registry is fetched over TLS, always. The segment was chosen by whoever
// ran the pull, and a path segment must not be able to pick plaintext.
func TestOriginIsAlwaysHTTPS(t *testing.T) {
	reg, _ := Lookup("nvcr.io")
	if got := reg.Origin(); got != "https://nvcr.io" {
		t.Fatalf("Origin() = %q", got)
	}
	docker, _ := Lookup("docker.io")
	if got := docker.Origin(); got != "https://registry-1.docker.io" {
		t.Fatalf("Docker Hub Origin() = %q, want the pull endpoint", got)
	}
}

// Docker Hub is the only registry with a library/ convention, and the one segment that
// has to keep it after every spelling folds onto it.
func TestOnlyDockerHubCarriesTheLibraryConvention(t *testing.T) {
	for _, spelling := range []string{"dockerhub", "docker.io", "registry.hub.docker.com"} {
		reg, ok := Lookup(spelling)
		if !ok || !reg.Library || reg.Segment != "dockerhub" {
			t.Errorf("Lookup(%q) = %+v, want the dockerhub segment with library/", spelling, reg)
		}
	}
	for _, other := range []string{"ghcr.io", "quay.io", "nvcr.io"} {
		if reg, _ := Lookup(other); reg.Library {
			t.Errorf("%q claims Docker Hub's library/ convention", other)
		}
	}
}

// The wildcard names a chain's discovery rule, and a client must never be able to ask
// for it as though it were a registry.
func TestTheWildcardIsNotARegistry(t *testing.T) {
	if _, ok := Lookup(AnyRegistry); ok {
		t.Fatal("the wildcard resolved as a registry")
	}
}
