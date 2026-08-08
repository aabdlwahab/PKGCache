package eco

import (
	"testing"

	"github.com/brightskies/pkgreg/internal/config"
)

// chainCtx builds a Ctx with a configured chain, which is the only state these
// accessors read.
func chainCtx(t *testing.T, descriptor Descriptor, chains map[string][]config.Endpoint) *Ctx {
	t.Helper()
	snapshot := config.Defaults()
	snapshot.ProjectUpstreams = map[string]map[string]map[string][]config.Endpoint{
		"global": {descriptor.ID: chains},
	}
	return &Ctx{
		Project: "global", Eco: descriptor.ID,
		desc: descriptor, cfg: &snapshot,
	}
}

// A chain of one is what every configuration written before chains existed projects to,
// and it has to behave exactly as a single URL did — this is the compatibility claim the
// whole change rests on.
func TestChainOfOneIsTheOldBehaviour(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams:        UpstreamNamedSet,
		DefaultUpstreams: map[string]string{"root/pypi": "https://pypi.org/simple"},
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {{URL: "https://mirror.internal/simple"}},
	})
	if got, ok := c.Upstream("root/pypi"); !ok || got != "https://mirror.internal/simple" {
		t.Fatalf("Upstream = %q, %v", got, ok)
	}
	if got, ok := c.SingleUpstream(); !ok || got != "https://mirror.internal/simple" {
		t.Fatalf("SingleUpstream = %q, %v", got, ok)
	}
	if chain := c.UpstreamChain("root/pypi"); len(chain) != 1 {
		t.Fatalf("UpstreamChain = %+v, want one endpoint", chain)
	}
}

// The head of a chain is what every existing caller sees, so adding a second endpoint
// must not change where an index is normally answered from.
func TestUpstreamsReportsTheHeadOfEachChain(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams:        UpstreamNamedSet,
		DefaultUpstreams: map[string]string{"root/pypi": "https://pypi.org/simple"},
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {
			{URL: "https://cache.internal/global/pypi/root/pypi/+simple", Priority: 10},
			{URL: "https://pypi.org/simple", Priority: 20},
		},
	})
	got, ok := c.Upstream("root/pypi")
	if !ok || got != "https://cache.internal/global/pypi/root/pypi/+simple" {
		t.Fatalf("Upstream = %q, want the head of the chain", got)
	}
	chain := c.UpstreamChain("root/pypi")
	if len(chain) != 2 {
		t.Fatalf("chain = %+v, want two endpoints", chain)
	}
	if chain[0].URL != "https://cache.internal/global/pypi/root/pypi/+simple" ||
		chain[1].URL != "https://pypi.org/simple" {
		t.Fatalf("chain is in the wrong order: %+v", chain)
	}
}

// An unconfigured name still resolves through the descriptor's own defaults, as a
// chain of one.
func TestUpstreamChainFallsBackToTheDescriptorDefault(t *testing.T) {
	descriptor := Descriptor{
		ID: "npm", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams:        UpstreamSingle,
		DefaultUpstreams: map[string]string{"registry": "https://registry.npmjs.org"},
	}
	c := chainCtx(t, descriptor, nil)
	chain := c.UpstreamChain("registry")
	if len(chain) != 1 || chain[0].URL != "https://registry.npmjs.org" {
		t.Fatalf("chain = %+v", chain)
	}
	if chain := c.UpstreamChain("nonexistent"); chain != nil {
		t.Fatalf("an unknown name produced %+v", chain)
	}
}

// Go randomises map iteration deliberately, so the old implementation returned a
// different origin between requests once more than one name was configured. An
// ecosystem declaring UpstreamSingle should only ever have one — but a configuration
// that grew a second would have failed intermittently rather than visibly.
func TestSingleUpstreamIsDeterministic(t *testing.T) {
	descriptor := Descriptor{
		ID: "npm", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamSingle,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"alpha":   {{URL: "https://alpha.example"}},
		"beta":    {{URL: "https://beta.example"}},
		"gamma":   {{URL: "https://gamma.example"}},
		"delta":   {{URL: "https://delta.example"}},
		"epsilon": {{URL: "https://epsilon.example"}},
	})
	first, ok := c.SingleUpstream()
	if !ok {
		t.Fatal("SingleUpstream found nothing")
	}
	for range 50 {
		got, _ := c.SingleUpstream()
		if got != first {
			t.Fatalf("SingleUpstream returned %q then %q", first, got)
		}
	}
}

// A chain's second endpoint is a different origin with its own credential. Looking only
// at the head would send the team cache's token to the public registry behind it.
func TestCredentialFollowsTheEndpointNotTheName(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {
			{
				URL: "https://cache.internal/simple",
				Credential: config.UpstreamCredential{
					Kind: "basic", Username: "team", Password: "team-secret",
				},
			},
			{URL: "https://pypi.org/simple"},
		},
	})

	team := c.credentialForURL("https://cache.internal/simple/demo/")
	if team == nil || team.Username != "team" {
		t.Fatalf("the team endpoint's credential was not found: %+v", team)
	}
	public := c.credentialForURL("https://pypi.org/simple/demo/")
	if public != nil {
		t.Fatalf("a credential leaked to the public registry: %+v", public)
	}
}

// Longest prefix wins, so a credential for a specific path does not lose to one for the
// host it sits on.
func TestCredentialPrefersTheMoreSpecificOrigin(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"broad": {{
			URL:        "https://host.internal",
			Credential: config.UpstreamCredential{Kind: "basic", Username: "broad"},
		}},
		"narrow": {{
			URL:        "https://host.internal/private/simple",
			Credential: config.UpstreamCredential{Kind: "basic", Username: "narrow"},
		}},
	})
	got := c.credentialForURL("https://host.internal/private/simple/demo/")
	if got == nil || got.Username != "narrow" {
		t.Fatalf("credential = %+v, want the more specific one", got)
	}
}
