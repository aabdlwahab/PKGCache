package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

const token = "test-write-token"

type staticTokens struct{ token string }

func (s staticTokens) HasToken(_, _, scope string) bool {
	return scope == "write" && s.token != ""
}

func (s staticTokens) VerifyToken(_, _, scope, token string) bool {
	return scope == "write" && token == s.token
}

func newHarness(t *testing.T) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return New(staticTokens{token: token}, 0)
	})
}

func put(h *ecotest.Harness, path, body string, opts ...func(*http.Request)) *ecotest.Response {
	h.T.Helper()
	req, err := http.NewRequest(http.MethodPut, h.URL(path), strings.NewReader(body))
	if err != nil {
		h.T.Fatalf("build PUT: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for _, o := range opts {
		o(req)
	}
	return h.Do(req)
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestUploadAndDownload(t *testing.T) {
	h := newHarness(t)
	const body = "build artifact bytes"

	resp := put(h, "/builds/v1/app.tar.gz", body)
	if resp.Status != http.StatusCreated {
		t.Fatalf("PUT status = %d: %s", resp.Status, resp.Text())
	}
	var created struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(resp.Body, &created); err != nil {
		t.Fatalf("PUT response is not JSON: %s", resp.Text())
	}
	if created.Path != "builds/v1/app.tar.gz" || created.Size != int64(len(body)) {
		t.Fatalf("PUT response = %+v", created)
	}
	if created.SHA256 != sha(body) {
		t.Fatalf("sha256 = %s, want %s", created.SHA256, sha(body))
	}
	// The URL handed back must include the project/eco prefix, or a client that
	// follows it lands somewhere that does not exist.
	if !strings.HasSuffix(created.URL, "/global/files/builds/v1/app.tar.gz") {
		t.Fatalf("url = %q, want the project-prefixed path", created.URL)
	}

	got := h.Get("/builds/v1/app.tar.gz")
	if got.Status != http.StatusOK || got.Text() != body {
		t.Fatalf("GET status=%d body=%q", got.Status, got.Text())
	}
	if ct := got.Header.Get("Content-Type"); !strings.Contains(ct, "gzip") &&
		ct != "application/octet-stream" {
		t.Logf("content type = %q", ct) // sniffed from the extension; informational
	}
}

func TestRangeAndConditional(t *testing.T) {
	h := newHarness(t)
	body := strings.Repeat("0123456789", 1000)
	put(h, "/big.bin", body)

	req, _ := http.NewRequest(http.MethodGet, h.URL("/big.bin"), nil)
	req.Header.Set("Range", "bytes=100-199")
	resp := h.Do(req)
	if resp.Status != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.Status)
	}
	if len(resp.Body) != 100 || string(resp.Body) != body[100:200] {
		t.Fatalf("range body wrong (%d bytes)", len(resp.Body))
	}

	req, _ = http.NewRequest(http.MethodGet, h.URL("/big.bin"), nil)
	req.Header.Set("If-None-Match", `"`+sha(body)+`"`)
	if resp := h.Do(req); resp.Status != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", resp.Status)
	}
}

// Write-once by default, so a repeated CI job cannot silently replace a released
// artifact.
func TestWriteOnceThenOverwrite(t *testing.T) {
	h := newHarness(t)
	put(h, "/a.txt", "first")

	if resp := put(h, "/a.txt", "second"); resp.Status != http.StatusConflict {
		t.Fatalf("second PUT status = %d, want 409", resp.Status)
	}
	if got := h.Get("/a.txt").Text(); got != "first" {
		t.Fatalf("content changed despite the conflict: %q", got)
	}

	resp := put(h, "/a.txt?overwrite=1", "second")
	if resp.Status != http.StatusOK {
		t.Fatalf("overwrite status = %d, want 200", resp.Status)
	}
	if got := h.Get("/a.txt").Text(); got != "second" {
		t.Fatalf("overwrite did not take: %q", got)
	}
}

