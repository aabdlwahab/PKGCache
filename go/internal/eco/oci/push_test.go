package oci

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func pushHarness(t *testing.T, policy PushPolicy, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return NewWithRegistries(map[string]string{"dockerhub": origin.URL}).WithPush(policy)
	})
}

func send(t *testing.T, h *ecotest.Harness, method, path string, body []byte, header http.Header) *ecotest.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.URL(path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	return h.Do(req)
}

// pushLayer sends a layer the way docker does: a POST to open the upload, one PATCH with
// the bytes, and a PUT naming the digest.
func pushLayer(t *testing.T, h *ecotest.Harness, repo string, content []byte, header http.Header) blob.Digest {
	t.Helper()
	opened := send(t, h, http.MethodPost, "/v2/local/"+repo+"/blobs/uploads/", nil, header)
	if opened.Status != http.StatusAccepted {
		t.Fatalf("open upload: %d %s", opened.Status, opened.Text())
	}
	location := opened.Header.Get("Location")
	sent := send(t, h, http.MethodPatch, location, content, header)
	if sent.Status != http.StatusAccepted || sent.Header.Get("Range") != byteRange(int64(len(content))) {
		t.Fatalf("patch: %d range %q %s", sent.Status, sent.Header.Get("Range"), sent.Text())
	}
	digest := blob.DigestBytes(content)
	closed := send(t, h, http.MethodPut, location+"?digest="+digest.Prefixed(), nil, header)
	if closed.Status != http.StatusCreated || closed.Header.Get("Docker-Content-Digest") != digest.Prefixed() {
		t.Fatalf("close upload: %d %s", closed.Status, closed.Text())
	}
	return digest
}

