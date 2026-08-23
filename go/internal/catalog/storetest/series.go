package storetest

import (
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// base is a fixed instant well inside the fine-resolution retention window, chosen so
// every case below can subtract hours from it without crossing an expiry boundary.
var base = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func delta(at time.Time, eco, outcome string, count, bytes int64) catalog.SeriesDelta {
	return catalog.SeriesDelta{
		Bucket: at, Project: "global", Eco: eco, Outcome: outcome,
		Count: count, Bytes: bytes,
	}
}

func totalOf(points []catalog.TrafficPoint) (count, bytes int64) {
	for _, point := range points {
		count += point.Count
		bytes += point.Bytes
	}
	return count, bytes
}

// The dimensions traffic_stats collapses — when, and which of the five pipeline steps
// answered — have to survive a round trip, or the console is back to a bare hit rate.
func seriesDimensions(t *testing.T, store catalog.Store) {
	earlier := base.Add(-10 * time.Minute)
	if err := store.RecordSeries([]catalog.SeriesDelta{
		delta(base, "pypi", "hit", 3, 300),
		delta(base, "pypi", "peer", 1, 100),
		delta(base, "npm", "miss", 2, 200),
		delta(earlier, "pypi", "hit", 5, 500),
	}, nil); err != nil {
		t.Fatalf("RecordSeries: %v", err)
	}

	points, err := store.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine,
		From: base.Add(-time.Hour), To: base.Add(time.Hour), GroupBy: "eco,outcome",
	})
	if err != nil {
		t.Fatalf("TrafficSeries: %v", err)
	}
	if len(points) != 4 {
		t.Fatalf("got %d points, want 4: %+v", len(points), points)
	}
	for _, point := range points {
		if point.Outcome == "" || point.Eco == "" {
			t.Fatalf("grouped query lost a dimension: %+v", point)
		}
	}

	// Two distinct buckets, so the earlier traffic must not have merged into the later.
	buckets := map[time.Time]bool{}
	for _, point := range points {
		buckets[point.Bucket] = true
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}

	// The same delta arriving twice adds up rather than replacing: these are counters,
	// and a flush is one window of many landing in the same bucket.
	if err := store.RecordSeries([]catalog.SeriesDelta{delta(base, "pypi", "hit", 2, 20)}, nil); err != nil {
		t.Fatalf("RecordSeries again: %v", err)
	}
	points, err = store.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine, GroupBy: "eco,outcome",
	})
	if err != nil {
		t.Fatalf("TrafficSeries: %v", err)
	}
	count, bytes := totalOf(points)
	if count != 13 || bytes != 1120 {
		t.Fatalf("accumulated to %d/%d, want 13/1120", count, bytes)
	}
}

func seriesGrouping(t *testing.T, store catalog.Store) {
	if err := store.RecordSeries([]catalog.SeriesDelta{
		delta(base, "pypi", "hit", 1, 10),
		delta(base, "pypi", "miss", 1, 10),
		delta(base, "npm", "hit", 1, 10),
	}, nil); err != nil {
		t.Fatalf("RecordSeries: %v", err)
	}

	for _, test := range []struct {
		groupBy string
		want    int
	}{
		{"", 1},            // one total line
		{"eco", 2},         // pypi, npm
		{"outcome", 2},     // hit, miss
		{"eco,outcome", 3}, // every combination that occurred
	} {
		points, err := store.TrafficSeries(catalog.SeriesQuery{
			Project: "global", Span: catalog.SpanFine, GroupBy: test.groupBy,
		})
		if err != nil {
			t.Fatalf("GroupBy %q: %v", test.groupBy, err)
		}
		if len(points) != test.want {
			t.Fatalf("GroupBy %q gave %d rows, want %d: %+v", test.groupBy, len(points), test.want, points)
		}
		// A dimension that was grouped away must come back empty, not partly filled
		// with whichever value the database happened to visit last.
		for _, point := range points {
			if test.groupBy == "" && (point.Eco != "" || point.Outcome != "") {
				t.Fatalf("ungrouped query leaked a dimension: %+v", point)
			}
		}
		if count, _ := totalOf(points); count != 3 {
			t.Fatalf("GroupBy %q changed the total to %d", test.groupBy, count)
		}
	}
}

