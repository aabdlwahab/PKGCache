package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Milestone M1 — "pkgreg stores, dedups and serves a blob with Range support;
// metrics live."
//
// This is the foundation's end-to-end proof: the blob store, the catalog, the
// configuration snapshot and the observability layer working together through real
// HTTP, with no ecosystem adapters involved yet.

func newApp(t *testing.T) *App {
	t.Helper()
	snap := config.Defaults()
	snap.DataDir = t.TempDir()
	snap.Log.Level = "error" // keep test output readable
	if err := snap.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	a, err := Open(&snap)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// store writes content through the blob store and records the catalog entry, which
// is the same two-step ordering the fetch path will use: bytes durable first, row
// second.
func store(t *testing.T, a *App, project, eco, key string, content []byte) blob.Digest {
	t.Helper()
	w, err := a.Blobs.Create()
	if err != nil {
		t.Fatalf("Blobs.Create: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	d, n, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	err = a.Catalog.PutEntry(catalog.Entry{
		EntryKey:  catalog.EntryKey{Project: project, Eco: eco, Key: key},
		Digest:    d,
		Size:      n,
		MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	return d
}

func TestM1StoreDedupAndServe(t *testing.T) {
	a := newApp(t)

	// A 3 MiB body, large enough that Range serving is doing real work.
	content := bytes.Repeat([]byte("pkgreg-milestone-1!"), 3<<20/19)
	sum := sha256.Sum256(content)
	want := blob.Digest(hex.EncodeToString(sum[:]))

	// ---- store ------------------------------------------------------------
	got := store(t, a, "global", "pypi", "root/pypi/+f/torch/torch-2.6.0.whl", content)
	if got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}

	// ---- dedup ------------------------------------------------------------
	// Three projects cache the same artifact. Under the old path-first layout that
	// was three copies on disk unless the digest happened to be known in advance;
	// here it is one blob, because every write is hashed as it streams.
	store(t, a, "team-a", "pypi", "root/pypi/+f/torch/torch-2.6.0.whl", content)
	store(t, a, "team-b", "npm", "torch/-/torch-2.6.0.tgz", content)

	if err := a.Catalog.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	count, bytesHeld, err := a.Blobs.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if count != 1 {
		t.Fatalf("blob count = %d, want 1 — three projects hold identical bytes", count)
	}
	if bytesHeld != int64(len(content)) {
		t.Fatalf("bytes on disk = %d, want %d (stored once)", bytesHeld, len(content))
	}

	// Logical size still accounts to each project in full, which is what makes
	// honest per-team reporting possible.
	for _, p := range []string{"global", "team-a", "team-b"} {
		n, logical, err := a.Catalog.CountEntries(p)
		if err != nil {
			t.Fatalf("CountEntries(%s): %v", p, err)
		}
		if n != 1 || logical != int64(len(content)) {
			t.Fatalf("%s: entries=%d logical=%d", p, n, logical)
		}
	}

	// ---- serve, whole body ------------------------------------------------
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := blob.Serve(w, r, a.Blobs, want, "application/octet-stream"); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(body), len(content))
	}
	if etag := resp.Header.Get("ETag"); etag != `"`+string(want)+`"` {
		t.Fatalf("ETag = %q, want the digest", etag)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("Accept-Ranges not advertised")
	}

	// ---- serve, Range -----------------------------------------------------
	// This is the devpi defect the original project existed to fix: without Range,
	// a resumed download of a multi-GB wheel starts over from zero.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	partial, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if len(partial) != 1000 || !bytes.Equal(partial, content[1000:2000]) {
		t.Fatalf("range body wrong: %d bytes", len(partial))
	}
	wantCR := fmt.Sprintf("bytes 1000-1999/%d", len(content))
	if cr := resp.Header.Get("Content-Range"); cr != wantCR {
		t.Fatalf("Content-Range = %q, want %q", cr, wantCR)
	}

	// ---- serve, conditional ------------------------------------------------
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", `"`+string(want)+`"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for a matching ETag", resp.StatusCode)
	}

	// ---- HEAD --------------------------------------------------------------
	resp, err = http.Head(srv.URL)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.ContentLength != int64(len(content)) {
		t.Fatalf("HEAD Content-Length = %d, want %d", resp.ContentLength, len(content))
	}
}

