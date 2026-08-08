package engine

import (
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/catalog"
)

// record() has always been handed the outcome and the timestamp; until now it reported
// both to Prometheus and then collapsed the outcome to a boolean and dropped the
// timestamp on the floor. This is the test that the durable series keeps them.
func TestServeRecordsEachOutcomeSeparatelyInTheSeries(t *testing.T) {
	h := newHarness(t)
	h.origin.Serve("/six.whl", []byte("wheel bytes"))
	res := h.resolution("/six.whl")

	// First read goes to the origin; the next two are served locally.
	if _, outcome, err := h.serve(t, get("/six.whl"), res); err != nil || outcome != OutcomeMiss {
		t.Fatalf("first read: outcome=%s err=%v", outcome, err)
	}
	for i := 0; i < 2; i++ {
		if _, outcome, err := h.serve(t, get("/six.whl"), res); err != nil || outcome != OutcomeHit {
			t.Fatalf("read %d: outcome=%s err=%v", i, outcome, err)
		}
	}
	if err := h.engine.Flush(); err != nil {
		t.Fatal(err)
	}

	points, err := h.cat.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine, GroupBy: "outcome",
	})
	if err != nil {
		t.Fatal(err)
	}
	byOutcome := map[string]int64{}
	for _, point := range points {
		byOutcome[point.Outcome] += point.Count
	}
	if byOutcome["miss"] != 1 {
		t.Fatalf("miss = %d, want 1 (%v)", byOutcome["miss"], byOutcome)
	}
	if byOutcome["hit"] != 2 {
		t.Fatalf("hit = %d, want 2 (%v)", byOutcome["hit"], byOutcome)
	}

	// The lifetime tally still exists and still folds hits and misses together — the
	// series is an addition, not a replacement.
	stats, err := h.cat.Stats(catalog.StatsQuery{Project: "global"})
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, row := range stats.ByEco {
		requests += row.HitCount + row.MissCount
	}
	if requests != 3 {
		t.Fatalf("traffic_stats saw %d requests, want 3", requests)
	}
}

// A flush window that straddles a bucket boundary has to split, or a burst would be
// attributed entirely to whichever bucket the flush happened to land in.
func TestSeriesSplitsAWindowAcrossBucketBoundaries(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 7, 29, 12, 4, 30, 0, time.UTC)

	h.engine.stats.recordSeries("global", "pypi", "hit", 100, now)
	h.engine.stats.recordSeries("global", "pypi", "hit", 100, now.Add(time.Minute))
	if err := h.engine.Flush(); err != nil {
		t.Fatal(err)
	}

	points, err := h.cat.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d buckets, want 2 — the window did not split: %+v", len(points), points)
	}
	if !points[0].Bucket.Before(points[1].Bucket) {
		t.Fatalf("buckets are not ordered: %+v", points)
	}
	if gap := points[1].Bucket.Sub(points[0].Bucket); gap != time.Duration(catalog.SpanFine)*time.Second {
		t.Fatalf("buckets are %s apart, want %ds", gap, catalog.SpanFine)
	}
}

// A failed request is still a fact about the cache. Dropping failures would make an
// outage look like a quiet period.
func TestFailuresAppearInTheSeries(t *testing.T) {
	h := newHarness(t)
	res := h.resolution("/missing.whl") // the origin has nothing at this path

	if _, outcome, _ := h.serve(t, get("/missing.whl"), res); outcome == OutcomeHit {
		t.Fatalf("expected the request to fail, got %s", outcome)
	}
	if err := h.engine.Flush(); err != nil {
		t.Fatal(err)
	}

	points, err := h.cat.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine, GroupBy: "outcome",
	})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, point := range points {
		total += point.Count
	}
	if total == 0 {
		t.Fatal("a failed request left no trace in the series")
	}
}