// A dropped flush window has to read as "nobody knows", not as "no traffic". The store
// must therefore omit empty buckets rather than zero-filling them, so a chart can draw
// the difference.
func seriesGaps(t *testing.T, store catalog.Store) {
	if err := store.RecordSeries([]catalog.SeriesDelta{
		delta(base.Add(-time.Hour), "pypi", "hit", 1, 10),
		delta(base, "pypi", "hit", 1, 10),
	}, nil); err != nil {
		t.Fatalf("RecordSeries: %v", err)
	}
	points, err := store.TrafficSeries(catalog.SeriesQuery{
		Project: "global", Span: catalog.SpanFine,
		From: base.Add(-time.Hour), To: base,
	})
	if err != nil {
		t.Fatalf("TrafficSeries: %v", err)
	}
	// An hour at five-minute resolution is twelve slots; only two saw traffic.
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2 — empty buckets were materialised", len(points))
	}
}

func seriesCompaction(t *testing.T, store catalog.Store) {
	// Two fine buckets inside one hour, both old enough to be folded.
	old := base.Add(-72 * time.Hour)
	if err := store.RecordSeries([]catalog.SeriesDelta{
		delta(old, "pypi", "hit", 4, 400),
		delta(old.Add(5*time.Minute), "pypi", "hit", 6, 600),
		delta(base, "pypi", "hit", 1, 100), // recent: must survive at fine resolution
	}, nil); err != nil {
		t.Fatalf("RecordSeries: %v", err)
	}

	if err := store.CompactSeries(base); err != nil {
		t.Fatalf("CompactSeries: %v", err)
	}

	fine, err := store.TrafficSeries(catalog.SeriesQuery{Project: "global", Span: catalog.SpanFine})
	if err != nil {
		t.Fatalf("fine: %v", err)
	}
	if count, _ := totalOf(fine); count != 1 {
		t.Fatalf("fine resolution holds %d after compaction, want only the recent 1", count)
	}

	hourly, err := store.TrafficSeries(catalog.SeriesQuery{Project: "global", Span: catalog.SpanHour})
	if err != nil {
		t.Fatalf("hourly: %v", err)
	}
	if len(hourly) != 1 {
		t.Fatalf("got %d hourly rows, want the two fine buckets folded into 1", len(hourly))
	}
	count, bytes := totalOf(hourly)
	if count != 10 || bytes != 1000 {
		t.Fatalf("fold summed to %d/%d, want 10/1000", count, bytes)
	}

	// The fold adds into whatever is already in the coarse bucket, so a second run
	// would double-count if it could still see its own source rows. It cannot, because
	// the delete shares the fold's transaction — this is the test for that pairing.
	if err := store.CompactSeries(base); err != nil {
		t.Fatalf("CompactSeries again: %v", err)
	}
	hourly, err = store.TrafficSeries(catalog.SeriesQuery{Project: "global", Span: catalog.SpanHour})
	if err != nil {
		t.Fatalf("hourly again: %v", err)
	}
	if count, bytes := totalOf(hourly); count != 10 || bytes != 1000 {
		t.Fatalf("re-running compaction changed the total to %d/%d", count, bytes)
	}
}

