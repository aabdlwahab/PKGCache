package oci

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/eco/ecotest"
	testupstream "github.com/brightskies/pkgreg/internal/testutil/upstream"
)

func ociHarness(t *testing.T, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return NewWithRegistries(map[string]string{"dockerhub": origin.URL})
	})
}

func digestOf(body []byte) blob.Digest {
	sum := sha256.Sum256(body)
	return blob.Digest(fmt.Sprintf("%x", sum))
}

func imageManifest(configBody, layerBody []byte) []byte {
	doc := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      len(configBody),
			"digest":    digestOf(configBody).Prefixed(),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(layerBody),
			"digest":    digestOf(layerBody).Prefixed(),
		}},
	}
	body, _ := json.Marshal(doc)
	return body
}

func TestSingleArchPullBearerDigestAndBlobCache(t *testing.T) {
	configBody := []byte(`{"architecture":"amd64","os":"linux"}`)
	layerBody := []byte("compressed layer bytes")
	manifest := imageManifest(configBody, layerBody)
	manifestDigest := digestOf(manifest)

	h := ociHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/v2/library/alpine/manifests/3.20", testupstream.Behaviour{
			Body: manifest, ContentType: "application/vnd.docker.distribution.manifest.v2+json",
			RequireBearer: true,
		})
		origin.Handle("/v2/library/alpine/blobs/"+digestOf(configBody).Prefixed(),
			testupstream.Behaviour{Body: configBody, RequireBearer: true})
		origin.Handle("/v2/library/alpine/blobs/"+digestOf(layerBody).Prefixed(),
			testupstream.Behaviour{Body: layerBody, RequireBearer: true})
	})

	ping := h.Get("/v2/")
	if ping.Status != http.StatusOK ||
		ping.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("ping = %d headers=%v", ping.Status, ping.Header)
	}

	tag := h.Get("/v2/dockerhub/alpine/manifests/3.20")
	if tag.Status != http.StatusOK || string(tag.Body) != string(manifest) {
		t.Fatalf("tag = %d %s", tag.Status, tag.Text())
	}
	if got := tag.Header.Get("Docker-Content-Digest"); got != manifestDigest.Prefixed() {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, manifestDigest.Prefixed())
	}

	// The tag response linked the same bytes under their digest, so Docker's normal
	// follow-up digest request is a cache hit rather than another origin request.
	byDigest := h.Get("/v2/dockerhub/alpine/manifests/" + manifestDigest.Prefixed())
	if byDigest.Status != http.StatusOK || string(byDigest.Body) != string(manifest) {
		t.Fatalf("digest manifest = %d %s", byDigest.Status, byDigest.Text())
	}
	if hits := h.Origin.Hits("/v2/library/alpine/manifests/" + manifestDigest.Prefixed()); hits != 0 {
		t.Fatalf("digest manifest contacted origin %d times, want 0", hits)
	}

	for _, item := range []struct {
		digest blob.Digest
		body   []byte
	}{
		{digestOf(configBody), configBody},
		{digestOf(layerBody), layerBody},
	} {
		path := "/v2/library/alpine/blobs/" + item.digest.Prefixed()
		var firstHits int64
		for range 2 {
			resp := h.Get("/v2/dockerhub/alpine/blobs/" + item.digest.Prefixed())
			if resp.Status != http.StatusOK || string(resp.Body) != string(item.body) {
				t.Fatalf("blob %s = %d %q", item.digest, resp.Status, resp.Body)
			}
			if firstHits == 0 {
				firstHits = h.Origin.Hits(path)
			}
		}
		// A bearer-protected first fetch is two HTTP exchanges (401 challenge +
		// authenticated retry). The second client request must add neither.
		if hits := h.Origin.Hits(path); hits != firstHits || firstHits != 2 {
			t.Fatalf("%s origin hits = %d after first=%d, want unchanged challenge+retry",
				path, hits, firstHits)
		}
	}
	if hits := h.Origin.Hits("/v2/library/alpine/manifests/3.20"); hits != 2 {
		t.Fatalf("tag origin hits = %d, want bearer challenge + retry", hits)
	}
	if tokens := h.Origin.TokenRequests.Load(); tokens != 1 {
		t.Fatalf("bearer tokens minted = %d, want 1 cached token", tokens)
	}

	h.Flush()
	artifacts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, artifacts)
	}
	if artifact := artifacts[0]; artifact.Name != "dockerhub/alpine" ||
		artifact.Version != "3.20" || artifact.Digest != manifestDigest ||
		artifact.Size != int64(len(configBody)+len(layerBody)) {
		t.Fatalf("artifact = %+v", artifact)
	}
}

