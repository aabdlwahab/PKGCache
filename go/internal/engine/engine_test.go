package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

type harness struct {
	engine *Engine
	blobs  *blob.Store
	cat    *catalog.DB
	origin *testupstream.Server
	cfg    *config.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	blobs, err := blob.Open(dir)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(dir, "catalog.db")})
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	snap := config.Defaults()
	snap.DataDir = dir
	snap.Upstream.RequestTimeout = 30 * time.Second
	snap.Upstream.ConnectTimeout = 5 * time.Second
	cfg := config.NewStore(&snap)

	origin := testupstream.New()
	t.Cleanup(origin.Close)

	m := obs.NewMetrics()
	pool, poolErr := upstream.New(snap.Upstream, m)
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	e := New(Options{
		Blobs:   blobs,
		Catalog: cat,
		Pool:    pool,
		Config:  cfg,
		Metrics: m,
		Events:  obs.NewBus(),
	})
	return &harness{engine: e, blobs: blobs, cat: cat, origin: origin, cfg: cfg}
}

// serve runs one request through the engine and returns the recorder and outcome.
func (h *harness) serve(t *testing.T, req *http.Request, res Resolution) (*httptest.ResponseRecorder, Outcome, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	outcome, err := h.engine.Serve(rec, req, res)
	return rec, outcome, err
}

func (h *harness) resolution(path string) Resolution {
	return Resolution{
		Project:  "global",
		Eco:      "pypi",
		Key:      strings.TrimPrefix(path, "/"),
		Upstream: upstream.Request{URL: h.origin.URLFor(path)},
	}
}

func digestOf(b []byte) blob.Digest {
	sum := sha256.Sum256(b)
	return blob.Digest(hex.EncodeToString(sum[:]))
}

