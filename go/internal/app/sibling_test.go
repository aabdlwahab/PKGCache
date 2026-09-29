package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

func localApp(t *testing.T) *App {
	t.Helper()
	snap := config.LocalDefaults()
	snap.DataDir = t.TempDir()
	snap.Log.Level = "error"
	snap.Server.UnifiedAddr = "0.0.0.0:41780"
	snap.Server.ProxyAddr, snap.Server.AdminAddr = snap.Server.UnifiedAddr, snap.Server.UnifiedAddr
	a, err := Open(&snap)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// This machine gets the whole of a pkgcache; another machine gets packages, read-only, the
// little `peer add` asks for, and apt through the proxy for public names — never the
// control plane, the console, a tunnel, or a relay to an address only this host can reach.
func TestSiblingSurfaceIsReadOnlyPackagesOnly(t *testing.T) {
	handler := localApp(t).SinglePortHandler()
	serve := func(from, method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = from
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	const here, there = "127.0.0.1:50000", "172.17.21.106:50000"
	cases := []struct {
		from, method, target string
		want                 int
	}{
		{here, http.MethodGet, "/api/v1/tokens", http.StatusOK},
		{here, http.MethodGet, "/global/pypi/+indexes", http.StatusOK},
		{there, http.MethodGet, "/global/pypi/+indexes", http.StatusOK},
		{there, http.MethodGet, "/api/v1/projects", http.StatusOK},
		{there, http.MethodGet, "/healthz", http.StatusOK},
		{there, http.MethodGet, "/api/v1/tokens", http.StatusNotFound},
		{there, http.MethodPost, "/api/v1/tokens", http.StatusMethodNotAllowed},
		{there, http.MethodDelete, "/global/pypi/+f/x/x.whl", http.StatusMethodNotAllowed},
		{there, http.MethodGet, "/console", http.StatusNotFound},
		{there, http.MethodGet, "/metrics", http.StatusNotFound},
		{there, http.MethodGet, "http://10.0.0.5/debian/dists/stable/InRelease", http.StatusForbidden},
		{there, http.MethodGet, "http://mirror/debian/dists/stable/InRelease", http.StatusForbidden},
		{there, http.MethodGet, "http://localhost:8080/debian/dists/stable/InRelease", http.StatusForbidden},
		{there, http.MethodConnect, "deb.debian.org:443", http.StatusForbidden},
		// Discovery and forges named by another machine must be public names.
		{there, http.MethodGet, "/v2/10.0.0.5:5000/app/manifests/latest", http.StatusNotFound},
		{there, http.MethodGet, "/global/git/10.0.0.5/o/r/releases/download/v1/x.tgz", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := serve(c.from, c.method, c.target)
		if rec.Code != c.want {
			t.Errorf("%s %s %s = %d, want %d: %s", c.from, c.method, c.target, rec.Code, c.want,
				strings.TrimSpace(rec.Body.String()))
		}
	}
}

// A public name gets past the sibling gate to the apt adapter. Offline, so the answer is
// the cache's own offline miss and nothing leaves this machine.
func TestSiblingIsLentTheProxyForPublicNames(t *testing.T) {
	snap := config.LocalDefaults()
	snap.DataDir = t.TempDir()
	snap.Log.Level = "error"
	snap.Upstream.Offline = true
	snap.Server.UnifiedAddr = "0.0.0.0:41780"
	snap.Server.ProxyAddr, snap.Server.AdminAddr = snap.Server.UnifiedAddr, snap.Server.UnifiedAddr
	a, err := Open(&snap)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	req := httptest.NewRequest(http.MethodGet, "http://deb.debian.org/debian/dists/stable/InRelease", nil)
	req.RemoteAddr = "172.17.21.106:50000"
	rec := httptest.NewRecorder()
	a.SinglePortHandler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden || !strings.Contains(strings.ToLower(rec.Body.String()), "offline") {
		t.Fatalf("a sibling asking for a public name = %d %q, want the offline miss", rec.Code, rec.Body.String())
	}
}
