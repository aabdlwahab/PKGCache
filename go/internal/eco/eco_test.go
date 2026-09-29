package eco_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

// dummy is intentionally tiny: this is P3-01's proof that a new ecosystem only
// describes its protocol and inherits caching, streaming, dedup, and offline rules.
type dummy struct{ origin string }

func (d dummy) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID: "dummy", Display: "Dummy", Storage: eco.StorageBlob,
		Listener: eco.ListenerPathPrefixed, Upstreams: eco.UpstreamSingle,
		DefaultUpstreams: map[string]string{"origin": d.origin},
	}
}

func (d dummy) Routes() []eco.Route {
	return []eco.Route{{
		Methods: []string{http.MethodGet}, Pattern: "/{name}", Handler: d.get,
	}}
}

func (d dummy) get(w http.ResponseWriter, r *http.Request, p router.Params) {
	c := eco.CtxFrom(w, r, p)
	name := p.Unescape("name")
	err := c.Serve(engine.Resolution{
		Key: name, Upstream: c.UpstreamRequest(eco.JoinURL(d.origin, name), nil),
	})
	if err != nil {
		c.WriteError(err)
	}
}

func TestDummyEcosystemCachesEndToEnd(t *testing.T) {
	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		origin.Serve("/artifact.bin", []byte("dummy ecosystem bytes"))
		return dummy{origin: origin.URL}
	})
	for i := 0; i < 2; i++ {
		resp := h.Get("/artifact.bin")
		if resp.Status != http.StatusOK || resp.Text() != "dummy ecosystem bytes" {
			t.Fatalf("request %d = %d %q", i, resp.Status, resp.Text())
		}
	}
	if hits := h.Origin.Hits("/artifact.bin"); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
}

func relayCtx(t *testing.T, listener eco.ListenerKind, relay config.Relay) *eco.Ctx {
	t.Helper()
	snap := config.Defaults()
	snap.Local.Relays = map[string]config.Relay{config.GlobalProject: relay}
	desc := eco.Descriptor{ID: "apt", Listener: listener}
	req := httptest.NewRequest(http.MethodGet, "http://mirror.example/x", nil)
	return eco.NewCtx(httptest.NewRecorder(), req, config.GlobalProject, "", router.Params{},
		nil, &snap, desc)
}

// An https repository is relayed to the team as an ordinary proxied GET — the team's
// proxy refuses a tunnel — while the direct attempt behind it keeps the real URL.
func TestRelayedHTTPSRepositoryTravelsInPlainProxyForm(t *testing.T) {
	c := relayCtx(t, eco.ListenerForwardProxy,
		config.Relay{Hops: []config.Hop{{Proxy: "http://team.example:8443"}}, Direct: true})
	const target = "https://developer.download.nvidia.com/compute/cuda/repos/x/InRelease"
	req := c.UpstreamRequest(target, nil)
	if req.URL != "http://developer.download.nvidia.com:443/compute/cuda/repos/x/InRelease" ||
		req.Proxy != "http://team.example:8443" {
		t.Fatalf("relayed attempt = %s via %s", req.URL, req.Proxy)
	}
	if len(req.Fallbacks) != 1 || req.Fallbacks[0].URL != target || req.Fallbacks[0].Proxy != "" {
		t.Fatalf("direct fallback = %+v", req.Fallbacks)
	}

	walled := relayCtx(t, eco.ListenerForwardProxy, config.Relay{Hops: []config.Hop{{Proxy: "http://team.example:8443"}}})
	if req := walled.UpstreamRequest(target, nil); len(req.Fallbacks) != 0 {
		t.Fatalf("-no-direct kept a direct attempt: %+v", req.Fallbacks)
	}
}

// A request an ecosystem derived from its own request asks the team at the same path,
// with the origin behind it only when the team configuration allows reaching it.
func TestCachesFirstAsksTheTeamAtTheSamePath(t *testing.T) {
	const origin = "https://github.com/o/r/releases/download/v1/x.whl"
	const path = "github.com/o/r/releases/download/v1/x.whl"
	c := relayCtx(t, eco.ListenerPathPrefixed,
		config.Relay{Hops: []config.Hop{{Base: "https://team.example:8443/global"}}, Direct: true})
	req := c.CachesFirst(c.UpstreamRequest(origin, nil), path)
	if req.URL != "https://team.example:8443/global/apt/"+path {
		t.Fatalf("team URL = %s", req.URL)
	}
	if len(req.Fallbacks) != 1 || req.Fallbacks[0].URL != origin {
		t.Fatalf("fallbacks = %+v", req.Fallbacks)
	}
	walled := relayCtx(t, eco.ListenerPathPrefixed, config.Relay{Hops: []config.Hop{{Base: "https://team.example:8443/global"}}})
	if req := walled.CachesFirst(walled.UpstreamRequest(origin, nil), path); len(req.Fallbacks) != 0 {
		t.Fatalf("-no-direct kept the origin: %+v", req.Fallbacks)
	}
	none := relayCtx(t, eco.ListenerPathPrefixed, config.Relay{})
	if req := none.CachesFirst(none.UpstreamRequest(origin, nil), path); req.URL != origin {
		t.Fatalf("with no team the request changed: %s", req.URL)
	}
}