func TestChecksumVerification(t *testing.T) {
	h := newHarness(t)

	resp := put(h, "/verified.bin", "payload", func(r *http.Request) {
		r.Header.Set("X-Checksum-Sha256", sha("payload"))
	})
	if resp.Status != http.StatusCreated {
		t.Fatalf("matching checksum rejected: %d %s", resp.Status, resp.Text())
	}

	resp = put(h, "/bad.bin", "payload", func(r *http.Request) {
		r.Header.Set("X-Checksum-Sha256", sha("something else"))
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("mismatched checksum status = %d, want 400", resp.Status)
	}
	if h.Get("/bad.bin").Status != http.StatusNotFound {
		t.Fatal("a checksum-mismatched upload was stored anyway")
	}

	resp = put(h, "/malformed.bin", "payload", func(r *http.Request) {
		r.Header.Set("X-Checksum-Sha256", "not-a-digest")
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("malformed checksum status = %d", resp.Status)
	}
}

func TestAuthorization(t *testing.T) {
	h := newHarness(t)

	t.Run("no token", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, h.URL("/x.txt"), strings.NewReader("x"))
		if resp := h.Do(req); resp.Status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.Status)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, h.URL("/x.txt"), strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer wrong")
		if resp := h.Do(req); resp.Status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.Status)
		}
	})

	t.Run("X-Auth-Token is accepted", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, h.URL("/y.txt"), strings.NewReader("y"))
		req.Header.Set("X-Auth-Token", token)
		if resp := h.Do(req); resp.Status != http.StatusCreated {
			t.Fatalf("status = %d: %s", resp.Status, resp.Text())
		}
	})

	t.Run("downloads stay anonymous", func(t *testing.T) {
		if resp := h.Get("/y.txt"); resp.Status != http.StatusOK {
			t.Fatalf("anonymous GET status = %d", resp.Status)
		}
	})
}

func TestNoTokenConfigured(t *testing.T) {
	h := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return New(staticTokens{token: ""}, 0)
	})
	req, _ := http.NewRequest(http.MethodPut, h.URL("/x.txt"), strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer anything")
	resp := h.Do(req)
	if resp.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if !strings.Contains(resp.Text(), "generate one in the console") {
		t.Fatalf("message should tell the operator what to do: %q", resp.Text())
	}
}

// The air-gapped side is a pure mirror: an upload there would be discarded by the
// next import, so it must be refused rather than silently lost.
func TestOfflineIsReadOnly(t *testing.T) {
	h := newHarness(t)
	put(h, "/before.txt", "cached")
	h.Offline(true)

	resp := put(h, "/after.txt", "should not land")
	if resp.Status != http.StatusForbidden {
		t.Fatalf("offline PUT status = %d, want 403", resp.Status)
	}
	const retiredMessage = "read-only: writes are disabled on the air-gapped (OFFLINE) side"
	if strings.TrimSpace(resp.Text()) != retiredMessage {
		t.Fatalf("offline message = %q, want retired contract %q", resp.Text(), retiredMessage)
	}
	if got := h.Get("/before.txt"); got.Status != http.StatusOK || got.Text() != "cached" {
		t.Fatal("offline mode must still serve what is already cached")
	}
}

func TestMaxBytes(t *testing.T) {
	h := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return New(staticTokens{token: token}, 10)
	})
	if resp := put(h, "/small.bin", "under"); resp.Status != http.StatusCreated {
		t.Fatalf("under the cap was rejected: %d", resp.Status)
	}
	resp := put(h, "/big.bin", strings.Repeat("x", 100))
	if resp.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap status = %d, want 413", resp.Status)
	}
	if h.Get("/big.bin").Status != http.StatusNotFound {
		t.Fatal("an oversized upload was stored anyway")
	}
}

