package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

func decodeInto(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
}

// seedSeries writes traffic straight through the catalog. The engine path is covered
// in the engine's own tests; here the question is what the HTTP surface does with it.
func seedSeries(t *testing.T, a *App, at time.Time) {
	t.Helper()
	if err := a.Catalog.RecordSeries([]catalog.SeriesDelta{
		{Bucket: at, Project: "global", Eco: "pypi", Outcome: "hit", Count: 7, Bytes: 700},
		{Bucket: at, Project: "global", Eco: "pypi", Outcome: "miss", Count: 3, Bytes: 300},
		{Bucket: at, Project: "global", Eco: "npm", Outcome: "peer", Count: 1, Bytes: 100},
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTrafficSeriesEndpointGroupsAndReportsItsSpan(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)
	seedSeries(t, a, time.Now().Add(-10*time.Minute))

	response, body := controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/series?project=global&by=eco,outcome", cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("series = %d %s", response.StatusCode, body)
	}
	var payload struct {
		Span   int64                  `json:"span"`
		Points []catalog.TrafficPoint `json:"points"`
		Gaps   bool                   `json:"gaps_are_unknown"`
	}
	decodeInto(t, body, &payload)

	if payload.Span != catalog.SpanFine {
		t.Fatalf("span = %d, want %d for a recent window", payload.Span, catalog.SpanFine)
	}
	if len(payload.Points) != 3 {
		t.Fatalf("got %d points, want 3: %+v", len(payload.Points), payload.Points)
	}
	if !payload.Gaps {
		t.Fatal("the response does not declare that absent buckets are unknown")
	}
	// The five outcomes are the whole reason the table exists; losing them here would
	// put the console back on a bare hit rate.
	outcomes := map[string]bool{}
	for _, point := range payload.Points {
		outcomes[point.Outcome] = true
	}
	for _, want := range []string{"hit", "miss", "peer"} {
		if !outcomes[want] {
			t.Fatalf("outcome %q missing from %+v", want, payload.Points)
		}
	}
}

// Asking for five-minute detail over a month is not an error — those buckets were
// folded away by design. The server answers at the resolution that exists and says
// which one, rather than returning an empty array that reads as an outage.
func TestSeriesDowngradesSpanForAnOldWindowInsteadOfFailing(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	from := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	response, body := controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/series?project=global&span=5m&from="+from, cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("series = %d %s", response.StatusCode, body)
	}
	var payload struct {
		Span int64 `json:"span"`
	}
	decodeInto(t, body, &payload)
	if payload.Span != catalog.SpanDay {
		t.Fatalf("span = %d, want daily for a 90-day window", payload.Span)
	}

	// A span that is not one of the three is still a client error.
	response, _ = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/series?span=7s", cookie, "")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad span = %d, want 400", response.StatusCode)
	}
	// So is a backwards window.
	response, _ = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/series?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z",
		cookie, "")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("reversed window = %d, want 400", response.StatusCode)
	}
}

func TestStatsCarriesStorageAndDedupSavings(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	// The same body under two names: two entries, one blob.
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := a.Engine.PutBytes("global", "files", name,
			[]byte("identical bytes"), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}

	response, body := controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats?project=global", cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stats = %d %s", response.StatusCode, body)
	}
	var payload struct {
		TotalBlobs int64 `json:"total_blobs"`
		Storage    struct {
			BlobCount    int64 `json:"blob_count"`
			BlobBytes    int64 `json:"blob_bytes"`
			EntryCount   int64 `json:"entry_count"`
			EntryBytes   int64 `json:"entry_bytes"`
			FSFree       int64 `json:"fs_free"`
			FSTotal      int64 `json:"fs_total"`
			MinFreeBytes int64 `json:"min_free_bytes"`
		} `json:"storage"`
	}
	decodeInto(t, body, &payload)

	if payload.Storage.EntryCount != 2 || payload.Storage.BlobCount != 1 {
		t.Fatalf("entries=%d blobs=%d, want 2 and 1",
			payload.Storage.EntryCount, payload.Storage.BlobCount)
	}
	if payload.Storage.EntryBytes <= payload.Storage.BlobBytes {
		t.Fatalf("dedup saving is invisible: logical %d, stored %d",
			payload.Storage.EntryBytes, payload.Storage.BlobBytes)
	}
	if payload.Storage.FSTotal <= 0 || payload.Storage.FSFree <= 0 {
		t.Fatalf("no filesystem reading: %+v", payload.Storage)
	}
	// The existing fields still have to be there — the storage object is additive.
	if payload.TotalBlobs == 0 {
		t.Fatal("the pre-existing stats payload lost total_blobs")
	}
}

