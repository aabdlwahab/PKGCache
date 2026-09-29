package apt

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func aptHarness(t *testing.T, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return New()
	})
}

func proxyClient(t *testing.T, h *ecotest.Harness) *http.Client {
	t.Helper()
	proxy, err := url.Parse(h.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func proxyGet(t *testing.T, client *http.Client, target string) (int, http.Header, string) {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s through proxy: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func TestAbsoluteFormDebIsImmutableAndInventoried(t *testing.T) {
	const (
		originPath = "/debian/pool/main/d/demo/demo_1.2.3_amd64.deb"
		body       = "synthetic deb archive"
	)
	h := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(originPath, []byte(body))
	})
	client := proxyClient(t, h)
	target := h.Origin.URLFor(originPath)

	for range 2 {
		status, _, got := proxyGet(t, client, target)
		if status != http.StatusOK || got != body {
			t.Fatalf("deb = %d %q", status, got)
		}
	}
	if hits := h.Origin.Hits(originPath); hits != 1 {
		t.Fatalf("immutable deb origin hits = %d, want 1", hits)
	}

	h.Flush()
	artifacts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: config.GlobalProject, Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, artifacts)
	}
	got := artifacts[0]
	if got.Name != "demo" || got.Version != "1.2.3" || got.Arch != "amd64" ||
		got.Extra["format"] != "deb" || got.Origin != target {
		t.Fatalf("deb artifact = %+v", got)
	}
}

func TestVolatileIndexRevalidatesAndServesOffline(t *testing.T) {
	const path = "/debian/dists/stable/InRelease"
	h := aptHarness(t, func(origin *testupstream.Server) {
		origin.Handle(path, testupstream.Behaviour{
			Body: []byte("signed release"), ETag: `"release-v1"`,
			LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
			ContentType:  "text/plain",
		})
	})
	client := proxyClient(t, h)
	target := h.Origin.URLFor(path)

	for range 2 {
		status, headers, body := proxyGet(t, client, target)
		if status != http.StatusOK || body != "signed release" {
			t.Fatalf("InRelease = %d %q", status, body)
		}
		if headers.Get("ETag") != `"release-v1"` {
			t.Fatalf("ETag = %q", headers.Get("ETag"))
		}
	}
	if hits := h.Origin.Hits(path); hits != 2 {
		t.Fatalf("volatile origin hits = %d, want fetch + conditional revalidation", hits)
	}

	before := h.Origin.Requests.Load()
	h.Offline(true)
	status, _, body := proxyGet(t, client, target)
	if status != http.StatusOK || body != "signed release" {
		t.Fatalf("offline InRelease = %d %q", status, body)
	}
	if after := h.Origin.Requests.Load(); after != before {
		t.Fatalf("offline index contacted origin: before=%d after=%d", before, after)
	}
}

func TestAPKIsImmutableAndParsed(t *testing.T) {
	const (
		originPath = "/alpine/v3.20/main/x86_64/ca-certificates-bundle-20240705-r0.apk"
		body       = "synthetic apk archive"
	)
	h := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(originPath, []byte(body))
	})
	client := proxyClient(t, h)
	target := h.Origin.URLFor(originPath)

	for range 2 {
		status, headers, got := proxyGet(t, client, target)
		if status != http.StatusOK || got != body {
			t.Fatalf("apk = %d %q", status, got)
		}
		if mediaType := headers.Get("Content-Type"); mediaType != "application/octet-stream" {
			t.Fatalf("apk Content-Type = %q", mediaType)
		}
	}
	if hits := h.Origin.Hits(originPath); hits != 1 {
		t.Fatalf("immutable apk origin hits = %d, want 1", hits)
	}
	h.Flush()
	artifacts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: config.GlobalProject, Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, artifacts)
	}
	got := artifacts[0]
	if got.Name != "ca-certificates-bundle" || got.Version != "20240705-r0" ||
		got.Arch != "x86_64" || got.Extra["format"] != "apk" {
		t.Fatalf("apk artifact = %+v", got)
	}
}

