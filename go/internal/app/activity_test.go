package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The activity signal decides when pkgcache's daemon may exit, so what does and does
// not count is a behaviour worth pinning rather than a detail of the wrapper.
func TestActivityExcludesProbes(t *testing.T) {
	cases := []struct {
		path  string
		count bool
	}{
		{"/global/npm/left-pad", true},
		{"/v2/dockerhub/library/alpine/manifests/3.20", true},
		{"/api/v1/projects", true},
		{"/console", true},
		{"/healthz", false},
		{"/readyz", false},
		{"/version", false},
		{"/metrics", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var seen int
			a := &App{Activity: func() { seen++ }}
			handler := a.noteActivity(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if got := seen > 0; got != tc.count {
				t.Fatalf("%s counted as activity = %v, want %v", tc.path, got, tc.count)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("the wrapper changed the response: %d", recorder.Code)
			}
		})
	}
}

// A server never sets Activity, and must run the same handler chain it always did:
// the wrapper is not installed at all rather than installed and skipped.
func TestNoActivityHookLeavesTheChainAlone(t *testing.T) {
	a := &App{}
	var served bool
	inner := &countingHandler{onServe: func() { served = true }}
	if wrapped := a.noteActivity(inner); wrapped != http.Handler(inner) {
		t.Fatalf("expected the original handler back, got %T", wrapped)
	}
	a.noteActivity(inner).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/global/npm/x", nil))
	if !served {
		t.Fatal("the handler was not called")
	}
}

type countingHandler struct{ onServe func() }

func (h *countingHandler) ServeHTTP(http.ResponseWriter, *http.Request) { h.onServe() }
