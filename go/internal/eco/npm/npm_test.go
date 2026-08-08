package npm

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/eco/ecotest"
	testupstream "github.com/brightskies/pkgreg/internal/testutil/upstream"
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