func TestProxyAllowlistRejectsUnlistedHost(t *testing.T) {
	const originPath = "/pool/pkg.deb"
	h := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(originPath, []byte("package"))
	})
	if err := h.Config.Apply(func(s *config.Snapshot) error {
		s.Server.ProxyAllowlist = []string{"packages.example.invalid"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	status, _, body := proxyGet(t, proxyClient(t, h), h.Origin.URLFor(originPath))
	if status != http.StatusForbidden ||
		!strings.Contains(body, "proxy_allowlist") {
		t.Fatalf("blocked target = %d %q", status, body)
	}
	if hits := h.Origin.Hits(originPath); hits != 0 {
		t.Fatalf("blocked target reached origin %d times", hits)
	}
}

func TestUpstreamNotFoundIsRelayed(t *testing.T) {
	const path = "/dists/stable/InRelease"
	h := aptHarness(t, nil)
	status, _, _ := proxyGet(t, proxyClient(t, h), h.Origin.URLFor(path))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want the origin's 404", status)
	}
}

func TestReconstructTarget(t *testing.T) {
	t.Run("absolute form preserves query and escaping", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			"http://mirror.example/a/pkg%2Bname.deb?token=a%2Bb", nil)
		target, err := reconstructTarget(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := target.String(); got !=
			"http://mirror.example/a/pkg%2Bname.deb?token=a%2Bb" {
			t.Fatalf("target = %q", got)
		}
	})
	t.Run("origin form uses Host", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/dists/stable/Release", nil)
		req.Host = "mirror.example"
		target, err := reconstructTarget(req)
		if err != nil || target.String() != "http://mirror.example/dists/stable/Release" {
			t.Fatalf("target=%v err=%v", target, err)
		}
	})
	t.Run("an https repository asked for in plain proxy form is fetched over TLS", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet,
			"http://developer.download.nvidia.com:443/compute/cuda/repos/ubuntu2404/x86_64/InRelease", nil)
		target, err := reconstructTarget(req)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/InRelease"
		if target.String() != want || cacheKey(target) != want {
			t.Fatalf("target = %s, key = %s", target, cacheKey(target))
		}
	})
	t.Run("upstream credentials refused", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://user:pass@mirror.example/x", nil)
		if _, err := reconstructTarget(req); err == nil {
			t.Fatal("target userinfo should be rejected")
		}
	})
}

func TestDescriptor(t *testing.T) {
	registry := eco.NewRegistry()
	registry.Register(New())
	desc := registry.Descriptors()[0]
	if desc.ID != ID || desc.Listener != eco.ListenerForwardProxy ||
		desc.Upstreams != eco.UpstreamNone ||
		!desc.FreshnessFor("immutable/http://x/pkg.deb").Immutable ||
		desc.FreshnessFor("volatile/http://x/InRelease").Immutable {
		t.Fatalf("descriptor = %+v", desc)
	}
	name, version, arch, ok := desc.Artifact(
		"immutable/http://mirror/debian/demo_1.0_arm64.deb")
	if !ok || name != "demo" || version != "1.0" || arch != "arm64" {
		t.Fatalf("artifact parse = %q %q %q %v", name, version, arch, ok)
	}
}

