// Package load contains opt-in milestone qualification tests.
//
// These tests deliberately move tens of gigabytes through the cache pipeline and
// therefore do not run as part of the quick `go test ./...` suite.
package load

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"hash"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/obs"
	"github.com/brightskies/pkgreg/internal/race"
	"github.com/brightskies/pkgreg/internal/upstream"
)

var runM2 = flag.Bool("m2-2gb", false, "run the 20-client, 2 GiB M2 qualification")

const (
	m2Clients = 20
	m2Bytes   = int64(2) << 30
	chunkSize = 1 << 20
)

// TestM2TwentyClientsSingle2GiB qualifies milestone M2:
//
//   - twenty concurrent clients receive the complete two-GiB artifact;
//   - every client computes the same independently known SHA-256;
//   - the origin receives exactly one request;
//   - only one blob is committed.
//
// Response bodies are hashed and discarded rather than buffered, keeping the test
// memory-bounded while still moving the full 40 GiB through the reader side.
func TestM2TwentyClientsSingle2GiB(t *testing.T) {
	if !*runM2 {
		t.Skip("opt in with: go test ./test/load -run TestM2 -args -m2-2gb")
	}
	if race.Enabled {
		t.Skip(race.SkipReason)
	}

	chunk := makeChunk()
	expected := repeatedDigest(chunk, m2Bytes)

	var originRequests atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(m2Bytes))
		w.WriteHeader(http.StatusOK)
		for remaining := m2Bytes; remaining > 0; {
			n := min(remaining, int64(len(chunk)))
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}))
	defer origin.Close()

	dataDir := t.TempDir()
	blobs, err := blob.Open(dataDir)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	cat, err := catalog.Open(catalog.Options{
		Path: filepath.Join(dataDir, "catalog.db"),
	})
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	defer cat.Close()

	snap := config.Defaults()
	snap.DataDir = dataDir
	cfg := config.NewStore(&snap)
	metrics := obs.NewMetrics()
	cache := engine.New(engine.Options{
		Blobs: blobs, Catalog: cat,
		Pool:   upstream.New(snap.Upstream, metrics),
		Config: cfg, Metrics: metrics, Events: obs.NewBus(),
		Context: context.Background(),
	})
	resolution := engine.Resolution{
		Project: "global",
		Eco:     "pypi",
		Key:     "root/pypi/+f/m2/m2-2gb.whl",
		Upstream: upstream.Request{
			URL: origin.URL + "/m2-2gb.whl",
		},
		Expect: engine.Expect{Digest: blob.Digest(fmt.Sprintf("%x", expected)), Size: m2Bytes},
	}

	var peakHeap atomic.Uint64
	stopMemory := make(chan struct{})
	memoryDone := make(chan struct{})
	go monitorHeap(stopMemory, memoryDone, &peakHeap)

	started := time.Now()
	start := make(chan struct{})
	results := make([]clientResult, m2Clients)
	var clients sync.WaitGroup
	for i := range m2Clients {
		clients.Add(1)
		go func() {
			defer clients.Done()
			<-start
			writer := newDigestResponse()
			req := httptest.NewRequest(http.MethodGet, "http://pkgreg.test/m2-2gb.whl", nil)
			_, serveErr := cache.Serve(writer, req, resolution)
			results[i] = clientResult{
				status: writer.status, bytes: writer.bytes,
				digest: writer.sum(), err: serveErr,
			}
		}()
	}
	close(start)
	clients.Wait()
	close(stopMemory)
	<-memoryDone
	elapsed := time.Since(started)

	for i, result := range results {
		if result.err != nil {
			t.Fatalf("client %d: %v", i, result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("client %d: status=%d", i, result.status)
		}
		if result.bytes != m2Bytes {
			t.Fatalf("client %d: bytes=%d, want %d", i, result.bytes, m2Bytes)
		}
		if result.digest != expected {
			t.Fatalf("client %d: sha256=%x, want %x", i, result.digest, expected)
		}
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("origin requests=%d, want exactly 1", got)
	}
	count, held, err := blobs.Usage()
	if err != nil {
		t.Fatalf("blob usage: %v", err)
	}
	if count != 1 || held != m2Bytes {
		t.Fatalf("blob store: count=%d bytes=%d, want 1 and %d", count, held, m2Bytes)
	}
	if err := cat.Flush(); err != nil {
		t.Fatalf("catalog flush: %v", err)
	}
	entry, err := cat.GetEntry(catalog.EntryKey{
		Project: resolution.Project, Eco: resolution.Eco, Key: resolution.Key,
	})
	if err != nil || entry.Digest != resolution.Expect.Digest || entry.Size != m2Bytes {
		t.Fatalf("catalog entry=%+v err=%v", entry, err)
	}

	t.Logf("M2 PASS: clients=%d artifact=%s aggregate=%s elapsed=%s throughput=%.2f GiB/s peak_heap=%s origin_requests=%d",
		m2Clients, formatBytes(m2Bytes), formatBytes(m2Bytes*m2Clients),
		elapsed.Round(time.Millisecond),
		float64(m2Bytes*m2Clients)/elapsed.Seconds()/float64(int64(1)<<30),
		formatBytes(int64(peakHeap.Load())), originRequests.Load())
}

type clientResult struct {
	status int
	bytes  int64
	digest [sha256.Size]byte
	err    error
}

type digestResponse struct {
	header http.Header
	status int
	bytes  int64
	hash   hash.Hash
}

func newDigestResponse() *digestResponse {
	return &digestResponse{header: http.Header{}, hash: sha256.New()}
}

func (w *digestResponse) Header() http.Header { return w.header }

func (w *digestResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *digestResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.hash.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *digestResponse) sum() [sha256.Size]byte {
	var sum [sha256.Size]byte
	copy(sum[:], w.hash.Sum(nil))
	return sum
}

func makeChunk() []byte {
	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = byte((i*31 + 7) % 251)
	}
	return chunk
}

func repeatedDigest(chunk []byte, size int64) [sha256.Size]byte {
	h := sha256.New()
	for remaining := size; remaining > 0; {
		n := min(remaining, int64(len(chunk)))
		_, _ = h.Write(chunk[:n])
		remaining -= n
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func monitorHeap(stop <-chan struct{}, done chan<- struct{}, peak *atomic.Uint64) {
	defer close(done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		for current := peak.Load(); stats.HeapAlloc > current; current = peak.Load() {
			if peak.CompareAndSwap(current, stats.HeapAlloc) {
				break
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func formatBytes(n int64) string {
	const gib = int64(1) << 30
	return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
}