// Storage is a gauge. Sampling twice in one hour must overwrite, or a chart of
// "how much is stored" would climb with sampling frequency rather than with content.
func storageSample(t *testing.T, store catalog.Store) {
	if err := store.SampleStorage(catalog.StorageSample{
		Bucket: base, BlobCount: 10, BlobBytes: 1000, FSFree: 500, FSTotal: 2000,
	}); err != nil {
		t.Fatalf("SampleStorage: %v", err)
	}
	if err := store.SampleStorage(catalog.StorageSample{
		Bucket: base.Add(20 * time.Minute), BlobCount: 12, BlobBytes: 1200,
		FSFree: 400, FSTotal: 2000,
	}); err != nil {
		t.Fatalf("SampleStorage again: %v", err)
	}
	samples, err := store.StorageSeries(base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("StorageSeries: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("got %d samples in one hour, want 1", len(samples))
	}
	if samples[0].BlobBytes != 1200 || samples[0].FSFree != 400 {
		t.Fatalf("second sample did not replace the first: %+v", samples[0])
	}
}

// The gap between what callers asked for and what the disk holds is what content
// addressing bought, so the two have to be reported separately.
func storageTotals(t *testing.T, store catalog.Store) {
	// One body, cached under two different names in two projects: two entries, one blob.
	put(t, store, key("global", "pypi", "six.whl"), "identical", base)
	put(t, store, key("team-a", "pypi", "six.whl"), "identical", base)

	totals, err := store.StorageTotals()
	if err != nil {
		t.Fatalf("StorageTotals: %v", err)
	}
	if totals.BlobCount != 1 {
		t.Fatalf("BlobCount = %d, want 1", totals.BlobCount)
	}
	if totals.EntryCount != 2 {
		t.Fatalf("EntryCount = %d, want 2", totals.EntryCount)
	}
	if totals.EntryBytes != 2*totals.BlobBytes {
		t.Fatalf("logical %d vs stored %d: dedup is not visible",
			totals.EntryBytes, totals.BlobBytes)
	}
}

func entryAges(t *testing.T, store catalog.Store) {
	fresh := key("global", "pypi", "fresh.whl")
	stale := key("global", "pypi", "stale.whl")
	put(t, store, fresh, "fresh", base.Add(-2*time.Hour))
	put(t, store, stale, "stale", base.Add(-200*24*time.Hour))

	buckets, err := store.EntryAges("global", base)
	if err != nil {
		t.Fatalf("EntryAges: %v", err)
	}
	if len(buckets) < 2 {
		t.Fatalf("got %d buckets", len(buckets))
	}
	if buckets[0].Entries != 1 {
		t.Fatalf("the two-hour-old entry did not land in the first bucket: %+v", buckets)
	}
	// 200 days is past the 90-day bound and short of a year.
	var total int64
	for _, bucket := range buckets {
		total += bucket.Entries
	}
	if total != 2 {
		t.Fatalf("histogram counts %d entries, want 2", total)
	}
	if buckets[len(buckets)-2].Entries != 1 {
		t.Fatalf("the 200-day-old entry is in the wrong bucket: %+v", buckets)
	}
}

func upstreamSeries(t *testing.T, store catalog.Store) {
	if err := store.RecordSeries(nil, []catalog.UpstreamDelta{
		{
			Bucket: base, Project: "global", Upstream: "pypi.org",
			Requests: 4, Errors: 1, Bytes: 4000, MillisSum: 800, MillisMax: 500,
		},
		{
			Bucket: base.Add(10 * time.Minute), Project: "global", Upstream: "pypi.org",
			Requests: 1, Errors: 0, Bytes: 1000, MillisSum: 200, MillisMax: 200,
		},
	}); err != nil {
		t.Fatalf("RecordSeries: %v", err)
	}
	points, err := store.UpstreamSeries(catalog.SeriesQuery{Project: "global"})
	if err != nil {
		t.Fatalf("UpstreamSeries: %v", err)
	}
	// Both deltas fall in the same hour, and upstream health is only kept hourly.
	if len(points) != 1 {
		t.Fatalf("got %d points, want 1: %+v", len(points), points)
	}
	point := points[0]
	if point.Requests != 5 || point.Errors != 1 || point.Bytes != 5000 {
		t.Fatalf("counters did not accumulate: %+v", point)
	}
	// Mean is derived from the stored sum, so it stays correct across accumulation —
	// a stored mean would have averaged the averages.
	if point.MeanMillis != 200 {
		t.Fatalf("MeanMillis = %d, want 1000/5 = 200", point.MeanMillis)
	}
	if point.MaxMillis != 500 {
		t.Fatalf("MaxMillis = %d, want 500", point.MaxMillis)
	}
}