func TestM1AdminSurface(t *testing.T) {
	a := newApp(t)
	srv := httptest.NewServer(a.AdminHandler())
	defer srv.Close()

	t.Run("healthz", func(t *testing.T) {
		body, status := get(t, srv.URL+"/healthz")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil || got["status"] != "ok" {
			t.Fatalf("body = %s", body)
		}
	})

	t.Run("readyz probes dependencies", func(t *testing.T) {
		body, status := get(t, srv.URL+"/readyz")
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", status, body)
		}
		var got struct {
			Ready  bool              `json:"ready"`
			Checks map[string]string `json:"checks"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !got.Ready || got.Checks["catalog"] != "ok" || got.Checks["blobs"] != "ok" {
			t.Fatalf("readiness = %+v", got)
		}
	})

	t.Run("metrics are live", func(t *testing.T) {
		// A *Vec contributes nothing to a scrape until some label combination
		// exists, so asserting on the bare metric name would prove nothing.
		// Requests is pre-created at startup for the global project (so dashboards
		// read 0, not "no data"); CatalogQuery has no bounded label set and is
		// exercised here instead.
		a.Metrics.Requests.WithLabelValues("pypi", "global", "hit").Inc()
		a.Metrics.CatalogQuery.WithLabelValues("get_entry").Observe(0.00004)

		body, status := get(t, srv.URL+"/metrics")
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		text := string(body)
		for _, want := range []string{
			"pkgreg_build_info",
			"pkgreg_blob_store_bytes",
			`pkgreg_requests_total{eco="pypi",outcome="hit",project="global"} 1`,
			// Pre-created at startup, still zero: the "no data vs 0" fix.
			`pkgreg_requests_total{eco="npm",outcome="fail",project="global"} 0`,
			"pkgreg_catalog_query_seconds_bucket",
			"go_goroutines",    // runtime collector registered
			"process_open_fds", // process collector registered
		} {
			if !strings.Contains(text, want) {
				t.Errorf("metric %q missing from /metrics", want)
			}
		}
	})

	t.Run("security headers", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := resp.Header.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
	})
}

// Storage metrics must reflect what is actually on disk, not what was requested.
func TestM1StorageMetrics(t *testing.T) {
	a := newApp(t)
	content := []byte("metrics check")
	store(t, a, "global", "files", "a.txt", content)
	store(t, a, "global", "files", "b.txt", content) // same bytes: still one blob

	a.refreshStorageMetrics()
	srv := httptest.NewServer(a.AdminHandler())
	defer srv.Close()

	body, _ := get(t, srv.URL+"/metrics")
	text := string(body)
	if !strings.Contains(text, "pkgreg_blob_count 1") {
		t.Fatalf("expected exactly one blob to be reported:\n%s", grep(text, "pkgreg_blob"))
	}
	if !strings.Contains(text, fmt.Sprintf("pkgreg_blob_store_bytes %d", len(content))) {
		t.Fatalf("store size wrong:\n%s", grep(text, "pkgreg_blob"))
	}
}

// A staging file from a previous kill -9 must be swept before anything serves, and a
// committed blob must survive that sweep.
func TestM1RecoversFromInterruptedWrite(t *testing.T) {
	snap := config.Defaults()
	snap.DataDir = t.TempDir()
	snap.Log.Level = "error"

	first, err := Open(&snap)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	keep := store(t, first, "global", "files", "keep.txt", []byte("committed"))
	// Simulate a crash: a writer that never committed, and no Close.
	orphan, err := first.Blobs.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := orphan.Write([]byte("half a wheel")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Catalog.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(&snap)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	if !second.Blobs.Exists(keep) {
		t.Fatal("recovery removed a committed blob")
	}
	count, _, err := second.Blobs.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if count != 1 {
		t.Fatalf("blob count = %d, want 1 — the interrupted write must not be published", count)
	}
	if _, err := second.Catalog.GetEntry(catalog.EntryKey{
		Project: "global", Eco: "files", Key: "keep.txt",
	}); err != nil {
		t.Fatalf("catalog did not survive the restart: %v", err)
	}
}

func get(t *testing.T, url string) ([]byte, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode
}

func grep(text, substr string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
