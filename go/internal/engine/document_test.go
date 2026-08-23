package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func (h *harness) doc(path string, ttl time.Duration) DocSpec {
	return DocSpec{
		Project: "global",
		Eco:     "pypi",
		Name:    "simple" + path,
		URL:     h.origin.URLFor(path),
		TTL:     ttl,
	}
}

func TestDocumentFetchThenCached(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"files":[]}`)
	h.origin.Handle("/simple/numpy/", testupstream.Behaviour{
		Body: body, ContentType: "application/vnd.pypi.simple.v1+json",
	})
	spec := h.doc("/simple/numpy/", time.Hour)

	first, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if string(first.Body) != string(body) || first.FromCache {
		t.Fatalf("first = %+v", first)
	}
	if first.MediaType != "application/vnd.pypi.simple.v1+json" {
		t.Fatalf("media type = %q", first.MediaType)
	}

	second, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.FromCache {
		t.Fatal("a fresh document must be served without touching upstream")
	}
	if h.origin.Hits("/simple/numpy/") != 1 {
		t.Fatalf("upstream hit %d times, want 1", h.origin.Hits("/simple/numpy/"))
	}
}

// A 304 keeps the bytes and just restarts the freshness clock — which is what makes
// a short TTL cheap instead of wasteful.
func TestDocumentRevalidatesWithETag(t *testing.T) {
	h := newHarness(t)
	body := []byte("index v1")
	h.origin.Handle("/simple/chalk/", testupstream.Behaviour{Body: body, ETag: `"v1"`})
	spec := h.doc("/simple/chalk/", 0) // always revalidate

	if _, err := h.engine.Document(context.Background(), spec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if !got.Revalidated {
		t.Fatal("expected a 304 revalidation")
	}
	if string(got.Body) != string(body) {
		t.Fatalf("body changed across a 304: %q", got.Body)
	}
	if h.origin.Hits("/simple/chalk/") != 2 {
		t.Fatalf("upstream hit %d times, want 2 (fetch + revalidate)", h.origin.Hits("/simple/chalk/"))
	}
}

func TestDocumentRevalidatesWithLastModified(t *testing.T) {
	h := newHarness(t)
	const lm = "Mon, 02 Jan 2006 15:04:05 GMT"
	h.origin.Handle("/rel/InRelease", testupstream.Behaviour{Body: []byte("Release"), LastModified: lm})
	spec := DocSpec{Project: "global", Eco: "apt", Name: "InRelease", URL: h.origin.URLFor("/rel/InRelease")}

	if _, err := h.engine.Document(context.Background(), spec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if !got.Revalidated {
		t.Fatal("expected If-Modified-Since to produce a 304")
	}
}

// Changed content must replace the cached copy.
func TestDocumentPicksUpChanges(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/simple/x/", testupstream.Behaviour{Body: []byte("v1"), ETag: `"1"`})
	spec := h.doc("/simple/x/", 0)

	if _, err := h.engine.Document(context.Background(), spec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.origin.Handle("/simple/x/", testupstream.Behaviour{Body: []byte("v2"), ETag: `"2"`})

	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if string(got.Body) != "v2" || got.Revalidated {
		t.Fatalf("got %+v, want the new body", got)
	}
}

// Availability beats freshness for an index: a build that succeeds against a
// slightly old index is better than one that fails because the origin blipped.
func TestDocumentServesStaleWhenUpstreamFails(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/simple/y/", testupstream.Behaviour{Body: []byte("good")})
	spec := h.doc("/simple/y/", 0)
	if _, err := h.engine.Document(context.Background(), spec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.origin.Handle("/simple/y/", testupstream.Behaviour{Status: 503})
	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("should have fallen back to cache: %v", err)
	}
	if !got.Stale || string(got.Body) != "good" {
		t.Fatalf("got %+v, want the stale cached copy", got)
	}
}

func TestDocumentFailsWhenNothingCached(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/simple/z/", testupstream.Behaviour{Status: 500})
	_, err := h.engine.Document(context.Background(), h.doc("/simple/z/", 0))
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("err = %v, want ErrUpstreamStatus", err)
	}
	if status, ok := UpstreamStatus(err); !ok || status != http.StatusInternalServerError {
		t.Fatalf("upstream status = %d, %v; want 500, true", status, ok)
	}
}

func TestDocumentOfflineServesLastKnown(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/simple/off/", testupstream.Behaviour{Body: []byte("cached index")})
	spec := h.doc("/simple/off/", 0)
	if _, err := h.engine.Document(context.Background(), spec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = true
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := h.origin.Requests.Load()

	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("offline document: %v", err)
	}
	if !got.Stale || !got.FromCache || string(got.Body) != "cached index" {
		t.Fatalf("got %+v", got)
	}
	if h.origin.Requests.Load() != before {
		t.Fatal("offline mode contacted upstream")
	}
}

func TestDocumentOfflineWithNothingCached(t *testing.T) {
	h := newHarness(t)
	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = true
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	_, err := h.engine.Document(context.Background(), h.doc("/simple/nope/", 0))
	if !errors.Is(err, ErrNotCached) {
		t.Fatalf("err = %v, want ErrNotCached", err)
	}
}

func TestImmutableDocumentIsVerifiedAndNeverRevalidated(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(body)
	digest := blob.Digest(fmt.Sprintf("%x", sum))
	h.origin.Handle("/v2/x/manifests/sha256:"+digest.String(),
		testupstream.Behaviour{Body: body})
	spec := DocSpec{
		Project: "global", Eco: "oci",
		Name:      "manifest/" + digest.String(),
		URL:       h.origin.URLFor("/v2/x/manifests/sha256:" + digest.String()),
		Immutable: true,
		Expect:    Expect{Digest: digest, Size: int64(len(body))},
	}

	for range 2 {
		got, err := h.engine.Document(context.Background(), spec)
		if err != nil || string(got.Body) != string(body) {
			t.Fatalf("Document: body=%q err=%v", got.Body, err)
		}
	}
	if hits := h.origin.Hits("/v2/x/manifests/sha256:" + digest.String()); hits != 1 {
		t.Fatalf("immutable document fetched %d times, want 1", hits)
	}
}

func TestDocumentRejectsExpectedDigestBeforePublication(t *testing.T) {
	h := newHarness(t)
	body := []byte("wrong manifest")
	wantSum := sha256.Sum256([]byte("expected manifest"))
	want := blob.Digest(fmt.Sprintf("%x", wantSum))
	h.origin.Serve("/manifest", body)
	spec := DocSpec{
		Project: "global", Eco: "oci", Name: "manifest/bad",
		URL: h.origin.URLFor("/manifest"), Immutable: true,
		Expect: Expect{Digest: want},
	}

	if _, err := h.engine.Document(context.Background(), spec); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if _, err := h.cat.GetEntry(catalog.EntryKey{
		Project: "global", Eco: "oci", Key: "manifest/bad",
	}); err == nil {
		t.Fatal("digest-mismatched document was published")
	}
}

// The production stall this design exists to prevent: uv requests an index and then
// every file in it, and concurrent requests must not each re-fetch and re-parse a
// 6 MB document.
func TestDocumentSingleFlight(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("index-entry-", 1<<20)
	h.origin.Handle("/simple/grpcio/", testupstream.Behaviour{
		Body: body, ChunkSize: 64 << 10, DelayPerChunk: time.Millisecond,
	})
	spec := h.doc("/simple/grpcio/", time.Hour)

	const callers = 24
	var wg sync.WaitGroup
	errs := make([]error, callers)
	lens := make([]int, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := h.engine.Document(context.Background(), spec)
			errs[i] = err
			if d != nil {
				lens[i] = len(d.Body)
			}
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if lens[i] != len(body) {
			t.Fatalf("caller %d got %d bytes, want %d", i, lens[i], len(body))
		}
	}
	if n := h.origin.Hits("/simple/grpcio/"); n != 1 {
		t.Fatalf("upstream fetched %d times for %d concurrent callers, want 1", n, callers)
	}
}

// Documents are blobs, so an index shared between two projects is stored once.
func TestDocumentDedupsAcrossProjects(t *testing.T) {
	h := newHarness(t)
	body := []byte("identical index bytes")
	h.origin.Handle("/simple/shared/", testupstream.Behaviour{Body: body})

	for _, project := range []string{"global", "team-a"} {
		spec := h.doc("/simple/shared/", time.Hour)
		spec.Project = project
		if _, err := h.engine.Document(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", project, err)
		}
	}
	if err := h.cat.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var blobs int
	if err := h.cat.WalkBlobs(func(catalog.Blob) error { blobs++; return nil }); err != nil {
		t.Fatalf("WalkBlobs: %v", err)
	}
	if blobs != 1 {
		t.Fatalf("blob rows = %d, want 1 for identical documents in two projects", blobs)
	}
}

func TestDocumentRejectsOversizedBody(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/simple/huge/", testupstream.Behaviour{Body: testupstream.Repeat("x", 5000)})
	spec := h.doc("/simple/huge/", 0)
	spec.MaxBytes = 1000

	if _, err := h.engine.Document(context.Background(), spec); err == nil {
		t.Fatal("a document over the cap must be rejected, not buffered")
	}
}

// A cached document whose blob was collected must self-heal rather than fail.
func TestDocumentHealsAfterBlobLoss(t *testing.T) {
	h := newHarness(t)
	body := []byte("recoverable")
	h.origin.Handle("/simple/heal/", testupstream.Behaviour{Body: body})
	spec := h.doc("/simple/heal/", time.Hour)

	first, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.blobs.Delete(first.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := h.engine.Document(context.Background(), spec)
	if err != nil {
		t.Fatalf("did not heal: %v", err)
	}
	if string(got.Body) != string(body) {
		t.Fatalf("healed body = %q", got.Body)
	}
}

// ---- refs -----------------------------------------------------------------
//
// One Ref type replaces the OCI oci_tags table, the git git_refs table, apt's .meta
// sidecar files and npm's re-fetch-every-time. These assert the shape works for the
// cases each of those served.

func TestRefRoundTrip(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		ref  catalog.Ref
	}{
		{
			name: "oci tag",
			ref: catalog.Ref{
				RefKey: catalog.RefKey{Project: "global", Eco: "oci", Name: "dockerhub/library/alpine:3.20"},
				Target: "sha256:" + string(digestOf([]byte("manifest"))),
				TTL:    5 * time.Minute,
			},
		},
		{
			name: "git branch",
			ref: catalog.Ref{
				RefKey: catalog.RefKey{Project: "global", Eco: "git", Name: "github.com/a/b:refs/heads/main"},
				Target: "0123456789abcdef0123456789abcdef01234567",
				TTL:    time.Minute,
			},
		},
		{
			name: "npm dist-tag",
			ref: catalog.Ref{
				RefKey: catalog.RefKey{Project: "global", Eco: "npm", Name: "chalk@latest"},
				Target: "5.3.0",
				TTL:    time.Minute,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := h.engine.SetRef(c.ref); err != nil {
				t.Fatalf("SetRef: %v", err)
			}
			got, ok := h.engine.Ref(c.ref.Project, c.ref.Eco, c.ref.Name)
			if !ok {
				t.Fatal("ref not found after SetRef")
			}
			if got.Target != c.ref.Target {
				t.Fatalf("target = %q, want %q", got.Target, c.ref.Target)
			}
			if !got.Fresh(time.Now()) {
				t.Fatal("a just-written ref should be fresh")
			}
			if got.Fresh(time.Now().Add(2 * c.ref.TTL)) {
				t.Fatal("ref should be stale past its TTL")
			}
		})
	}
}

// Listing a mirror's refs is how the offline side answers "what do we hold?".
func TestListRefsByPrefix(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{
		"dockerhub/library/alpine:3.19",
		"dockerhub/library/alpine:3.20",
		"ghcr/org/tool:v1",
	} {
		if err := h.engine.SetRef(catalog.Ref{
			RefKey: catalog.RefKey{Project: "global", Eco: "oci", Name: name},
			Target: "sha256:x", TTL: time.Minute,
		}); err != nil {
			t.Fatalf("SetRef: %v", err)
		}
	}
	got, err := h.engine.ListRefs("global", "oci", "dockerhub/library/alpine:")
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got), got)
	}
}

func TestPutBytesIsServable(t *testing.T) {
	h := newHarness(t)
	body := []byte("<html>generated index</html>")
	if _, err := h.engine.PutBytes("global", "pypi", "generated/index.html", body, "text/html"); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	res := Resolution{Project: "global", Eco: "pypi", Key: "generated/index.html"}
	rec, outcome, err := h.serve(t, get("/generated"), res)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if outcome != OutcomeHit {
		t.Fatalf("outcome = %s, want hit", outcome)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// A cache-only resolution with no upstream is a clean miss, not a panic or a hang.
func TestCacheOnlyResolutionMisses(t *testing.T) {
	h := newHarness(t)
	res := Resolution{Project: "global", Eco: "files", Key: "never/uploaded"}
	_, outcome, err := h.serve(t, get("/never"), res)
	if !errors.Is(err, ErrNotCached) {
		t.Fatalf("err = %v, want ErrNotCached", err)
	}
	if outcome != OutcomeFail {
		t.Fatalf("outcome = %s", outcome)
	}
}

func TestDocumentDistinctNamesDoNotCollide(t *testing.T) {
	h := newHarness(t)
	for i := range 4 {
		path := fmt.Sprintf("/simple/pkg%d/", i)
		h.origin.Handle(path, testupstream.Behaviour{Body: []byte(fmt.Sprintf("index-%d", i))})
	}
	for i := range 4 {
		spec := h.doc(fmt.Sprintf("/simple/pkg%d/", i), time.Hour)
		got, err := h.engine.Document(context.Background(), spec)
		if err != nil {
			t.Fatalf("pkg%d: %v", i, err)
		}
		if want := fmt.Sprintf("index-%d", i); string(got.Body) != want {
			t.Fatalf("pkg%d body = %q, want %q", i, got.Body, want)
		}
	}
}
