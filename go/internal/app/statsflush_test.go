package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// The engine accumulates usage counters in memory and relies on a periodic flush to
// persist them. That loop did not exist: Flush was called only from Close, so on a
// long-running instance the leaderboard and traffic totals read zero, the time series
// had nothing to plot, and entries.last_access never advanced — which quietly turned
// eviction from least-recently-used into least-recently-written, discarding the
// hottest content first.
//
// A short interval makes the loop observable in a test without waiting the default
// thirty seconds.
func TestUsageCountersReachTheCatalogWithoutAShutdown(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Maintenance.StatsFlushInterval = 50 * time.Millisecond
	})

	if _, err := a.Engine.PutBytes("global", "files", "hot.bin",
		[]byte("cached bytes"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	// Read it back through the data plane so hits are recorded the way real traffic
	// records them.
	server := httptest.NewServer(a.UnifiedHandler())
	defer server.Close()
	for i := 0; i < 3; i++ {
		response := getResponse(t, server.Client(), server.URL+"/global/files/hot.bin")
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("read %d = %d", i, response.StatusCode)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		points, err := a.Catalog.TrafficSeries(catalog.SeriesQuery{
			Project: "global", Span: catalog.SpanFine, GroupBy: "outcome",
		})
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, point := range points {
			total += point.Count
		}
		if total >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage counters never reached the catalog: %d requests recorded", total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A zero interval disables the loop rather than spinning on a zero-duration ticker,
// which would panic.
func TestZeroFlushIntervalIsNotATicker(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Maintenance.StatsFlushInterval = 0
	})
	if _, err := a.Engine.PutBytes("global", "files", "x.bin", []byte("x"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // long enough for a misconfigured ticker to fire
}
