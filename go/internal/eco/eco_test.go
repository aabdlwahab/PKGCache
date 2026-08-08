package eco_test

import (
	"net/http"
	"testing"

	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/eco/ecotest"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
	testupstream "github.com/brightskies/pkgreg/internal/testutil/upstream"
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
