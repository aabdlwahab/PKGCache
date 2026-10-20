package engine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/obs"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// fullCache refuses every store, which is pkgcache over its limit.
type fullCache struct{}

func (fullCache) MayStore(int64) (bool, string) { return false, "the cache is full" }

// A full cache still serves what the origin has, and keeps none of the origin's own
// headers beyond the ones that describe the bytes.
func TestPassThroughRelaysTheArtifactAndNotTheOriginsCookies(t *testing.T) {
	h := newHarness(t)
	h.engine.guard = fullCache{}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=abc; Domain=npmjs.org")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("wheel bytes"))
	}))
	t.Cleanup(origin.Close)

	res := h.resolution("/six.whl")
	res.Upstream = upstream.Request{URL: origin.URL + "/six.whl"}
	rec, outcome, err := h.serve(t, get("/six.whl"), res)
	if err != nil || outcome != OutcomeUncached {
		t.Fatalf("outcome %q, err %v", outcome, err)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "wheel bytes" {
		t.Fatalf("served %d %q", rec.Code, rec.Body.String())
	}
	for _, leaked := range []string{"Set-Cookie", "Strict-Transport-Security"} {
		if got := rec.Header().Get(leaked); got != "" {
			t.Errorf("%s relayed from the origin: %q", leaked, got)
		}
	}
	if rec.Header().Get("ETag") != `"v1"` || rec.Header().Get("X-Pkgcache-Uncached") == "" {
		t.Errorf("headers that describe the bytes went missing: %v", rec.Header())
	}
}

// An origin's refusal is an error, as it is on a miss — not a finished download of the
// origin's error page.
func TestPassThroughReportsAnOriginRefusalAsAFailure(t *testing.T) {
	h := newHarness(t)
	h.engine.guard = fullCache{}
	h.origin.Handle("/gone.whl", testupstream.Behaviour{Status: http.StatusNotFound})

	subscription := h.engine.events.Subscribe(16, obs.EventFetchDone, obs.EventFetchError)
	defer subscription.Close()

	rec, outcome, err := h.serve(t, get("/gone.whl"), h.resolution("/gone.whl"))
	var refused *UpstreamHTTPError
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound || outcome != OutcomeFail {
		t.Fatalf("outcome %q, err %v", outcome, err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("the origin's error page was relayed: %q", rec.Body.String())
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-subscription.C:
			switch event.Kind {
			case obs.EventFetchDone:
				t.Fatalf("a refusal was published as a finished fetch: %+v", event)
			case obs.EventFetchError:
				return
			}
		case <-deadline:
			t.Fatal("no fetch.error for the refusal")
		}
	}
}
