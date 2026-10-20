package eco

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// This machine's team token is for this machine's requests. A sibling that asks for
// something this cache does not hold gets no token spent on its behalf, on either path a
// credential leaves by: a chain row's own, or the team's data-plane hop.
func TestSiblingRequestIsFetchedWithNoCredential(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed, Upstreams: UpstreamNamedSet,
	}
	token := config.UpstreamCredential{Kind: "bearer", Token: "team-token"}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {{URL: "https://team.internal:8443/work/pypi/root/pypi/+simple", Credential: token}},
	})
	c.cfg.Local.Relays = map[string]config.Relay{
		config.GlobalProject: {Hops: []config.Hop{{Base: "https://team.internal:8443/work", Token: "team-token"}}, Direct: true},
	}
	file := "https://team.internal:8443/work/pypi/root/pypi/+simple/demo/"
	relayed := upstream.Request{URL: "https://files.pythonhosted.org/packages/demo.whl"}

	c.R = httptest.NewRequest("GET", "/global/pypi/root/pypi/+simple/demo/", nil)
	if got := c.credentialForURL(file); got == nil || got.Token != "team-token" {
		t.Fatalf("this machine's own request got %+v, want the team token", got)
	}
	if got := c.CachesFirst(relayed, "+files/x"); got.Credential == nil {
		t.Fatal("this machine's own relayed request lost the team token")
	}

	c.R = c.R.WithContext(FromSibling(context.Background()))
	if got := c.credentialForURL(file); got != nil {
		t.Fatalf("a sibling's request was given %+v", got)
	}
	if got := c.CachesFirst(relayed, "+files/x"); got.Credential != nil {
		t.Fatal("a sibling's relayed request carried the team token")
	}
}