// An error after the response began must not be written into it: the body the client is
// reading as a success would gain "cache error" at the end.
func TestWriteErrorLeavesAStartedResponseAlone(t *testing.T) {
	serve := func(start bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		snap := config.Defaults()
		base := eco.NewCtx(rec, req, config.GlobalProject, "", router.Params{}, nil, &snap,
			eco.Descriptor{ID: "dummy"})
		c := eco.CtxFrom(rec, eco.Bind(req, base), router.Params{})
		if start {
			c.W.WriteHeader(http.StatusOK)
			_, _ = c.W.Write([]byte("partial body"))
		}
		c.WriteError(errors.New("upstream broke mid-stream"))
		return rec
	}
	if rec := serve(true); rec.Code != http.StatusOK || rec.Body.String() != "partial body" {
		t.Fatalf("started response became %d %q", rec.Code, rec.Body.String())
	}
	if rec := serve(false); rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unstarted failure answered %d, want 500", rec.Code)
	}
}

// Several caches are asked in the order a chain would: siblings, then the team, then the
// origin — for a forward-proxy URL and for a path alike.
func TestRelayAsksEveryCacheInOrder(t *testing.T) {
	relay := config.Relay{Direct: true, Hops: []config.Hop{
		{Proxy: "http://sibling.example:41780", Base: "http://sibling.example:41780/global"},
		{Proxy: "http://team.example:8443", Base: "https://team.example:8443/global"},
	}}
	const target = "https://mirror.example/pool/x.deb"
	proxied := relayCtx(t, eco.ListenerForwardProxy, relay).UpstreamRequest(target, nil)
	if proxied.Proxy != "http://sibling.example:41780" || len(proxied.Fallbacks) != 2 ||
		proxied.Fallbacks[0].Proxy != "http://team.example:8443" ||
		proxied.Fallbacks[1].URL != target || proxied.Fallbacks[1].Proxy != "" {
		t.Fatalf("proxy attempts = %s via %s, then %+v", proxied.URL, proxied.Proxy, proxied.Fallbacks)
	}

	const origin = "https://github.com/o/r/releases/download/v1/x.whl"
	c := relayCtx(t, eco.ListenerPathPrefixed, relay)
	byPath := c.CachesFirst(c.UpstreamRequest(origin, nil), "github.com/o/r/releases/download/v1/x.whl")
	if byPath.URL != "http://sibling.example:41780/global/apt/github.com/o/r/releases/download/v1/x.whl" ||
		len(byPath.Fallbacks) != 2 ||
		byPath.Fallbacks[0].URL != "https://team.example:8443/global/apt/github.com/o/r/releases/download/v1/x.whl" ||
		byPath.Fallbacks[1].URL != origin {
		t.Fatalf("path attempts = %s, then %+v", byPath.URL, byPath.Fallbacks)
	}
}

// A sibling is asked for apt as well now, but only for a public name — the only kind it
// relays — and never for a request that came from a sibling, which could go round in a
// circle between two machines that borrow from each other. Its attempt is Optional, so a
// refusal falls through to the team.
func TestSiblingRelaysAptForPublicNamesOnly(t *testing.T) {
	relay := config.Relay{Direct: true, Hops: []config.Hop{
		{Proxy: "http://global@sibling.example:41780", Base: "http://sibling.example:41780/global", Sibling: true},
		{Proxy: "http://team.example:8443", Base: "https://team.example:8443/global"},
	}}
	public := "http://deb.debian.org/debian/pool/main/c/curl/curl_8.5.0_amd64.deb"
	req := relayCtx(t, eco.ListenerForwardProxy, relay).UpstreamRequest(public, nil)
	if req.Proxy != "http://global@sibling.example:41780" || !req.Optional ||
		len(req.Fallbacks) != 2 || req.Fallbacks[0].Proxy != "http://team.example:8443" ||
		req.Fallbacks[0].Optional || req.Fallbacks[1].URL != public || req.Fallbacks[1].Proxy != "" {
		t.Fatalf("public name: %s via %s (optional %v), then %+v", req.URL, req.Proxy, req.Optional, req.Fallbacks)
	}
	for _, private := range []string{
		"http://10.0.0.5/debian/pool/x.deb", "http://mirror/debian/pool/x.deb", "http://localhost/x.deb",
	} {
		req := relayCtx(t, eco.ListenerForwardProxy, relay).UpstreamRequest(private, nil)
		if req.Proxy != "http://team.example:8443" || req.Optional {
			t.Errorf("%s went to %s (optional %v), want the team first", private, req.Proxy, req.Optional)
		}
	}

	fromSibling := relayCtx(t, eco.ListenerForwardProxy, relay)
	fromSibling.R = fromSibling.R.WithContext(eco.FromSibling(fromSibling.R.Context()))
	if req := fromSibling.UpstreamRequest(public, nil); req.Proxy != "http://team.example:8443" {
		t.Fatalf("a sibling's request was relayed to %s, want the team", req.Proxy)
	}
	const origin = "https://github.com/o/r/releases/download/v1/x.whl"
	byPath := fromSibling.CachesFirst(fromSibling.UpstreamRequest(origin, nil), "github.com/o/r/releases/download/v1/x.whl")
	if byPath.URL != "https://team.example:8443/global/apt/github.com/o/r/releases/download/v1/x.whl" {
		t.Fatalf("a sibling's release download went to %s, want the team", byPath.URL)
	}
}
