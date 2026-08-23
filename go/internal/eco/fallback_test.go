package eco

import (
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// The six adapters know nothing about chains. Each builds a URL as origin + an
// ecosystem-specific path, so an alternate origin for the same path is that URL with its
// prefix swapped — which is why the fallbacks are derived here rather than declared in
// each adapter, where they would be six copies that could disagree.
func TestFallbacksAreDerivedFromTheComposedURL(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {
			{URL: "https://cache.internal/global/pypi/root/pypi/+simple"},
			{URL: "https://pypi.org/simple"},
		},
	})

	// What an adapter would compose: the head of the chain plus its own path.
	composed := "https://cache.internal/global/pypi/root/pypi/+simple/demo-pkg/"
	request := c.UpstreamRequest(composed, nil)

	if request.URL != composed {
		t.Fatalf("URL = %q, want it untouched", request.URL)
	}
	if len(request.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want one", request.Fallbacks)
	}
	want := "https://pypi.org/simple/demo-pkg/"
	if request.Fallbacks[0].URL != want {
		t.Fatalf("fallback = %q, want %q", request.Fallbacks[0].URL, want)
	}
}

// A chain of one produces no fallbacks at all, which is what makes every configuration
// that predates chains behave exactly as it did.
func TestChainOfOneProducesNoFallbacks(t *testing.T) {
	descriptor := Descriptor{
		ID: "npm", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams:        UpstreamSingle,
		DefaultUpstreams: map[string]string{"registry": "https://registry.npmjs.org"},
	}
	c := chainCtx(t, descriptor, nil)
	request := c.UpstreamRequest("https://registry.npmjs.org/left-pad", nil)
	if len(request.Fallbacks) != 0 {
		t.Fatalf("fallbacks = %+v, want none", request.Fallbacks)
	}
}

// A URL composed from an origin nothing is configured for gets no fallbacks: the OCI
// token endpoint and a redirect target are reached this way, and inventing alternates
// for them would send a token request to a package index.
func TestUnrelatedURLsGetNoFallbacks(t *testing.T) {
	descriptor := Descriptor{
		ID: "oci", Storage: StorageBlob, Listener: ListenerProtocolRooted,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"dockerhub": {
			{URL: "https://cache.internal/v2"},
			{URL: "https://registry-1.docker.io/v2"},
		},
	})
	request := c.UpstreamRequest("https://auth.docker.io/token?scope=repository:x", nil)
	if len(request.Fallbacks) != 0 {
		t.Fatalf("an unrelated URL gained fallbacks: %+v", request.Fallbacks)
	}
}

// Longest prefix wins when picking which chain a URL came from, for the same reason it
// does when picking a credential.
func TestFallbacksPreferTheMoreSpecificChain(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"broad": {
			{URL: "https://host.internal"},
			{URL: "https://broad-fallback.example"},
		},
		"narrow": {
			{URL: "https://host.internal/private/simple"},
			{URL: "https://narrow-fallback.example"},
		},
	})
	request := c.UpstreamRequest("https://host.internal/private/simple/demo/", nil)
	if len(request.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v", request.Fallbacks)
	}
	want := "https://narrow-fallback.example/demo/"
	if request.Fallbacks[0].URL != want {
		t.Fatalf("fallback = %q, want %q", request.Fallbacks[0].URL, want)
	}
}

// The fallback carries its own origin's credential, not the head's.
func TestFallbackCarriesItsOwnCredential(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {
			{
				URL:        "https://cache.internal/simple",
				Credential: config.UpstreamCredential{Kind: "basic", Username: "team"},
			},
			{URL: "https://pypi.org/simple"},
		},
	})
	request := c.UpstreamRequest("https://cache.internal/simple/demo/", nil)
	if request.Credential == nil || request.Credential.Username != "team" {
		t.Fatalf("head credential = %+v", request.Credential)
	}
	if len(request.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v", request.Fallbacks)
	}
	if request.Fallbacks[0].Credential != nil {
		t.Fatalf("the team credential leaked to the public registry: %+v",
			request.Fallbacks[0].Credential)
	}
}

// OCI is the one ecosystem whose origins have to be written as repository roots — with
// their /v2 — and this is why.
//
// A fallback is the head's URL with its prefix swapped for a later endpoint's, over an
// untouched suffix. That only reconstructs a valid URL when both endpoints are roots of
// the same shape. A team cache fronts several registries, so its root names both the API
// root and the registry (/v2/dockerhub); if the public endpoint beside it were written
// bare as "https://registry-1.docker.io", the swap would produce a URL with no /v2 in it
// at all and every fallback pull would 404 against the real registry.
func TestOCIFallbackKeepsTheDistributionAPIRoot(t *testing.T) {
	descriptor := Descriptor{
		ID: "oci", Storage: StorageBlob, Listener: ListenerProtocolRooted,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"dockerhub": {
			{URL: "https://cache.internal:8443/v2/dockerhub"},
			{URL: "https://registry-1.docker.io/v2"},
		},
	})

	// What the adapter composes: the head, then the repository and the reference.
	composed := "https://cache.internal:8443/v2/dockerhub/library/alpine/manifests/3.20"
	request := c.UpstreamRequest(composed, nil)

	if len(request.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want one", request.Fallbacks)
	}
	want := "https://registry-1.docker.io/v2/library/alpine/manifests/3.20"
	if request.Fallbacks[0].URL != want {
		t.Fatalf("fallback = %q, want %q", request.Fallbacks[0].URL, want)
	}
}