func TestStorageSeriesReturnsSamplesAndAFreshCurrentReading(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	if err := a.Catalog.SampleStorage(catalog.StorageSample{
		Bucket: time.Now().Add(-2 * time.Hour), BlobCount: 5, BlobBytes: 500,
		FSFree: 100, FSTotal: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	response, body := controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/storage", cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("storage = %d %s", response.StatusCode, body)
	}
	var payload struct {
		Samples []catalog.StorageSample `json:"samples"`
		Current struct {
			FSTotal int64 `json:"fs_total"`
		} `json:"current"`
	}
	decodeInto(t, body, &payload)
	// Two points: the one seeded above, and the one the maintenance service takes at
	// startup so a fresh instance is not a blank chart for its first hour.
	if len(payload.Samples) != 2 {
		t.Fatalf("got %d samples, want the seeded one plus the startup sample: %+v",
			len(payload.Samples), payload.Samples)
	}
	if payload.Samples[0].BlobBytes != 500 {
		t.Fatalf("the seeded historical sample is missing or out of order: %+v", payload.Samples)
	}
	// History is hourly; "current" is measured on this request, so it must not be the
	// stubbed sample above.
	if payload.Current.FSTotal == 1000 {
		t.Fatal("current reading came from the sample table instead of the filesystem")
	}
	if payload.Current.FSTotal <= 0 {
		t.Fatal("current reading is missing")
	}
}

func TestEntryAgesAndUpstreamSeriesEndpoints(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	if _, err := a.Engine.PutBytes("global", "files", "warm.txt",
		[]byte("warm"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := a.Catalog.RecordSeries(nil, []catalog.UpstreamDelta{{
		Bucket: time.Now(), Project: "global", Upstream: "pypi.org",
		Requests: 4, Errors: 1, Bytes: 4000, MillisSum: 800, MillisMax: 500,
	}}); err != nil {
		t.Fatal(err)
	}

	response, body := controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/ages?project=global", cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ages = %d %s", response.StatusCode, body)
	}
	var ages struct {
		Buckets []catalog.AgeBucket `json:"buckets"`
	}
	decodeInto(t, body, &ages)
	if len(ages.Buckets) < 2 || ages.Buckets[0].Entries != 1 {
		t.Fatalf("a just-written entry is not in the freshest bucket: %+v", ages.Buckets)
	}

	response, body = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/stats/upstreams?project=global", cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upstreams = %d %s", response.StatusCode, body)
	}
	var upstreams struct {
		Points []catalog.UpstreamPoint `json:"points"`
	}
	decodeInto(t, body, &upstreams)
	if len(upstreams.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(upstreams.Points))
	}
	if upstreams.Points[0].MeanMillis != 200 || upstreams.Points[0].Errors != 1 {
		t.Fatalf("upstream health is wrong: %+v", upstreams.Points[0])
	}
}

// The lockwarm job has existed in ops since it was written with no versioned route to
// reach it. This is that route.
func TestLockwarmIsReachableFromTheVersionedAPI(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	response, body := controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/projects/global/lockwarm", cookie,
		`{"lock":"version = 1\n","host":"cache.internal"}`)
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		t.Fatalf("lockwarm = %d %s", response.StatusCode, body)
	}
	var job struct {
		ID     int64  `json:"id"`
		Action string `json:"action"`
	}
	decodeInto(t, body, &job)
	if job.Action != "lockwarm" {
		t.Fatalf("submitted %q, want lockwarm (%s)", job.Action, body)
	}
	if job.ID == 0 {
		t.Fatalf("no job id: %s", body)
	}
}

// Every series endpoint is behind the same gate as the rest of the control API.
func TestSeriesEndpointsRequireAuth(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	for _, path := range []string{
		"/api/v1/stats/series", "/api/v1/stats/storage",
		"/api/v1/stats/upstreams", "/api/v1/stats/ages",
	} {
		response, body := controlRequest(t, server.Client(), http.MethodGet,
			server.URL+path, nil, "")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a session = %d %s", path, response.StatusCode, body)
		}
	}
}
