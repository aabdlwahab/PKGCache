package router

import (
	"net/http"
	"testing"
)

// The formatter and the parser are in this package together so they cannot drift: what
// ProxyURLFor writes, ResolveProxy has to read back as the same project.
func TestProxyURLRoundTrips(t *testing.T) {
	known := func(name string) bool { return name == "work" }

	proxy := ProxyURLFor("http://127.0.0.1:3142", "work")
	if proxy != "http://work@127.0.0.1:3142" {
		t.Fatalf("formatted %q", proxy)
	}
	request, err := http.NewRequest(http.MethodGet, "http://deb.debian.org/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	// What a client does with a proxy URL carrying a username.
	request.SetBasicAuth("work", "")
	request.Header.Set("Proxy-Authorization", request.Header.Get("Authorization"))
	if target := ResolveProxy(request, "apt", known); target.Project != "work" {
		t.Fatalf("read back %q, want work", target.Project)
	}
}

// Idempotent, because a shell exports the scoped URL and a build inside that shell reads
// the variable back. Scoping it twice would produce a username nobody registered, which
// resolves to global — the exact failure this is meant to end.
func TestProxyURLForIsIdempotent(t *testing.T) {
	once := ProxyURLFor("http://127.0.0.1:3142", "work")
	if twice := ProxyURLFor(once, "work"); twice != once {
		t.Fatalf("second application changed it: %q -> %q", once, twice)
	}
	// And a URL somebody already gave credentials to is not ours to rewrite.
	given := "http://someone:secret@proxy.internal:3142"
	if got := ProxyURLFor(given, "work"); got != given {
		t.Fatalf("overwrote existing userinfo: %q", got)
	}
}

// Global is the absence of a label. ResolveProxy ignores a username of "global", so
// writing one would be noise that reads as a tenant and is not.
func TestGlobalGetsNoLabel(t *testing.T) {
	if got := ProxyURLFor("http://127.0.0.1:3142", GlobalProject); got != "http://127.0.0.1:3142" {
		t.Fatalf("global gained a username: %q", got)
	}
	if got := OCIProjectPrefix(GlobalProject); got != "" {
		t.Fatalf("global gained a path segment: %q", got)
	}
}

// The OCI prefix has to be what ResolveOCI strips back off, or a pull resolves to a
// repository nobody has.
func TestOCIPrefixRoundTrips(t *testing.T) {
	known := func(name string) bool { return name == "work" }
	path := "/v2/" + OCIProjectPrefix("work") + "dockerhub/library/alpine/manifests/3.20"

	target, ok := ResolveOCI(path, "oci", known)
	if !ok {
		t.Fatal("the scoped path did not resolve")
	}
	if target.Project != "work" {
		t.Fatalf("project = %q, want work", target.Project)
	}
	if target.Path != "/v2/dockerhub/library/alpine/manifests/3.20" {
		t.Fatalf("the adapter was handed %q, which still carries the project", target.Path)
	}
}