func pushedManifest(config, layer blob.Digest) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":2},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":5}]}`,
		config.Prefixed(), layer.Prefixed()))
}

func manifestHeader() http.Header {
	return http.Header{"Content-Type": {"application/vnd.oci.image.manifest.v1+json"}}
}

// The whole round trip on a machine's cache: an image built here is pushed under local/
// and pulled back by tag, by digest and layer by layer. The registry behind the harness
// serves nothing, so every answer came from what was pushed.
func TestPushedImageIsPulledBack(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	config := pushLayer(t, h, "tools/mold", []byte("{}"), nil)
	layer := pushLayer(t, h, "tools/mold", []byte("bytes"), nil)
	manifest := pushedManifest(config, layer)
	pushed := send(t, h, http.MethodPut, "/v2/local/tools/mold/manifests/latest", manifest, manifestHeader())
	if pushed.Status != http.StatusCreated {
		t.Fatalf("push manifest: %d %s", pushed.Status, pushed.Text())
	}
	digest := blob.DigestBytes(manifest)
	if pushed.Header.Get("Docker-Content-Digest") != digest.Prefixed() {
		t.Fatalf("digest %q, want %s", pushed.Header.Get("Docker-Content-Digest"), digest.Prefixed())
	}

	for _, ref := range []string{"latest", digest.Prefixed()} {
		got := h.Get("/v2/local/tools/mold/manifests/" + ref)
		if got.Status != http.StatusOK || !bytes.Equal(got.Body, manifest) {
			t.Fatalf("pull %s: %d %s", ref, got.Status, got.Text())
		}
		if got.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
			t.Fatalf("pull %s: content type %q", ref, got.Header.Get("Content-Type"))
		}
	}
	if got := h.Get("/v2/local/tools/mold/blobs/" + layer.Prefixed()); got.Status != http.StatusOK || got.Text() != "bytes" {
		t.Fatalf("pull layer: %d %q", got.Status, got.Text())
	}
	if tags := h.Get("/v2/local/tools/mold/tags/list"); !strings.Contains(tags.Text(), `"latest"`) {
		t.Fatalf("tags: %s", tags.Text())
	}
}

// docker sends a layer it already sent elsewhere as a mount, and some clients send a small
// one in a single request; both store it without an upload session.
func TestMonolithicAndMountedUploads(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	content := []byte("whole layer")
	digest := blob.DigestBytes(content)
	whole := send(t, h, http.MethodPost, "/v2/local/a/blobs/uploads/?digest="+digest.Prefixed(), content, nil)
	if whole.Status != http.StatusCreated {
		t.Fatalf("monolithic: %d %s", whole.Status, whole.Text())
	}
	mounted := send(t, h, http.MethodPost, "/v2/local/b/blobs/uploads/?mount="+digest.Prefixed()+"&from=local/a", nil, nil)
	if mounted.Status != http.StatusCreated || mounted.Header.Get("Docker-Content-Digest") != digest.Prefixed() {
		t.Fatalf("mount: %d %s", mounted.Status, mounted.Text())
	}
	// A mount of something this instance does not hold opens an ordinary upload instead.
	absent := blob.DigestBytes([]byte("not here"))
	fallback := send(t, h, http.MethodPost, "/v2/local/b/blobs/uploads/?mount="+absent.Prefixed(), nil, nil)
	if fallback.Status != http.StatusAccepted {
		t.Fatalf("mount of an absent layer: %d, want an upload session", fallback.Status)
	}
}

// A manifest whose layers are not here is an image nobody could pull, and accepting it
// would let the push report success.
func TestManifestNamingALayerNeverPushedIsRefused(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	config := pushLayer(t, h, "app", []byte("{}"), nil)
	missing := blob.DigestBytes([]byte("never sent"))
	got := send(t, h, http.MethodPut, "/v2/local/app/manifests/1.0", pushedManifest(config, missing), manifestHeader())
	if got.Status != http.StatusBadRequest || !strings.Contains(got.Text(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("got %d %s", got.Status, got.Text())
	}
	if pulled := h.Get("/v2/local/app/manifests/1.0"); pulled.Status != http.StatusNotFound {
		t.Fatalf("the refused manifest is pullable: %d", pulled.Status)
	}
}

func TestManifestPushedUnderAnotherDigestIsRefused(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	config := pushLayer(t, h, "app", []byte("{}"), nil)
	layer := pushLayer(t, h, "app", []byte("bytes"), nil)
	wrong := blob.DigestBytes([]byte("some other manifest"))
	got := send(t, h, http.MethodPut, "/v2/local/app/manifests/"+wrong.Prefixed(), pushedManifest(config, layer), manifestHeader())
	if got.Status != http.StatusBadRequest || !strings.Contains(got.Text(), "DIGEST_INVALID") {
		t.Fatalf("got %d %s", got.Status, got.Text())
	}
}

// Everything else is a copy of somebody's registry, under that registry's name; pushing
// into it would put a built image where a pulled one is expected.
func TestPushesGoUnderLocal(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	got := send(t, h, http.MethodPost, "/v2/dockerhub/library/alpine/blobs/uploads/", nil, nil)
	if got.Status != http.StatusForbidden || !strings.Contains(got.Text(), "local/") {
		t.Fatalf("got %d %s", got.Status, got.Text())
	}
	if got := send(t, h, http.MethodPost, "/v2/local/Upper/blobs/uploads/", nil, nil); got.Status != http.StatusBadRequest {
		t.Fatalf("an invalid repository name was accepted: %d", got.Status)
	}
}

type writeToken struct{ token string }

func (w writeToken) HasToken(_, _, scope string) bool { return w.token != "" && scope == "write" }
func (w writeToken) VerifyToken(_, _, scope, presented string) bool {
	return w.token != "" && scope == "write" && presented == w.token
}

// A team server takes a push only with a write token, as a bearer token — how pkgcache
// sends one — or as a Basic password, how docker login stores one.
func TestTeamServerTakesPushesOnlyWithAWriteToken(t *testing.T) {
	none := pushHarness(t, PushPolicy{Tokens: writeToken{}}, nil)
	if got := send(t, none, http.MethodPost, "/v2/local/app/blobs/uploads/", nil, nil); got.Status != http.StatusForbidden ||
		!strings.Contains(got.Text(), "write token") {
		t.Fatalf("no token configured: %d %s", got.Status, got.Text())
	}

	h := pushHarness(t, PushPolicy{Tokens: writeToken{token: "id.secret"}}, nil)
	refused := send(t, h, http.MethodPost, "/v2/local/app/blobs/uploads/", nil,
		http.Header{"Authorization": {"Bearer id.wrong"}})
	if refused.Status != http.StatusUnauthorized || !strings.HasPrefix(refused.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("wrong token: %d %q", refused.Status, refused.Header.Get("WWW-Authenticate"))
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("anyone:id.secret"))
	for _, credential := range []string{"Bearer id.secret", basic} {
		got := send(t, h, http.MethodPost, "/v2/local/app/blobs/uploads/", nil, http.Header{"Authorization": {credential}})
		if got.Status != http.StatusAccepted {
			t.Fatalf("%s: %d %s", strings.Fields(credential)[0], got.Status, got.Text())
		}
	}
	// Pulls are unchanged: a token is what pushing asks for, and nothing else.
	if got := none.Get("/v2/"); got.Status != http.StatusOK {
		t.Fatalf("the registry ping now wants credentials: %d", got.Status)
	}
}

// The air-gapped side is a mirror: an image pushed there would be lost at the next import.
func TestOfflineProjectTakesNoPush(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	h.Offline(true)
	if got := send(t, h, http.MethodPost, "/v2/local/app/blobs/uploads/", nil, nil); got.Status != http.StatusForbidden ||
		!strings.Contains(got.Text(), "offline") {
		t.Fatalf("got %d %s", got.Status, got.Text())
	}
}

// A pushed image's base layers are usually cached already. They are answered as present,
// so docker does not send them, and the manifest files them under hosted/ by linking the
// blob the store holds — one copy on disk, and the pushed image no longer depends on a
// cached entry that eviction may take.
func TestCachedBaseLayerIsLinkedNotSentAgain(t *testing.T) {
	base := []byte("base layer")
	digest := blob.DigestBytes(base)
	h := pushHarness(t, PushPolicy{Open: true}, func(origin *testupstream.Server) {
		origin.Serve("/v2/library/python/blobs/"+digest.Prefixed(), base)
	})
	if got := h.Get("/v2/dockerhub/library/python/blobs/" + digest.Prefixed()); got.Status != http.StatusOK {
		t.Fatalf("pull base layer: %d", got.Status)
	}
	if got := send(t, h, http.MethodHead, "/v2/local/app/blobs/"+digest.Prefixed(), nil, nil); got.Status != http.StatusOK {
		t.Fatalf("a cached layer was not reported present to the pusher: %d", got.Status)
	}
	config := pushLayer(t, h, "app", []byte("{}"), nil)
	if got := send(t, h, http.MethodPut, "/v2/local/app/manifests/1.0", pushedManifest(config, digest), manifestHeader()); got.Status != http.StatusCreated {
		t.Fatalf("push manifest: %d %s", got.Status, got.Text())
	}
	h.Flush()
	if entry, ok := h.Engine.Entry("global", ID, hostedBlobKey(digest)); ok != nil || entry.Digest != digest {
		t.Fatalf("the base layer was not filed under hosted/: %+v %v", entry, ok)
	}
	if hits := h.Origin.Hits("/v2/library/python/blobs/" + digest.Prefixed()); hits != 1 {
		t.Fatalf("the base layer was fetched %d times", hits)
	}
}

// A chunk that does not start where the upload stands would stage a different layer from
// the one the client is about to name.
func TestChunkThatDoesNotStartWhereTheUploadStandsIsRefused(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	opened := send(t, h, http.MethodPost, "/v2/local/app/blobs/uploads/", nil, nil)
	location := opened.Header.Get("Location")
	got := send(t, h, http.MethodPatch, location, []byte("late"), http.Header{"Content-Range": {"10-13"}})
	if got.Status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("got %d %s", got.Status, got.Text())
	}
	if got := send(t, h, http.MethodPatch, location, []byte("first"), http.Header{"Content-Range": {"0-4"}}); got.Status != http.StatusAccepted {
		t.Fatalf("a chunk at the right offset was refused: %d", got.Status)
	}
}

// buildx pushes each platform's manifest by digest, then the index naming them under the
// tag. The index is refused until its children are here, and served whole once they are.
func TestMultiPlatformIndexIsPushedAfterItsChildren(t *testing.T) {
	h := pushHarness(t, PushPolicy{Open: true}, nil)
	config := pushLayer(t, h, "app", []byte("{}"), nil)
	layer := pushLayer(t, h, "app", []byte("bytes"), nil)
	child := pushedManifest(config, layer)
	childDigest := blob.DigestBytes(child)
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",`+
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,`+
		`"platform":{"architecture":"amd64","os":"linux"}}]}`, childDigest.Prefixed(), len(child)))
	indexHeader := http.Header{"Content-Type": {"application/vnd.oci.image.index.v1+json"}}

	if got := send(t, h, http.MethodPut, "/v2/local/app/manifests/2.0", index, indexHeader); got.Status != http.StatusBadRequest ||
		!strings.Contains(got.Text(), "MANIFEST_UNKNOWN") {
		t.Fatalf("an index ahead of its children: %d %s", got.Status, got.Text())
	}
	if got := send(t, h, http.MethodPut, "/v2/local/app/manifests/"+childDigest.Prefixed(), child, manifestHeader()); got.Status != http.StatusCreated {
		t.Fatalf("push child: %d %s", got.Status, got.Text())
	}
	if got := send(t, h, http.MethodPut, "/v2/local/app/manifests/2.0", index, indexHeader); got.Status != http.StatusCreated {
		t.Fatalf("push index: %d %s", got.Status, got.Text())
	}
	if got := h.Get("/v2/local/app/manifests/2.0"); got.Status != http.StatusOK || !bytes.Equal(got.Body, index) {
		t.Fatalf("pull index: %d %s", got.Status, got.Text())
	}
	keys, err := companions(hostedTagKey("app", "2.0"), func() ([]byte, error) { return index, nil })
	if err != nil || len(keys) != 1 || keys[0] != hostedManifestKey(childDigest) {
		t.Fatalf("companions of a pushed index: %v %v", keys, err)
	}
}

// What was pushed is the only copy, so eviction must pass it by, and local/ is a name no
// project can be created under, since an image path reads its first segment as a project.
func TestPushedContentIsHostedAndItsNamespaceReserved(t *testing.T) {
	d := New().Descriptor()
	for key, want := range map[string]bool{
		hostedBlobKey(blob.DigestBytes([]byte("x"))):     true,
		hostedManifestKey(blob.DigestBytes([]byte("x"))): true,
		hostedTagKey("app", "1.0"):                       true,
		blobKey(blob.DigestBytes([]byte("x"))):           false,
		tagRefName("dockerhub", "library/alpine", "3"):   false,
	} {
		if got := d.Hosted(key); got != want {
			t.Errorf("Hosted(%q) = %v, want %v", key, got, want)
		}
	}
	if len(d.Reserved) != 1 || d.Reserved[0] != HostedNamespace {
		t.Fatalf("Reserved = %v", d.Reserved)
	}
}

// A machine behind a team pulls local/ from the team's own local/ when it holds nothing
// itself; one with no team has nowhere else to look.
func TestLocalPullsGoToTheTeamsLocal(t *testing.T) {
	alone := discoveryCtx(t, nil)
	if route, ok := resolveName(alone, "local/app"); !ok || !route.hosted || route.base != "" {
		t.Fatalf("with no team: %+v %v", route, ok)
	}
	behind := discoveryCtx(t, func(s *config.Snapshot) {
		s.ProjectUpstreams = map[string]map[string]map[string][]config.Endpoint{
			config.GlobalProject: {ID: {ociname.AnyRegistry: {{URL: "https://team:8443/v2/work"}}}},
		}
	})
	route, ok := resolveName(behind, "local/app")
	if !ok || route.base != "https://team:8443/v2/work/local" {
		t.Fatalf("behind a team: %+v %v", route, ok)
	}
	if got := manifestURL(route, "1.0"); got != "https://team:8443/v2/work/local/app/manifests/1.0" {
		t.Fatalf("team manifest URL %q", got)
	}
}