// relayThrough sends a harness's apt requests through another cache's forward proxy, as
// pkgcache does once it is pointed at a team cache.
func relayThrough(t *testing.T, h *ecotest.Harness, proxy string, direct bool) {
	t.Helper()
	if err := h.Config.Apply(func(s *config.Snapshot) error {
		s.Local.Relays = map[string]config.Relay{
			config.GlobalProject: {Hops: []config.Hop{{Proxy: proxy}}, Direct: direct},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Three tiers for apt: the local cache asks the team's cache, which asks the mirror. The
// URL apt named travels unchanged; only the route does not.
func TestRelayedThroughTheTeamCache(t *testing.T) {
	const (
		debPath   = "/debian/pool/main/d/demo/demo_1.2.3_amd64.deb"
		indexPath = "/debian/dists/stable/InRelease"
	)
	team := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(debPath, []byte("deb via the team"))
		origin.Serve(indexPath, []byte("signed index"))
	})
	local := aptHarness(t, nil)
	relayThrough(t, local, team.Server.URL, false)
	client := proxyClient(t, local)

	for _, path := range []string{debPath, indexPath} {
		status, _, got := proxyGet(t, client, team.Origin.URLFor(path))
		if status != http.StatusOK || got == "" {
			t.Fatalf("%s = %d %q", path, status, got)
		}
	}
	// The team's cache holding them is what proves the route: its origin is also
	// reachable from here, so a direct fetch would have answered just the same.
	for _, key := range []string{
		"immutable/" + team.Origin.URLFor(debPath),
		"volatile/" + team.Origin.URLFor(indexPath),
	} {
		if _, err := team.Engine.Entry(config.GlobalProject, ID, key); err != nil {
			t.Fatalf("the team cache never saw %s: %v", key, err)
		}
	}
	if hits := team.Origin.Hits(debPath); hits != 1 {
		t.Fatalf("mirror hits = %d, want 1", hits)
	}
}

// With the team cache down, -no-direct means failing rather than quietly reaching the
// mirror; without it, the mirror is asked directly.
func TestRelayFallsBackOnlyWhenAllowed(t *testing.T) {
	const debPath = "/debian/pool/main/d/demo/demo_1.2.3_amd64.deb"
	local := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(debPath, []byte("deb"))
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	down := closed.URL
	closed.Close()
	client := proxyClient(t, local)
	target := local.Origin.URLFor(debPath)

	relayThrough(t, local, down, false)
	if status, _, _ := proxyGet(t, client, target); status < 500 {
		t.Fatalf("no-direct with the team down = %d, want a 5xx", status)
	}
	if hits := local.Origin.Hits(debPath); hits != 0 {
		t.Fatalf("no-direct reached the mirror %d times", hits)
	}

	relayThrough(t, local, down, true)
	if status, _, got := proxyGet(t, client, target); status != http.StatusOK || got != "deb" {
		t.Fatalf("direct fallback = %d %q", status, got)
	}
}

// Three tiers with a sibling in the middle: the local cache asks the sibling's proxy, the
// sibling fetches through its own relay (here the mirror itself, standing in for the
// internet as a proxy that serves by path), and both keep what went past them. The name
// apt asked for is a public one, the only kind a sibling relays.
func TestAptTravelsThroughASibling(t *testing.T) {
	const debPath = "/debian/pool/main/d/demo/demo_1.2.3_amd64.deb"
	sibling := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(debPath, []byte("deb via the sibling"))
	})
	relayThrough(t, sibling, sibling.Origin.URL, false)
	local := aptHarness(t, nil)
	if err := local.Config.Apply(func(s *config.Snapshot) error {
		s.Local.Relays = map[string]config.Relay{config.GlobalProject: {
			Hops: []config.Hop{{Proxy: sibling.Server.URL, Sibling: true}},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	target := "http://deb.example.test" + debPath
	status, _, got := proxyGet(t, proxyClient(t, local), target)
	if status != http.StatusOK || got != "deb via the sibling" {
		t.Fatalf("through the sibling = %d %q", status, got)
	}
	if _, err := sibling.Engine.Entry(config.GlobalProject, ID, "immutable/"+target); err != nil {
		t.Fatalf("the sibling never kept it: %v", err)
	}
	if hits := sibling.Origin.Hits(debPath); hits != 1 {
		t.Fatalf("mirror hits = %d, want 1", hits)
	}
}

// A sibling on an older pkgcache refuses every proxy request with 403, and a new one
// refuses what it will not relay. Neither stops apt: the attempt falls through to the
// next place in the relay.
func TestARefusingSiblingFallsThroughToTheTeam(t *testing.T) {
	const debPath = "/debian/pool/main/d/demo/demo_1.2.3_amd64.deb"
	team := aptHarness(t, func(origin *testupstream.Server) {
		origin.Serve(debPath, []byte("deb via the team"))
	})
	relayThrough(t, team, team.Origin.URL, false)
	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this address serves other machines' caches", http.StatusForbidden)
	}))
	t.Cleanup(older.Close)
	local := aptHarness(t, nil)
	if err := local.Config.Apply(func(s *config.Snapshot) error {
		s.Local.Relays = map[string]config.Relay{config.GlobalProject: {
			Hops: []config.Hop{{Proxy: older.URL, Sibling: true}, {Proxy: team.Server.URL}},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	status, _, got := proxyGet(t, proxyClient(t, local), "http://deb.example.test"+debPath)
	if status != http.StatusOK || got != "deb via the team" {
		t.Fatalf("with a refusing sibling = %d %q", status, got)
	}
}