func TestDelete(t *testing.T) {
	h := newHarness(t)
	put(h, "/gone.txt", "bytes")

	req, _ := http.NewRequest(http.MethodDelete, h.URL("/gone.txt"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp := h.Do(req); resp.Status != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", resp.Status)
	}
	if h.Get("/gone.txt").Status != http.StatusNotFound {
		t.Fatal("content survived DELETE")
	}

	// Deleting again is a 404, and deleting without a token is refused.
	if resp := h.Do(req); resp.Status != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d", resp.Status)
	}
	unauth, _ := http.NewRequest(http.MethodDelete, h.URL("/x"), nil)
	if resp := h.Do(unauth); resp.Status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated DELETE status = %d", resp.Status)
	}
}

// The listing is a catalog query, not a readdir, so it can never show something the
// cache would then fail to serve.
func TestAutoindex(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{
		"/builds/v1/app.tar.gz",
		"/builds/v1/app.sha256",
		"/builds/v2/app.tar.gz",
		"/readme.txt",
	} {
		put(h, p, "content of "+p)
	}

	root := h.Get("/")
	if root.Status != http.StatusOK {
		t.Fatalf("root listing status = %d", root.Status)
	}
	if !strings.Contains(root.Text(), `<a href="builds/">builds/</a>`) {
		t.Fatalf("root listing missing the builds directory:\n%s", root.Text())
	}
	if !strings.Contains(root.Text(), `<a href="readme.txt">readme.txt</a>`) {
		t.Fatalf("root listing missing readme.txt:\n%s", root.Text())
	}
	// One level only: a nested file must not appear in the root listing.
	if strings.Contains(root.Text(), "app.tar.gz") {
		t.Fatalf("root listing leaked a nested file:\n%s", root.Text())
	}

	sub := h.Get("/builds/v1/")
	if !strings.Contains(sub.Text(), `<a href="app.tar.gz">`) ||
		!strings.Contains(sub.Text(), `<a href="app.sha256">`) {
		t.Fatalf("subdirectory listing wrong:\n%s", sub.Text())
	}
	if !strings.Contains(sub.Text(), `<a href="../">../</a>`) {
		t.Fatal("subdirectory listing needs a parent link for wget -r")
	}

	if h.Get("/nothing-here/").Status != http.StatusNotFound {
		t.Fatal("an empty prefix should 404, not render an empty listing")
	}
}

func TestEmptyRootAutoindex(t *testing.T) {
	h := newHarness(t)
	root := h.Get("/")
	if root.Status != http.StatusOK {
		t.Fatalf("empty root listing status = %d, want 200", root.Status)
	}
	if !strings.Contains(root.Text(), "<title>Index of /</title>") {
		t.Fatalf("empty root listing body is not an autoindex:\n%s", root.Text())
	}
	if !strings.Contains(root.Text(), "<pre>\n\n</pre>") {
		t.Fatalf("empty root listing does not preserve the retired body contract:\n%s", root.Text())
	}
	if h.Get("/nothing-here/").Status != http.StatusNotFound {
		t.Fatal("an unknown nested prefix should still return 404")
	}
}

// A file must win over a directory of the same name.
func TestFileBeatsDirectory(t *testing.T) {
	h := newHarness(t)
	put(h, "/thing", "i am a file")
	put(h, "/thing/nested", "i am nested")

	if got := h.Get("/thing"); got.Text() != "i am a file" {
		t.Fatalf("exact match lost to the listing: %q", got.Text())
	}
	if got := h.Get("/thing/"); !strings.Contains(got.Text(), "nested") {
		t.Fatalf("trailing slash should list: %q", got.Text())
	}
}

func TestAutoindexEscapesHTML(t *testing.T) {
	h := newHarness(t)
	put(h, "/%3Cscript%3E.txt", "xss")

	body := h.Get("/").Text()
	if strings.Contains(body, "<script>.txt<") {
		t.Fatalf("filename was not escaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected an escaped filename:\n%s", body)
	}
}

