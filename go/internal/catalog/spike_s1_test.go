package catalog

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/race"
)

// Spike S1 — does the pure-Go SQLite driver hold up?
//
// This is the assumption the whole language choice rested on. modernc.org/sqlite is a
// machine translation of SQLite's C into Go, which is what keeps the binary CGO-free
// and genuinely static; the price is that it is slower than the real thing. The
// measured cache has ~32k entries, so the bet was that the difference is irrelevant
// here. These tests are that bet, written down and enforced.
//
// Run with: go test ./internal/catalog -run TestSpikeS1 -v
// Fallback if this ever fails: CGO + mattn/go-sqlite3 linked statically against musl.
//
// These skip under -race. The detector costs 5-20x, and this package is at the top
// of that range because modernc.org/sqlite is transpiled C and therefore
// memory-access-dense — measured 1,223 inserts/s instrumented against 15,396 not.
// The skip is loud rather than a build tag so the spike cannot silently disappear
// from a CI job that only runs -race. Correctness lives in catalog_test.go and does
// run instrumented.
const (
	s1Entries       = 100_000 // 3x headroom over 100x the measured cache
	s1LookupP99Max  = 200 * time.Microsecond
	s1MinInsertRate = 5_000 // rows/sec, batched
	s1GroupByMax    = 50 * time.Millisecond
)

func TestSpikeS1SQLiteUnderLoad(t *testing.T) {
	if race.Enabled {
		t.Skip(race.SkipReason)
	}
	if testing.Short() {
		t.Skip("spike: -short")
	}
	path := filepath.Join(t.TempDir(), "spike.db")
	db, err := Open(Options{Path: path, CacheSize: 1}) // LRU ~disabled: measure SQLite, not the cache
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// ---- insert throughput -------------------------------------------------
	start := time.Now()
	for i := range s1Entries {
		e := entry("global", "pypi", fmt.Sprintf("root/pypi/+f/pkg%06d/pkg-1.0-py3-none-any.whl", i),
			fmt.Sprintf("content-%d", i))
		if err := db.PutEntry(e); err != nil {
			t.Fatalf("PutEntry %d: %v", i, err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	insertElapsed := time.Since(start)
	rate := float64(s1Entries) / insertElapsed.Seconds()
	t.Logf("insert: %d rows in %v = %.0f rows/s", s1Entries, insertElapsed.Round(time.Millisecond), rate)
	if rate < s1MinInsertRate {
		t.Errorf("insert rate %.0f/s below the %d/s floor", rate, s1MinInsertRate)
	}

	// ---- point-lookup latency ----------------------------------------------
	// This is the hot path: one entry lookup per cache hit.
	const samples = 5_000
	lat := make([]time.Duration, 0, samples)
	for i := range samples {
		k := EntryKey{
			Project: "global", Eco: "pypi",
			Key: fmt.Sprintf("root/pypi/+f/pkg%06d/pkg-1.0-py3-none-any.whl", (i*7919)%s1Entries),
		}
		t0 := time.Now()
		if _, err := db.GetEntry(k); err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	p50, p99 := lat[len(lat)/2], lat[len(lat)*99/100]
	t.Logf("point lookup over %d rows: p50=%v p99=%v", s1Entries, p50, p99)
	if p99 > s1LookupP99Max {
		t.Errorf("lookup p99 %v exceeds %v", p99, s1LookupP99Max)
	}

	// ---- aggregate over the whole table ------------------------------------
	// The cross-cutting query the previous sharded design could not express at all.
	t0 := time.Now()
	if _, _, err := db.CountEntries("global"); err != nil {
		t.Fatalf("CountEntries: %v", err)
	}
	groupBy := time.Since(t0)
	t.Logf("aggregate over %d rows: %v", s1Entries, groupBy.Round(time.Microsecond))
	if groupBy > s1GroupByMax {
		t.Errorf("aggregate %v exceeds %v", groupBy, s1GroupByMax)
	}
}

// WAL must let readers run while the writer works — otherwise a stats query would
// block cache commits, and the two-pool design would be pointless.
func TestSpikeS1WALReaderSeesWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("spike: -short")
	}
	db := newDB(t)

	const n = 2_000
	var wg sync.WaitGroup
	writerDone := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		for i := range n {
			k := fmt.Sprintf("w%05d", i)
			if err := db.PutEntry(entry("global", "npm", k, k)); err != nil {
				t.Errorf("PutEntry: %v", err)
				return
			}
		}
		if err := db.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
	}()

	var reads int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-writerDone:
				return
			default:
			}
			if _, _, err := db.QueryArtifacts(ArtifactQuery{Project: "global"}); err != nil {
				t.Errorf("concurrent read: %v", err)
				return
			}
			reads++
		}
	}()
	wg.Wait()
	t.Logf("%d reads completed while %d rows were being written", reads, n)

	count, _, err := db.CountEntries("global")
	if err != nil {
		t.Fatalf("CountEntries: %v", err)
	}
	if count != n {
		t.Fatalf("reader/writer interference: %d rows durable, want %d", count, n)
	}
}

// The hot-path claim: with the LRU in front, a repeated key never reaches SQLite.
func BenchmarkGetEntryCached(b *testing.B) {
	if race.Enabled {
		b.Skip(race.SkipReason)
	}
	db := newDB(b)
	e := entry("global", "pypi", "root/pypi/+f/numpy/numpy-2.0.whl", "bytes")
	if err := db.PutEntry(e); err != nil {
		b.Fatalf("PutEntry: %v", err)
	}
	if err := db.Flush(); err != nil {
		b.Fatalf("Flush: %v", err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := db.GetEntry(e.EntryKey); err != nil {
			b.Fatalf("GetEntry: %v", err)
		}
	}
}

func BenchmarkGetEntryUncached(b *testing.B) {
	if race.Enabled {
		b.Skip(race.SkipReason)
	}
	db, err := Open(Options{Path: filepath.Join(b.TempDir(), "c.db"), CacheSize: 1})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const n = 10_000
	for i := range n {
		k := fmt.Sprintf("pkg%05d", i)
		if err := db.PutEntry(entry("global", "npm", k, k)); err != nil {
			b.Fatalf("PutEntry: %v", err)
		}
	}
	if err := db.Flush(); err != nil {
		b.Fatalf("Flush: %v", err)
	}
	b.ResetTimer()
	for i := range b.N {
		k := EntryKey{Project: "global", Eco: "npm", Key: fmt.Sprintf("pkg%05d", i%n)}
		if _, err := db.GetEntry(k); err != nil {
			b.Fatalf("GetEntry: %v", err)
		}
	}
}

func BenchmarkPutEntryBatched(b *testing.B) {
	if race.Enabled {
		b.Skip(race.SkipReason)
	}
	db := newDB(b)
	b.ResetTimer()
	for i := range b.N {
		k := fmt.Sprintf("pkg%08d", i)
		if err := db.PutEntry(entry("global", "npm", k, k)); err != nil {
			b.Fatalf("PutEntry: %v", err)
		}
	}
	b.StopTimer()
	if err := db.Flush(); err != nil {
		b.Fatalf("Flush: %v", err)
	}
}