func get(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

type installingPeer struct {
	store *blob.Store
	body  []byte
}

func (p installingPeer) Fetch(
	_ context.Context, _, _ string, digest blob.Digest,
) (bool, int64, error) {
	if digest != digestOf(p.body) {
		return false, 0, nil
	}
	writer, err := p.store.Create()
	if err != nil {
		return false, 0, err
	}
	defer writer.Abort()
	if _, err := writer.Write(p.body); err != nil {
		return false, 0, err
	}
	_, size, err := writer.Commit()
	return err == nil, size, err
}

func TestPeerPrecedesOfflineAndOrigin(t *testing.T) {
	dir := t.TempDir()
	store, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(dir, "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	snapshot := config.Defaults()
	snapshot.DataDir = dir
	snapshot.Upstream.Offline = true
	cfg := config.NewStore(&snapshot)
	metrics := obs.NewMetrics()
	body := []byte("peer-only-content")
	cache := New(Options{
		Blobs: store, Catalog: cat, Config: cfg, Metrics: metrics, Events: obs.NewBus(),
		Peer: installingPeer{store: store, body: body},
	})
	recorder := httptest.NewRecorder()
	outcome, err := cache.Serve(recorder, get("/peer.whl"), Resolution{
		Project: "global", Eco: "pypi", Key: "peer.whl",
		Expect:   Expect{Digest: digestOf(body)},
		Upstream: upstream.Request{URL: "https://internet.invalid/peer.whl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != OutcomePeer || recorder.Body.String() != string(body) {
		t.Fatalf("outcome=%s body=%q", outcome, recorder.Body.String())
	}
}

func TestPutEnforcesProjectQuotaAtCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := blob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(dir, "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	snapshot := config.Defaults()
	snapshot.DataDir = dir
	snapshot.Projects = map[string]config.Project{
		"limited": {Name: "limited", QuotaBytes: 4},
	}
	cfg := config.NewStore(&snapshot)
	cache := New(Options{
		Blobs: store, Catalog: cat, Config: cfg,
		Metrics: obs.NewMetrics(), Events: obs.NewBus(),
	})
	_, err = cache.Put("limited", "files", "too-big", strings.NewReader("12345"), PutOptions{})
	if !errors.Is(err, catalog.ErrQuota) {
		t.Fatalf("err=%v, want quota", err)
	}
	var quota *catalog.QuotaError
	if !errors.As(err, &quota) || quota.Usage != 0 || quota.Limit != 4 || quota.Attempt != 5 {
		t.Fatalf("quota=%+v", quota)
	}
	count, usage, err := cat.CountEntries("limited")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || usage != 0 {
		t.Fatalf("count=%d usage=%d", count, usage)
	}
}

// A cache hit must advance the entry's recency and hit count. Nothing else does:
// eviction reads those columns, so if a read leaves them untouched the ranking is
// insertion order and TTL eviction measures age-since-cached instead of idle time.
func TestCacheHitAdvancesEntryRecencyAndHits(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("hot-", 500)
	h.origin.Serve("/hot.whl", body)
	res := h.resolution("/hot.whl")

	if _, _, err := h.serve(t, get("/hot.whl"), res); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.Flush(); err != nil {
		t.Fatal(err)
	}
	key := catalog.EntryKey{Project: "global", Eco: "pypi", Key: "hot.whl"}
	before, err := h.cat.GetEntry(key)
	if err != nil {
		t.Fatal(err)
	}

	// Entry timestamps have second granularity, so move past the boundary before the
	// reads or an advance would be indistinguishable from no advance.
	time.Sleep(1100 * time.Millisecond)
	const reads = 3
	for i := 0; i < reads; i++ {
		_, outcome, err := h.serve(t, get("/hot.whl"), res)
		if err != nil || outcome != OutcomeHit {
			t.Fatalf("read %d: outcome=%s err=%v", i, outcome, err)
		}
	}
	if err := h.engine.Flush(); err != nil {
		t.Fatal(err)
	}

	after, err := h.cat.GetEntry(key)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastAccess.After(before.LastAccess) {
		t.Errorf("last_access did not advance on hits: before=%v after=%v",
			before.LastAccess, after.LastAccess)
	}
	if after.Hits != before.Hits+reads {
		t.Errorf("hits = %d, want %d", after.Hits, before.Hits+reads)
	}
}

// Over-quota content must be refused before the transfer. Enforcing only at
// entry-commit time left the bytes on disk with no entry row, so every subsequent
// request re-fetched the same artifact from upstream forever.
func TestMissPathRefusesOverQuotaBeforeFetching(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("q", 5000)
	h.origin.Serve("/big.whl", body)

	if err := h.cfg.SetProjects(map[string]config.Project{
		"limited": {Name: "limited", QuotaBytes: 4},
	}); err != nil {
		t.Fatal(err)
	}

	res := h.resolution("/big.whl")
	res.Project = "limited"

	for i := 0; i < 2; i++ {
		_, outcome, err := h.serve(t, get("/big.whl"), res)
		if outcome != OutcomeFail {
			t.Fatalf("request %d: outcome=%s, want fail", i, outcome)
		}
		var quota *catalog.QuotaError
		if !errors.As(err, &quota) {
			t.Fatalf("request %d: err=%v, want a quota error the adapter can turn into a 507", i, err)
		}
		if quota.Limit != 4 {
			t.Fatalf("request %d: quota=%+v", i, quota)
		}
	}

	if _, usage, err := h.cat.CountEntries("limited"); err != nil || usage != 0 {
		t.Errorf("usage=%d err=%v, want 0", usage, err)
	}
	count, bytes, err := h.blobs.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || bytes != 0 {
		t.Errorf("blob store holds %d objects / %d bytes for an over-quota project", count, bytes)
	}
}

// ---- the basic pipeline ---------------------------------------------------

func TestMissThenHit(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("wheel-bytes-", 200_000)
	h.origin.Serve("/numpy.whl", body)
	res := h.resolution("/numpy.whl")

	rec, outcome, err := h.serve(t, get("/numpy.whl"), res)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if outcome != OutcomeMiss {
		t.Fatalf("outcome = %s, want miss", outcome)
	}
	if got := rec.Body.Bytes(); !equal(got, body) {
		t.Fatalf("body mismatch: %d bytes vs %d", len(got), len(body))
	}

	rec, outcome, err = h.serve(t, get("/numpy.whl"), res)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if outcome != OutcomeHit {
		t.Fatalf("outcome = %s, want hit", outcome)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("hit served different bytes than the miss")
	}
	if h.origin.Hits("/numpy.whl") != 1 {
		t.Fatalf("upstream hit %d times, want 1", h.origin.Hits("/numpy.whl"))
	}
}

// Universal dedup: identical bytes fetched by one project are reusable by another
// without any upstream request, even across ecosystems.
func TestDedupAcrossProjects(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("shared-", 50_000)
	d := digestOf(body)
	h.origin.Serve("/torch.whl", body)

	first := h.resolution("/torch.whl")
	first.Expect = Expect{Digest: d}
	if _, outcome, err := h.serve(t, get("/torch.whl"), first); err != nil || outcome != OutcomeMiss {
		t.Fatalf("seed: outcome=%s err=%v", outcome, err)
	}

	second := Resolution{
		Project:  "team-a",
		Eco:      "npm",
		Key:      "torch/-/torch.tgz",
		Upstream: upstream.Request{URL: h.origin.URLFor("/torch.whl")},
		Expect:   Expect{Digest: d},
	}
	rec, outcome, err := h.serve(t, get("/torch"), second)
	if err != nil {
		t.Fatalf("dedup request: %v", err)
	}
	if outcome != OutcomeDedup {
		t.Fatalf("outcome = %s, want dedup", outcome)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("dedup served the wrong bytes")
	}
	if h.origin.Hits("/torch.whl") != 1 {
		t.Fatalf("dedup made an upstream request: %d hits", h.origin.Hits("/torch.whl"))
	}
}

func TestOfflineMiss(t *testing.T) {
	h := newHarness(t)
	h.origin.Serve("/pkg.whl", []byte("content"))
	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = true
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	_, outcome, err := h.serve(t, get("/pkg.whl"), h.resolution("/pkg.whl"))
	if !errors.Is(err, ErrNotCached) {
		t.Fatalf("err = %v, want ErrNotCached", err)
	}
	if outcome != OutcomeFail {
		t.Fatalf("outcome = %s", outcome)
	}
	if h.origin.Requests.Load() != 0 {
		t.Fatal("offline mode contacted upstream")
	}
}

func TestOfflineStillServesHits(t *testing.T) {
	h := newHarness(t)
	body := []byte("already cached")
	h.origin.Serve("/pkg.whl", body)
	res := h.resolution("/pkg.whl")
	if _, _, err := h.serve(t, get("/pkg.whl"), res); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = true
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec, outcome, err := h.serve(t, get("/pkg.whl"), res)
	if err != nil || outcome != OutcomeHit {
		t.Fatalf("offline hit failed: outcome=%s err=%v", outcome, err)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("offline hit served wrong bytes")
	}
}

// Per-project soft offline must not affect other projects.
func TestPerProjectOffline(t *testing.T) {
	h := newHarness(t)
	h.origin.Serve("/pkg.whl", []byte("content"))
	if err := h.cfg.Apply(func(s *config.Snapshot) error {
		s.Projects["team-a"] = config.Project{Name: "team-a", Offline: true}
		return nil
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	blocked := h.resolution("/pkg.whl")
	blocked.Project = "team-a"
	if _, _, err := h.serve(t, get("/pkg.whl"), blocked); !errors.Is(err, ErrNotCached) {
		t.Fatalf("team-a should be offline: %v", err)
	}
	if _, outcome, err := h.serve(t, get("/pkg.whl"), h.resolution("/pkg.whl")); err != nil || outcome != OutcomeMiss {
		t.Fatalf("global should still be online: outcome=%s err=%v", outcome, err)
	}
}

// ---- integrity ------------------------------------------------------------

// Bad bytes must never be published. The next request has to be able to retry
// cleanly rather than being served poison forever.
func TestDigestMismatchIsNotCached(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("good-", 10_000)
	h.origin.Handle("/pkg.whl", testupstream.Behaviour{Body: body, Corrupt: true})

	res := h.resolution("/pkg.whl")
	res.Expect = Expect{Digest: digestOf(body)}

	_, _, err := h.serve(t, get("/pkg.whl"), res)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if _, err := h.cat.GetEntry(catalog.EntryKey{Project: "global", Eco: "pypi", Key: "pkg.whl"}); err == nil {
		t.Fatal("a corrupt fetch wrote a catalog entry")
	}
	if h.blobs.Exists(digestOf(body)) {
		t.Fatal("corrupt content was published to the blob store")
	}

	// And a subsequent good fetch must succeed — nothing poisoned.
	h.origin.Handle("/pkg.whl", testupstream.Behaviour{Body: body})
	rec, outcome, err := h.serve(t, get("/pkg.whl"), res)
	if err != nil || outcome != OutcomeMiss {
		t.Fatalf("retry after corruption failed: outcome=%s err=%v", outcome, err)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("retry served wrong bytes")
	}
}

func TestTruncatedTransferIsRejected(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("data-", 100_000)
	h.origin.Handle("/pkg.whl", testupstream.Behaviour{Body: body, TruncateAfter: 40_000})

	_, _, err := h.serve(t, get("/pkg.whl"), h.resolution("/pkg.whl"))
	if err == nil {
		t.Fatal("a body shorter than its declared Content-Length must fail")
	}
	if _, cerr := h.cat.GetEntry(catalog.EntryKey{Project: "global", Eco: "pypi", Key: "pkg.whl"}); cerr == nil {
		t.Fatal("a truncated fetch wrote a catalog entry")
	}
}

func TestUpstreamErrorStatus(t *testing.T) {
	h := newHarness(t)
	h.origin.Handle("/pkg.whl", testupstream.Behaviour{Status: http.StatusNotFound})
	_, outcome, err := h.serve(t, get("/pkg.whl"), h.resolution("/pkg.whl"))
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("err = %v, want ErrUpstreamStatus", err)
	}
	if outcome != OutcomeFail {
		t.Fatalf("outcome = %s", outcome)
	}
}

// A stale catalog row pointing at a collected blob must self-heal, not 404.
func TestStaleEntryHeals(t *testing.T) {
	h := newHarness(t)
	body := []byte("content that will be collected")
	h.origin.Serve("/pkg.whl", body)
	res := h.resolution("/pkg.whl")
	if _, _, err := h.serve(t, get("/pkg.whl"), res); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := h.blobs.Delete(digestOf(body)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rec, outcome, err := h.serve(t, get("/pkg.whl"), res)
	if err != nil {
		t.Fatalf("stale entry did not heal: %v", err)
	}
	if outcome != OutcomeMiss {
		t.Fatalf("outcome = %s, want a re-fetch", outcome)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("healed request served wrong bytes")
	}
}

// ---- HEAD and Range on a miss --------------------------------------------

func TestRangeOnMissWaitsForCommit(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("range-", 60_000)
	h.origin.Serve("/pkg.whl", body)

	req := get("/pkg.whl")
	req.Header.Set("Range", "bytes=100-199")
	rec, outcome, err := h.serve(t, req, h.resolution("/pkg.whl"))
	if err != nil {
		t.Fatalf("ranged miss: %v", err)
	}
	if outcome != OutcomeMiss {
		t.Fatalf("outcome = %s", outcome)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.Bytes(); len(got) != 100 || !equal(got, body[100:200]) {
		t.Fatalf("range body wrong: %d bytes", len(got))
	}
}

func TestHeadOnMiss(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("head-", 30_000)
	h.origin.Serve("/pkg.whl", body)

	req := httptest.NewRequest(http.MethodHead, "/pkg.whl", nil)
	rec, _, err := h.serve(t, req, h.resolution("/pkg.whl"))
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD returned a body of %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
}

// Content-Length must only be claimed when upstream declared one; guessing would
// desynchronise framing if the transfer came up short.
func TestChunkedUpstreamOmitsContentLength(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("chunk-", 20_000)
	h.origin.Handle("/pkg.whl", testupstream.Behaviour{Body: body, OmitContentLength: true, ChunkSize: 4096})

	rec, _, err := h.serve(t, get("/pkg.whl"), h.resolution("/pkg.whl"))
	if err != nil {
		t.Fatalf("chunked miss: %v", err)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want it absent", got)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("chunked body mismatch")
	}
}

// ---- inventory and stats ---------------------------------------------------

func TestArtifactRecorded(t *testing.T) {
	h := newHarness(t)
	body := []byte("wheel")
	h.origin.Serve("/numpy-2.0.whl", body)

	res := h.resolution("/numpy-2.0.whl")
	res.Artifact = &catalog.Artifact{Name: "numpy", Version: "2.0.0", Arch: "py3-none-any"}
	res.AccessName = "numpy"
	if _, _, err := h.serve(t, get("/numpy-2.0.whl"), res); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if err := h.cat.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	arts, total, err := h.cat.QueryArtifacts(catalog.ArtifactQuery{Project: "global"})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if total != 1 || arts[0].Name != "numpy" || arts[0].Size != int64(len(body)) {
		t.Fatalf("inventory = %+v (total %d)", arts, total)
	}
	if arts[0].Digest != digestOf(body) {
		t.Fatal("artifact digest does not match the content")
	}

	if err := h.engine.Flush(); err != nil {
		t.Fatalf("stats Flush: %v", err)
	}
	stats, err := h.cat.Stats(catalog.StatsQuery{Project: "global"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.Leaderboard) == 0 || stats.Leaderboard[0].Name != "numpy" {
		t.Fatalf("leaderboard = %+v", stats.Leaderboard)
	}
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- Spike S2: single-flight and progressive delivery ----------------------
//
// This is risk R2 in the plan — the one rated Critical, because a bug here corrupts
// a multi-gigabyte artifact rather than merely failing. Every property the design
// depends on is asserted here permanently, not just during the spike.

// N concurrent clients for the same key must produce exactly one upstream fetch, and
// all N must receive byte-identical content.
func TestS2SingleFlightManyReaders(t *testing.T) {
	h := newHarness(t)
	const readers = 20
	body := testupstream.Repeat("single-flight-payload-", 2<<20) // 2 MiB
	h.origin.Handle("/big.whl", testupstream.Behaviour{
		Body: body, ChunkSize: 32 << 10, DelayPerChunk: time.Millisecond,
	})
	res := h.resolution("/big.whl")

	var wg sync.WaitGroup
	bodies := make([][]byte, readers)
	errs := make([]error, readers)
	start := make(chan struct{})

	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			if _, err := h.engine.Serve(rec, get("/big.whl"), res); err != nil {
				errs[i] = err
				return
			}
			bodies[i] = rec.Body.Bytes()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
		if !equal(bodies[i], body) {
			t.Fatalf("reader %d got %d bytes, want %d", i, len(bodies[i]), len(body))
		}
	}
	if n := h.origin.Hits("/big.whl"); n != 1 {
		t.Fatalf("upstream fetched %d times, want exactly 1 for %d concurrent readers", n, readers)
	}
	if h.engine.Inflight().Len() != 0 {
		t.Fatalf("registry leaked %d fetches", h.engine.Inflight().Len())
	}
}

// A client disconnecting mid-download must not abort the fetch: other readers and
// the cache itself still want it. This is why the fetch goroutine runs on a detached
// context.
func TestS2ClientDisconnectDoesNotAbortFetch(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("persist-", 512<<10)
	h.origin.Handle("/big.whl", testupstream.Behaviour{
		Body: body, ChunkSize: 16 << 10, DelayPerChunk: 2 * time.Millisecond,
	})
	res := h.resolution("/big.whl")

	ctx, cancel := context.WithCancel(context.Background())
	req := get("/big.whl").WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		_, _ = h.engine.Serve(rec, req, res)
	}()

	time.Sleep(30 * time.Millisecond) // let some bytes flow
	cancel()                          // the client goes away
	<-done

	// The fetch must still complete and land in the cache.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := h.cat.GetEntry(catalog.EntryKey{
			Project: "global", Eco: "pypi", Key: "big.whl",
		}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fetch did not complete after the client disconnected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.blobs.Exists(digestOf(body)) {
		t.Fatal("content was not published despite the fetch completing")
	}

	// And the next request is a hit, with no second upstream fetch.
	rec, outcome, err := h.serve(t, get("/big.whl"), res)
	if err != nil || outcome != OutcomeHit {
		t.Fatalf("outcome = %s err = %v, want a hit", outcome, err)
	}
	if !equal(rec.Body.Bytes(), body) {
		t.Fatal("content served after the abandoned fetch is wrong")
	}
	if n := h.origin.Hits("/big.whl"); n != 1 {
		t.Fatalf("upstream fetched %d times, want 1", n)
	}
}

// An upstream failure part-way through must reach every attached reader and leave
// nothing behind.
func TestS2UpstreamFailureMidStream(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("fails-", 400_000)
	h.origin.Handle("/bad.whl", testupstream.Behaviour{
		Body: body, TruncateAfter: 100_000, ChunkSize: 8 << 10,
	})
	res := h.resolution("/bad.whl")

	const readers = 8
	var wg sync.WaitGroup
	failures := make([]bool, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			_, err := h.engine.Serve(rec, get("/bad.whl"), res)
			// A reader either sees the error directly, or sees a short body — both
			// are correct; what matters is that nothing is cached.
			failures[i] = err != nil || rec.Body.Len() != len(body)
		}()
	}
	wg.Wait()

	for i, failed := range failures {
		if !failed {
			t.Fatalf("reader %d reported success for a truncated transfer", i)
		}
	}
	if _, err := h.cat.GetEntry(catalog.EntryKey{
		Project: "global", Eco: "pypi", Key: "bad.whl",
	}); err == nil {
		t.Fatal("a failed fetch wrote a catalog entry")
	}
	if h.engine.Inflight().Len() != 0 {
		t.Fatal("registry leaked a failed fetch")
	}
}

// Readers must genuinely receive bytes while the download is still running, rather
// than being released only at the end.
func TestS2ProgressiveDeliveryIsActuallyProgressive(t *testing.T) {
	h := newHarness(t)
	body := testupstream.Repeat("progressive-", 256<<10)
	h.origin.Handle("/slow.whl", testupstream.Behaviour{
		Body: body, ChunkSize: 8 << 10, DelayPerChunk: 5 * time.Millisecond,
	})
	res := h.resolution("/slow.whl")

	fetchKey := "global\x00pypi\x00slow.whl"
	f, created := h.engine.Inflight().Start(fetchKey, "pypi")
	if !created {
		t.Fatal("expected to create the fetch")
	}
	req := res.Upstream
	req.Eco = "pypi"
	go h.engine.runFetch(f, req, Expect{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := f.Reader(ctx)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer reader.Close()

	// Read a first chunk and confirm the download is still running when we get it.
	buf := make([]byte, 4096)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if n == 0 {
		t.Fatal("first Read returned no bytes")
	}
	if _, done, _, _ := f.state(); done {
		t.Fatal("received bytes only after the fetch completed — delivery is not progressive")
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := append(buf[:n], rest...)
	if !equal(got, body) {
		t.Fatalf("progressive read produced %d bytes, want %d", len(got), len(body))
	}
}

// A reader attaching after the fetch has already committed must still get the
// content, via the blob store rather than the staging file.
func TestS2ReaderAttachingAfterCommit(t *testing.T) {
	h := newHarness(t)
	body := []byte("small and fast")
	h.origin.Serve("/quick.whl", body)
	res := h.resolution("/quick.whl")

	fetchKey := "global\x00pypi\x00quick.whl"
	f, _ := h.engine.Inflight().Start(fetchKey, "pypi")
	req := res.Upstream
	req.Eco = "pypi"
	go h.engine.runFetch(f, req, Expect{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := f.Reader(ctx); !errors.Is(err, errCommitted) {
		t.Fatalf("err = %v, want errCommitted so the caller serves the blob", err)
	}
	d, size, ferr := f.Digest()
	if ferr != nil || d != digestOf(body) || size != int64(len(body)) {
		t.Fatalf("digest=%s size=%d err=%v", d, size, ferr)
	}
}

// Publication is part of fetch completion, not work a reader performs afterwards.
// Otherwise a request arriving in the done→catalog window can start a duplicate
// transfer after the registry has forgotten the first one.
func TestS2PublishesBeforeAnnouncingCompletion(t *testing.T) {
	h := newHarness(t)
	body := []byte("published before completion")
	h.origin.Serve("/ordered.whl", body)
	res := h.resolution("/ordered.whl")
	key := catalog.EntryKey{Project: res.Project, Eco: res.Eco, Key: res.Key}

	fetchKey := "global\x00pypi\x00ordered.whl"
	f, created := h.engine.Inflight().Start(fetchKey, "pypi")
	if !created {
		t.Fatal("expected to create the fetch")
	}
	publishing := make(chan struct{})
	release := make(chan struct{})
	req := res.Upstream
	req.Eco = res.Eco
	go h.engine.runFetch(f, req, Expect{}, func(digest blob.Digest, size int64) {
		close(publishing)
		<-release
		h.engine.link(key, res, digest, size, time.Now())
	})

	<-publishing
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch reported completion before publication: %v", err)
	}
	if got := h.engine.Inflight().Len(); got != 1 {
		t.Fatalf("registry removed the fetch during publication: len=%d", got)
	}

	close(release)
	if err := f.Wait(context.Background()); err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	entry, err := h.cat.GetEntry(key)
	if err != nil || entry.Digest != digestOf(body) {
		t.Fatalf("entry was not visible at completion: entry=%+v err=%v", entry, err)
	}
	if got := h.engine.Inflight().Len(); got != 0 {
		t.Fatalf("registry retained completed fetch: len=%d", got)
	}
}

// Model a request that performed its first catalog lookup just before another fetch
// published. serveMiss must check once more after it wins Registry.Start, otherwise
// this interleaving starts a duplicate transfer.
func TestS2RechecksEntryAfterBecomingFetchOwner(t *testing.T) {
	h := newHarness(t)
	body := []byte("won the race")
	res := h.resolution("/handoff.whl")
	key := catalog.EntryKey{Project: res.Project, Eco: res.Eco, Key: res.Key}
	digest, err := h.engine.PutBytes(res.Project, res.Eco, res.Key, body, "application/octet-stream")
	if err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	rec := httptest.NewRecorder()
	outcome, err := h.engine.serveMiss(rec, get("/handoff.whl"), res, key, time.Now())
	if err != nil || outcome != OutcomeHit {
		t.Fatalf("handoff outcome=%s err=%v", outcome, err)
	}
	if !equal(rec.Body.Bytes(), body) || digest != digestOf(body) {
		t.Fatal("handoff served the wrong content")
	}
	if hits := h.origin.Hits("/handoff.whl"); hits != 0 {
		t.Fatalf("handoff made %d upstream requests", hits)
	}
	if got := h.engine.Inflight().Len(); got != 0 {
		t.Fatalf("handoff leaked a registry entry: len=%d", got)
	}
}

// Distinct keys must not be collapsed by the single-flight registry.
func TestS2DistinctKeysFetchIndependently(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		h.origin.Serve(fmt.Sprintf("/pkg%d.whl", i), testupstream.Repeat(fmt.Sprintf("body-%d-", i), 10_000))
	}
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("/pkg%d.whl", i)
			rec := httptest.NewRecorder()
			if _, err := h.engine.Serve(rec, get(path), h.resolution(path)); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}()
	}
	wg.Wait()
	for i := range 5 {
		if n := h.origin.Hits(fmt.Sprintf("/pkg%d.whl", i)); n != 1 {
			t.Fatalf("pkg%d fetched %d times", i, n)
		}
	}
}