func TestMultiArchChildBackfillsTagSize(t *testing.T) {
	configBody := []byte("platform config")
	layerBody := []byte(strings.Repeat("layer", 50))
	child := imageManifest(configBody, layerBody)
	childDigest := digestOf(child)
	indexDoc := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"size":      len(child),
			"digest":    childDigest.Prefixed(),
			"platform":  map[string]string{"os": "linux", "architecture": "amd64"},
		}},
	}
	index, _ := json.Marshal(indexDoc)
	indexDigest := digestOf(index)

	h := ociHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/v2/org/app/manifests/latest", testupstream.Behaviour{
			Body: index, ContentType: "application/vnd.oci.image.index.v1+json",
		})
		origin.Handle("/v2/org/app/manifests/"+childDigest.Prefixed(),
			testupstream.Behaviour{
				Body: child, ContentType: "application/vnd.oci.image.manifest.v1+json",
			})
	})

	if resp := h.Get("/v2/dockerhub/org/app/manifests/latest"); resp.Status != http.StatusOK {
		t.Fatalf("index = %d %s", resp.Status, resp.Text())
	}
	if resp := h.Get("/v2/dockerhub/org/app/manifests/" + childDigest.Prefixed()); resp.Status != http.StatusOK {
		t.Fatalf("child = %d %s", resp.Status, resp.Text())
	}
	h.Flush()
	artifacts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, artifacts)
	}
	got := artifacts[0]
	if got.Name != "dockerhub/org/app" || got.Version != "latest" ||
		got.Arch != "amd64" || got.Digest != indexDigest ||
		got.Size != int64(len(configBody)+len(layerBody)) {
		t.Fatalf("back-filled artifact = %+v", got)
	}
}

func TestOfflineTagAndTagsListForNamedProject(t *testing.T) {
	manifest := imageManifest([]byte("cfg"), []byte("layer"))
	h := ociHarness(t, func(origin *testupstream.Server) {
		origin.Handle("/v2/library/alpine/manifests/3.20", testupstream.Behaviour{
			Body: manifest, ContentType: manifestMediaType,
		})
		origin.Handle("/v2/library/alpine/tags/list", testupstream.Behaviour{
			Body:        []byte(`{"name":"library/alpine","tags":["3.20","latest"]}`),
			ContentType: "application/json",
		})
	})
	h.SetProject("team-a")

	if resp := h.Get("/v2/dockerhub/alpine/manifests/3.20"); resp.Status != http.StatusOK {
		t.Fatalf("seed tag = %d %s", resp.Status, resp.Text())
	}
	online := h.Get("/v2/dockerhub/alpine/tags/list")
	if online.Status != http.StatusOK ||
		!strings.Contains(online.Text(), `"name":"team-a/dockerhub/alpine"`) {
		t.Fatalf("online tags/list = %d %s", online.Status, online.Text())
	}
	before := h.Origin.Requests.Load()
	h.Offline(true)

	if resp := h.Get("/v2/dockerhub/alpine/manifests/3.20"); resp.Status != http.StatusOK ||
		string(resp.Body) != string(manifest) {
		t.Fatalf("offline tag = %d %s", resp.Status, resp.Text())
	}
	offline := h.Get("/v2/dockerhub/alpine/tags/list")
	if offline.Status != http.StatusOK ||
		!strings.Contains(offline.Text(), `"name":"team-a/dockerhub/alpine"`) ||
		!strings.Contains(offline.Text(), `"3.20"`) {
		t.Fatalf("offline tags/list = %d %s", offline.Status, offline.Text())
	}
	if after := h.Origin.Requests.Load(); after != before {
		t.Fatalf("offline requests contacted origin: before=%d after=%d", before, after)
	}
}

func TestDigestManifestMismatchIsNotPublished(t *testing.T) {
	expected := []byte("expected")
	digest := digestOf(expected)
	h := ociHarness(t, func(origin *testupstream.Server) {
		origin.Serve("/v2/org/app/manifests/"+digest.Prefixed(), []byte("wrong"))
	})

	resp := h.Get("/v2/dockerhub/org/app/manifests/" + digest.Prefixed())
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.Status, resp.Text())
	}
	if _, err := h.Catalog.GetEntry(catalog.EntryKey{
		Project: "global", Eco: ID, Key: manifestKey(digest),
	}); err == nil {
		t.Fatal("mismatched manifest was published")
	}
}

func TestUpstreamManifestNotFoundUsesRegistryStatus(t *testing.T) {
	h := ociHarness(t, nil)
	resp := h.Get("/v2/dockerhub/alpine/manifests/missing")
	if resp.Status != http.StatusNotFound ||
		!strings.Contains(resp.Text(), `"code":"MANIFEST_UNKNOWN"`) {
		t.Fatalf("response = %d %s", resp.Status, resp.Text())
	}
}

func TestDescriptor(t *testing.T) {
	registry := eco.NewRegistry()
	registry.Register(New())
	desc := registry.Descriptors()[0]
	if desc.ID != ID || desc.Listener != eco.ListenerProtocolRooted ||
		desc.Upstreams != eco.UpstreamNamedSet ||
		!desc.FreshnessFor("manifest/abc").Immutable ||
		desc.FreshnessFor("tag/dockerhub/library/alpine/latest").Immutable {
		t.Fatalf("descriptor = %+v", desc)
	}
}
