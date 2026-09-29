package npm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func npmHarness(t *testing.T, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return NewWithOrigin(origin.URL)
	})
}

func packument(t *testing.T, origin *testupstream.Server, name, filename, version string) []byte {
	t.Helper()
	doc := map[string]any{
		"name":      name,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"name": name, "version": version,
				"dist": map[string]any{
					"tarball":   origin.URLFor("/tarballs/" + filename),
					"shasum":    "0123456789abcdef",
					"integrity": "sha512-example",
				},
				"future-field": map[string]any{"nested": []any{1, true, "kept"}},
			},
		},
		"x-registry-extension": map[string]any{"opaque": "preserve me"},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPackumentRewriteAndUnknownFields(t *testing.T) {
	var original []byte
	h := npmHarness(t, func(origin *testupstream.Server) {
		original = packument(t, origin, "left-pad", "left-pad-1.3.0.tgz", "1.3.0")
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body: original, ContentType: "application/json", ETag: `"packument-v1"`,
		})
	})

	resp := h.Get("/left-pad")
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Status, resp.Text())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var extension map[string]any
	if err := json.Unmarshal(got["x-registry-extension"], &extension); err != nil ||
		extension["opaque"] != "preserve me" {
		t.Fatalf("unknown root field was lost: %s", got["x-registry-extension"])
	}
	var versions map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got["versions"], &versions); err != nil {
		t.Fatal(err)
	}
	var future map[string]any
	if err := json.Unmarshal(versions["1.3.0"]["future-field"], &future); err != nil ||
		len(future["nested"].([]any)) != 3 {
		t.Fatalf("unknown version field was lost: %s", versions["1.3.0"]["future-field"])
	}
	var meta struct {
		Dist struct {
			Tarball string `json:"tarball"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(gotVersion(t, got["versions"], "1.3.0"), &meta); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(meta.Dist.Tarball,
		"/global/npm/left-pad/-/left-pad-1.3.0.tgz") {
		t.Fatalf("tarball was not rewritten through the cache: %q", meta.Dist.Tarball)
	}
}

func TestScopedPackageEncodedAndLiteralForms(t *testing.T) {
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/@babel/core", testupstream.Behaviour{
			Body:        packument(t, origin, "@babel/core", "core-7.0.0.tgz", "7.0.0"),
			ContentType: "application/json",
		})
	})

	for _, path := range []string{"/@babel%2Fcore", "/@babel/core"} {
		resp := h.Get(path)
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, resp.Status, resp.Text())
		}
		if !strings.Contains(resp.Text(), "/global/npm/@babel/core/-/core-7.0.0.tgz") {
			t.Fatalf("GET %s did not preserve the scoped package path: %s", path, resp.Text())
		}
	}
}

func TestTarballStreamsCachesAndRecordsArtifact(t *testing.T) {
	const tarball = "npm tar archive bytes"
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/chalk", testupstream.Behaviour{
			Body:        packument(t, origin, "chalk", "chalk-5.3.0.tgz", "5.3.0"),
			ContentType: "application/json",
		})
		origin.Serve("/tarballs/chalk-5.3.0.tgz", []byte(tarball))
	})

	for i := 0; i < 2; i++ {
		resp := h.Get("/chalk/-/chalk-5.3.0.tgz")
		if resp.Status != http.StatusOK || resp.Text() != tarball {
			t.Fatalf("request %d: status=%d body=%q", i, resp.Status, resp.Text())
		}
	}
	if hits := h.Origin.Hits("/tarballs/chalk-5.3.0.tgz"); hits != 1 {
		t.Fatalf("tarball upstream hits = %d, want 1", hits)
	}
	h.Flush()
	arts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, arts)
	}
	if arts[0].Name != "chalk" || arts[0].Version != "5.3.0" ||
		arts[0].Extra["integrity"] != "sha512-example" {
		t.Fatalf("artifact = %+v", arts[0])
	}
}

func TestConcurrentTarballRequestsSingleFlight(t *testing.T) {
	const clients = 12
	body := strings.Repeat("tarball-", 128<<10)
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body:        packument(t, origin, "left-pad", "left-pad-1.3.0.tgz", "1.3.0"),
			ContentType: "application/json",
		})
		origin.Handle("/tarballs/left-pad-1.3.0.tgz", testupstream.Behaviour{
			Body: bodyBytes(body), ChunkSize: 16 << 10,
		})
	})

	var wg sync.WaitGroup
	errs := make(chan string, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := h.Get("/left-pad/-/left-pad-1.3.0.tgz")
			if resp.Status != http.StatusOK || resp.Text() != body {
				errs <- resp.Text()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for failure := range errs {
		t.Fatalf("client received wrong response: %q", failure)
	}
	if hits := h.Origin.Hits("/tarballs/left-pad-1.3.0.tgz"); hits != 1 {
		t.Fatalf("tarball upstream hits = %d, want 1", hits)
	}
}

// A tarball is tried at its conventional path, and on a 404 at the address its packument
// names, both under the tarball's one key. A request already falling back could join a
// later request's first try, still at the conventional path, and be handed that 404 as if
// it were the answer: one of twelve concurrent clients hit it on a GPU server. The origin's delays
// set that order up. The first two clients' conventional try ends at 300 ms and their
// packument arrives at 600 ms, and a third client starts its own conventional try at
// 450 ms, so at 600 ms the first two join a fetch of the wrong address.
func TestTarballFallbackIsNotHandedAnotherRequestsMiss(t *testing.T) {
	body := strings.Repeat("tarball-", 4<<10)
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/left-pad/-/left-pad-1.3.0.tgz", testupstream.Behaviour{
			Status: http.StatusNotFound, Delay: 300 * time.Millisecond,
		})
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body:        packument(t, origin, "left-pad", "left-pad-1.3.0.tgz", "1.3.0"),
			ContentType: "application/json", Delay: 300 * time.Millisecond,
		})
		origin.Handle("/tarballs/left-pad-1.3.0.tgz", testupstream.Behaviour{Body: bodyBytes(body)})
	})

	var wg sync.WaitGroup
	responses := make(chan string, 3)
	fetch := func(after time.Duration) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(after)
			resp := h.Get("/left-pad/-/left-pad-1.3.0.tgz")
			if resp.Status != http.StatusOK || resp.Text() != body {
				responses <- fmt.Sprintf("%d %.120q", resp.Status, resp.Text())
			}
		}()
	}
	fetch(0)
	fetch(0)
	fetch(450 * time.Millisecond)
	wg.Wait()
	close(responses)
	for failure := range responses {
		t.Errorf("client received %s", failure)
	}
	if hits := h.Origin.Hits("/tarballs/left-pad-1.3.0.tgz"); hits != 1 {
		t.Errorf("tarball upstream hits = %d, want 1", hits)
	}
}

// A cache chained behind another revalidates a packument each time its TTL runs out. The
// one in front answered with no validator, so every revalidation moved the whole packument
// again; now the second ask is a 304. A proxy between the two records what the front one
// answered.
func TestChainedPackumentRevalidationIsNotModified(t *testing.T) {
	team := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body:        packument(t, origin, "left-pad", "left-pad-1.3.0.tgz", "1.3.0"),
			ContentType: "application/json",
		})
	})
	target, err := url.Parse(team.URL("/"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var answered []int
	front := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) },
		ModifyResponse: func(resp *http.Response) error {
			mu.Lock()
			answered = append(answered, resp.StatusCode)
			mu.Unlock()
			return nil
		},
	})
	t.Cleanup(front.Close)
	local := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		behind := NewWithOrigin(front.URL)
		behind.ttl = 0 // revalidate on every request
		return behind
	})
	for range 2 {
		if resp := local.Get("/left-pad"); resp.Status != http.StatusOK || !strings.Contains(resp.Text(), "left-pad-1.3.0.tgz") {
			t.Fatalf("packument = %d %.80q", resp.Status, resp.Text())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 2 || answered[0] != http.StatusOK || answered[1] != http.StatusNotModified {
		t.Fatalf("the team answered %v, want [200 304]", answered)
	}
}

func TestOfflineUsesCachedPackumentAndTarball(t *testing.T) {
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body:        packument(t, origin, "left-pad", "left-pad-1.3.0.tgz", "1.3.0"),
			ContentType: "application/json",
		})
		origin.Serve("/tarballs/left-pad-1.3.0.tgz", []byte("cached"))
	})
	if resp := h.Get("/left-pad/-/left-pad-1.3.0.tgz"); resp.Status != http.StatusOK {
		t.Fatalf("seed status = %d", resp.Status)
	}
	h.Offline(true)
	if resp := h.Get("/left-pad"); resp.Status != http.StatusOK {
		t.Fatalf("offline packument status = %d: %s", resp.Status, resp.Text())
	}
	if resp := h.Get("/left-pad/-/left-pad-1.3.0.tgz"); resp.Text() != "cached" {
		t.Fatalf("offline tarball = %d %q", resp.Status, resp.Text())
	}
}

func TestDescriptor(t *testing.T) {
	reg := eco.NewRegistry()
	reg.Register(New())
	d := reg.Descriptors()[0]
	if d.ID != ID || d.Upstreams != eco.UpstreamSingle ||
		!d.FreshnessFor("left-pad/-/left-pad.tgz").Immutable ||
		d.FreshnessFor("packument/left-pad").Immutable {
		t.Fatalf("descriptor = %+v", d)
	}
	name, version, _, ok := d.Artifact("left-pad/-/left-pad-1.3.0.tgz")
	if !ok || name != "left-pad" || version != "1.3.0" {
		t.Fatalf("artifact parse = %q %q %v", name, version, ok)
	}
}

func gotVersion(t *testing.T, raw json.RawMessage, version string) json.RawMessage {
	t.Helper()
	var versions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &versions); err != nil {
		t.Fatal(err)
	}
	return versions[version]
}

func bodyBytes(s string) []byte { return []byte(s) }

// Metadata that could not be fetched is a server error, not a missing package.
func TestPackumentFailureIsNotReportedAsMissing(t *testing.T) {
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/broken-pkg", testupstream.Behaviour{Status: http.StatusBadGateway})
		origin.Handle("/absent-pkg", testupstream.Behaviour{Status: http.StatusNotFound})
	})
	if resp := h.Get("/broken-pkg"); resp.Status < 500 {
		t.Fatalf("unreachable metadata answered %d, want a server error", resp.Status)
	}
	if resp := h.Get("/absent-pkg"); resp.Status != http.StatusNotFound {
		t.Fatalf("a package the registry does not have answered %d, want 404", resp.Status)
	}
}

// corepack reads one version, by number or by dist-tag, and follows its tarball. Every
// spelling answers from the packument the cache already holds.
func TestOneVersionIsServedByVersionAndByTag(t *testing.T) {
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/pnpm", testupstream.Behaviour{
			Body:        packument(t, origin, "pnpm", "pnpm-9.15.4.tgz", "9.15.4"),
			ContentType: "application/json",
		})
		origin.Serve("/tarballs/pnpm-9.15.4.tgz", []byte("pnpm itself"))
		origin.Handle("/@babel/core", testupstream.Behaviour{
			Body:        packument(t, origin, "@babel/core", "core-7.0.0.tgz", "7.0.0"),
			ContentType: "application/json",
		})
	})

	var entry struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	for _, path := range []string{"/pnpm/9.15.4", "/pnpm/latest"} {
		resp := h.Get(path)
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, resp.Status, resp.Text())
		}
		if err := json.Unmarshal(resp.Body, &entry); err != nil {
			t.Fatalf("GET %s is not one version: %v", path, err)
		}
		if entry.Version != "9.15.4" || entry.Dist.Integrity != "sha512-example" ||
			!strings.HasSuffix(entry.Dist.Tarball, "/global/npm/pnpm/-/pnpm-9.15.4.tgz") {
			t.Fatalf("GET %s = %+v, want 9.15.4 with its tarball through the cache", path, entry)
		}
	}
	follow, _ := http.NewRequest(http.MethodGet, entry.Dist.Tarball, nil)
	if got := h.Do(follow); got.Status != http.StatusOK || got.Text() != "pnpm itself" {
		t.Fatalf("following the version's tarball = %d %q", got.Status, got.Text())
	}
	if n := h.Origin.Hits("/pnpm"); n != 1 {
		t.Fatalf("the packument was fetched %d times, want once for every version asked", n)
	}

	for _, path := range []string{"/@babel%2Fcore/7.0.0", "/@babel/core/7.0.0", "/@babel/core/latest"} {
		resp := h.Get(path)
		if resp.Status != http.StatusOK ||
			!strings.Contains(resp.Text(), "/global/npm/@babel/core/-/core-7.0.0.tgz") {
			t.Fatalf("GET %s = %d: %s", path, resp.Status, resp.Text())
		}
	}

	for _, path := range []string{"/pnpm/10.0.0", "/pnpm/next", "/babel/core/7.0.0"} {
		if resp := h.Get(path); resp.Status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.Status)
		}
	}
	// The packument itself is unchanged by any of this.
	if resp := h.Get("/@babel/core"); resp.Status != http.StatusOK ||
		!strings.Contains(resp.Text(), `"versions"`) {
		t.Fatalf("scoped packument = %d: %s", resp.Status, resp.Text())
	}
}

// A tarball at the registry's conventional path is fetched without its packument. A
// frozen-lockfile install asks for nothing else, and looking each one up in its packument
// first cost a multi-megabyte document per tarball at every tier of a chain.
func TestTarballAtTheConventionalPathNeedsNoPackument(t *testing.T) {
	h := npmHarness(t, func(origin *testupstream.Server) {
		origin.Serve("/react/-/react-19.0.0-rc-1.tgz", []byte("react"))
		origin.Serve("/@babel/core/-/core-7.0.0.tgz", []byte("babel"))
		origin.Handle("/react", testupstream.Behaviour{Status: http.StatusInternalServerError})
		origin.Handle("/@babel/core", testupstream.Behaviour{Status: http.StatusInternalServerError})
	})

	for path, want := range map[string]string{
		"/react/-/react-19.0.0-rc-1.tgz":  "react",
		"/@babel/core/-/core-7.0.0.tgz":   "babel",
		"/@babel%2Fcore/-/core-7.0.0.tgz": "babel",
	} {
		if resp := h.Get(path); resp.Status != http.StatusOK || resp.Text() != want {
			t.Fatalf("GET %s = %d %q", path, resp.Status, resp.Text())
		}
	}
	for _, packument := range []string{"/react", "/@babel/core"} {
		if n := h.Origin.Hits(packument); n != 0 {
			t.Errorf("%s was fetched %d times for a tarball at the conventional path", packument, n)
		}
	}
	h.Flush()
	arts, _, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: "global", Eco: ID})
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, a := range arts {
		versions[a.Name] = a.Version
	}
	if versions["react"] != "19.0.0-rc-1" || versions["@babel/core"] != "7.0.0" {
		t.Fatalf("inventory versions = %v", versions)
	}
}

func TestParseArtifactKeyKeepsPrereleaseVersions(t *testing.T) {
	for key, want := range map[string][2]string{
		"react/-/react-19.0.0-rc-1.tgz":     {"react", "19.0.0-rc-1"},
		"@types/node/-/node-20.1.0.tgz":     {"@types/node", "20.1.0"},
		"left-pad/-/left-pad-1.3.0.tgz":     {"left-pad", "1.3.0"},
		"odd/-/something-else-2.0.0.tgz":    {"odd", "2.0.0"}, // unconventional: last hyphen
		"@scope/pkg/-/pkg-1.0.0-beta.2.tgz": {"@scope/pkg", "1.0.0-beta.2"},
	} {
		name, version, _, ok := parseArtifactKey(key)
		if !ok || name != want[0] || version != want[1] {
			t.Errorf("parseArtifactKey(%q) = %q %q %v, want %q %q", key, name, version, ok, want[0], want[1])
		}
	}
}
