package eco

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/router"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// A pkgreg index links its files at /+f/, beside its /+simple rather than under it. A
// token-protected team cache answered the index with the token and then refused every
// file, because no endpoint's path covered the file's URL. The origin's one credential
// covers it, as pip's own credential reuse does.
func TestCredentialCoversTheWholeOriginOfOneCredential(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	token := config.UpstreamCredential{Kind: "bearer", Token: "team-token"}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"root/pypi": {
			{URL: "https://team.internal:8443/work/pypi/root/pypi/+simple", Credential: token},
			{URL: "https://pypi.org/simple"},
		},
		"torch/cu126": {
			{URL: "https://team.internal:8443/work/pypi/torch/cu126/+simple", Credential: token},
		},
	})
	file := c.credentialForURL("https://team.internal:8443/work/pypi/root/pypi/+f/demo/demo-1.0.whl")
	if file == nil || file.Token != "team-token" {
		t.Fatalf("a file on the team cache got credential %+v, want the team's token", file)
	}
	for _, elsewhere := range []string{
		"https://files.pythonhosted.org/packages/demo-1.0.whl",
		"https://team.internal/work/pypi/root/pypi/+f/demo/demo-1.0.whl", // another port
		"http://team.internal:8443/work/pypi/root/pypi/+f/demo/demo-1.0.whl",
	} {
		if got := c.credentialForURL(elsewhere); got != nil {
			t.Fatalf("%s got the team's credential", elsewhere)
		}
	}
}

// Two different credentials on one origin say nothing about which a third path wants, and
// guessing would hand one of them to the wrong owner.
func TestCredentialIsWithheldWhereAnOriginHasTwo(t *testing.T) {
	descriptor := Descriptor{
		ID: "pypi", Storage: StorageBlob, Listener: ListenerPathPrefixed,
		Upstreams: UpstreamNamedSet,
	}
	c := chainCtx(t, descriptor, map[string][]config.Endpoint{
		"a": {{URL: "https://host.internal/a/simple",
			Credential: config.UpstreamCredential{Kind: "bearer", Token: "a"}}},
		"b": {{URL: "https://host.internal/b/simple",
			Credential: config.UpstreamCredential{Kind: "bearer", Token: "b"}}},
	})
	if got := c.credentialForURL("https://host.internal/files/x.whl"); got != nil {
		t.Fatalf("an ambiguous origin produced credential %+v", got)
	}
	if got := c.credentialForURL("https://host.internal/b/simple/x/"); got == nil || got.Token != "b" {
		t.Fatalf("a path an endpoint covers lost its credential: %+v", got)
	}
}

// The team's data-plane hop is TLS and carries its token; a sibling's does not have one,
// and the origin behind them keeps whatever credential it had of its own.
func TestCachesFirstPresentsTheTeamToken(t *testing.T) {
	snapshot := config.Defaults()
	snapshot.Local.Relays = map[string]config.Relay{config.GlobalProject: {
		Direct: true,
		Hops: []config.Hop{
			{Base: "http://172.17.21.107:41780/global", Sibling: true},
			{Base: "https://team.internal:8443/work", Token: "team-token"},
		},
	}}
	request := httptest.NewRequest(http.MethodGet, "/global/git/x", nil)
	c := NewCtx(httptest.NewRecorder(), request, config.GlobalProject, "", router.Params{},
		nil, &snapshot, Descriptor{ID: "git", Listener: ListenerPathPrefixed})
	const origin = "https://github.com/o/r/releases/download/v1/x.tar.gz"
	attempt := c.CachesFirst(upstream.Request{URL: origin}, "github.com/o/r/releases/download/v1/x.tar.gz")
	if attempt.Credential != nil {
		t.Fatalf("the sibling was sent a credential: %+v", attempt.Credential)
	}
	if len(attempt.Fallbacks) != 2 {
		t.Fatalf("fallbacks = %+v", attempt.Fallbacks)
	}
	team := attempt.Fallbacks[0]
	if team.URL != "https://team.internal:8443/work/git/github.com/o/r/releases/download/v1/x.tar.gz" ||
		team.Credential == nil || team.Credential.Kind != "bearer" || team.Credential.Token != "team-token" {
		t.Fatalf("team attempt = %+v", team)
	}
	if direct := attempt.Fallbacks[1]; direct.URL != origin || direct.Credential != nil {
		t.Fatalf("the origin attempt = %+v", direct)
	}
}