func TestReservedAndInvalidPaths(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/ledger.db", // the role's own storage
		"/ledger.db-wal",
		"/catalog.db",   //
		"/upload.part",  // staging namespace from the Python layout
		"/+admin/thing", // the administrative namespace
		"/a/../b",       // traversal
		"/./x",          //
	} {
		t.Run(path, func(t *testing.T) {
			if resp := put(h, path, "x"); resp.Status < 400 {
				t.Fatalf("PUT %s was accepted (status %d)", path, resp.Status)
			}
		})
	}
	if resp := put(h, "/", "x"); resp.Status != http.StatusBadRequest {
		t.Fatalf("PUT to the root status = %d, want 400", resp.Status)
	}
	if resp := put(h, "/dir/", "x"); resp.Status != http.StatusBadRequest {
		t.Fatalf("PUT to a directory status = %d, want 400", resp.Status)
	}
}

func TestInventoryRecorded(t *testing.T) {
	h := newHarness(t)
	put(h, "/builds/app.tar.gz", "artifact")
	h.Flush()

	arts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: "global", Eco: ID})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if total != 1 || arts[0].Name != "builds/app.tar.gz" {
		t.Fatalf("inventory = %+v (total %d)", arts, total)
	}
	if arts[0].Digest.String() != sha("artifact") {
		t.Fatal("inventory digest does not match the content")
	}
	if !strings.HasPrefix(arts[0].Origin, "upload:") {
		t.Fatalf("origin = %q, want it to record the uploader", arts[0].Origin)
	}
}

// Re-uploading changed content must replace the inventory row, not accumulate one
// per historical digest.
func TestOverwriteReplacesInventory(t *testing.T) {
	h := newHarness(t)
	put(h, "/app.bin", "v1")
	put(h, "/app.bin?overwrite=1", "v2")
	h.Flush()

	arts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: "global", Eco: ID})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if total != 1 {
		t.Fatalf("inventory has %d rows, want 1: %+v", total, arts)
	}
	if arts[0].Digest.String() != sha("v2") {
		t.Fatal("inventory still points at the replaced content")
	}
}

// Identical uploads to different projects are one blob on disk.
func TestCrossProjectDedup(t *testing.T) {
	h := newHarness(t)
	const body = "shared build output"
	put(h, "/a.bin", body)
	h.SetProject("team-a")
	put(h, "/b.bin", body)
	h.Flush()

	count, _, err := h.Blobs.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if count != 1 {
		t.Fatalf("blob count = %d, want 1 for identical uploads in two projects", count)
	}
	if got := h.Get("/b.bin"); got.Text() != body {
		t.Fatalf("team-a content = %q", got.Text())
	}
	h.SetProject("global")
	if got := h.Get("/a.bin"); got.Text() != body {
		t.Fatalf("global content = %q", got.Text())
	}
	// Each project sees only its own keys.
	if h.Get("/b.bin").Status != http.StatusNotFound {
		t.Fatal("a key leaked across projects")
	}
}

func TestDescriptorIsValid(t *testing.T) {
	r := eco.NewRegistry()
	r.Register(New(staticTokens{}, 0))
	if r.Len() != 1 {
		t.Fatalf("registry len = %d", r.Len())
	}
	d := r.Descriptors()[0]
	if d.ID != ID || d.Storage != eco.StorageBlob || d.Listener != eco.ListenerPathPrefixed {
		t.Fatalf("descriptor = %+v", d)
	}
	if !d.FreshnessFor("anything").Immutable {
		t.Fatal("uploaded content should be immutable")
	}
	name, _, _, ok := d.Artifact("builds/app.tar.gz")
	if !ok || name != "builds/app.tar.gz" {
		t.Fatalf("Artifact = %q %v", name, ok)
	}
	steps := d.SetupSteps(eco.SetupContext{
		Host: "cache.example.com", Port: 8443, Project: "global",
		CAPath: "/etc/ssl/ca.crt", IsGlobal: true,
	})
	if len(steps) == 0 {
		t.Fatal("setup instructions are what let the console render this ecosystem")
	}
	joined := renderSteps(steps)
	if !strings.Contains(joined, "cache.example.com:8443/global/files") {
		t.Fatalf("setup steps do not reference the real endpoint:\n%s", joined)
	}
}

func renderSteps(steps []eco.SetupStep) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Comment)
		b.WriteString(s.Command)
		b.WriteString("\n")
	}
	return b.String()
}
