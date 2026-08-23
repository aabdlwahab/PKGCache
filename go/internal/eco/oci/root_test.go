package oci

import "testing"

// An origin may be written either way, and both have to keep working.
//
// Every OCI origin that predates team chaining is bare — "https://ghcr.io" — because the
// distribution spec fixes /v2 as the API root and this adapter appended it. A team cache
// fronts several registries, so the origin that reaches one of them has to name the
// registry too, which means naming /v2 as well. Appending a second one would address
// nothing.
func TestRepoRootAppendsTheAPIRootOnlyWhenItIsAbsent(t *testing.T) {
	for _, tc := range []struct{ name, base, want string }{
		{"bare origin", "https://registry-1.docker.io", "https://registry-1.docker.io/v2"},
		{"trailing slash", "https://ghcr.io/", "https://ghcr.io/v2"},
		{"already a root", "https://quay.io/v2", "https://quay.io/v2"},
		{"team, global project", "https://cache:8443/v2/dockerhub", "https://cache:8443/v2/dockerhub"},
		{"team, named project", "https://cache:8443/v2/acme/ghcr", "https://cache:8443/v2/acme/ghcr"},
		// "v2" has to be a path segment, not a substring: a host or a directory that
		// merely contains those characters is not an API root.
		{"v2 in the host", "https://v2.example.com", "https://v2.example.com/v2"},
		{"v2 inside a segment", "https://example.com/av2", "https://example.com/av2/v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoRoot(tc.base); got != tc.want {
				t.Fatalf("repoRoot(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}
