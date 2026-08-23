package engine

import (
	"errors"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// Usage statistics accumulate in memory and are flushed to the catalog periodically.
//
// A `uv sync` fires thousands of requests in a burst. Touching SQLite on each one to
// bump a counter would put a write on the hot path for data whose worst-case loss is
// a slightly understated leaderboard. So deltas accumulate here and a background loop
// folds them in every ~30 seconds and at shutdown.
type statsCollector struct {
	mu      sync.Mutex
	access  map[accessKey]*accessValue
	traffic map[trafficKey]*catalog.TrafficDelta
	touch   map[catalog.EntryKey]*touchValue
	series  map[seriesKey]*seriesValue
}

type accessKey struct{ project, eco, name string }

type accessValue struct {
	count int64
	last  time.Time
}

type touchValue struct {
	digest blob.Digest
	count  int64
	last   time.Time
}

type trafficKey struct{ project, eco string }

// seriesKey carries the two dimensions traffic_stats throws away: when the request
// happened, and which of the five pipeline steps answered it.
type seriesKey struct {
	bucket       int64
	project, eco string
	outcome      string
}

type seriesValue struct{ count, bytes int64 }

func newStatsCollector() *statsCollector {
	return &statsCollector{
		access:  make(map[accessKey]*accessValue),
		traffic: make(map[trafficKey]*catalog.TrafficDelta),
		touch:   make(map[catalog.EntryKey]*touchValue),
		series:  make(map[seriesKey]*seriesValue),
	}
}

// access counts a request for a named package.
func (s *statsCollector) recordAccess(project, eco, name string, at time.Time) {
	k := accessKey{project, eco, name}
	s.mu.Lock()
	v, ok := s.access[k]
	if !ok {
		v = &accessValue{}
		s.access[k] = v
	}
	v.count++
	v.last = at
	s.mu.Unlock()
}

// recordTouch counts a read of an existing cache entry. This is what makes eviction
// least-recently-*used* rather than least-recently-written.
func (s *statsCollector) recordTouch(k catalog.EntryKey, digest blob.Digest, at time.Time) {
	s.mu.Lock()
	v, ok := s.touch[k]
	if !ok {
		v = &touchValue{digest: digest}
		s.touch[k] = v
	}
	v.count++
	v.last = at
	if v.digest == "" {
		v.digest = digest
	}
	s.mu.Unlock()
}

// traffic counts bytes served, split by whether the cache supplied them.
func (s *statsCollector) recordTraffic(project, eco string, hit bool, n int64) {
	if n < 0 {
		return
	}
	k := trafficKey{project, eco}
	s.mu.Lock()
	v, ok := s.traffic[k]
	if !ok {
		v = &catalog.TrafficDelta{Project: project, Eco: eco}
		s.traffic[k] = v
	}
	if hit {
		v.HitCount++
		v.HitBytes += n
	} else {
		v.MissCount++
		v.MissBytes += n
	}
	s.mu.Unlock()
}

// recordSeries counts one resolved request against its time bucket and outcome.
//
// Bucketing here rather than at write time is what keeps the flush cheap: a window
// spans at most two buckets, so this map stays the same size it always was, while the
// rows it produces land in the right place on the timeline even when a flush crosses
// a boundary.
func (s *statsCollector) recordSeries(project, eco, outcome string, n int64, at time.Time) {
	if n < 0 {
		n = 0
	}
	k := seriesKey{
		bucket:  catalog.Bucket(at, catalog.SpanFine).Unix(),
		project: project, eco: eco, outcome: outcome,
	}
	s.mu.Lock()
	v, ok := s.series[k]
	if !ok {
		v = &seriesValue{}
		s.series[k] = v
	}
	v.count++
	v.bytes += n
	s.mu.Unlock()
}

// drain removes and returns the accumulated window.
func (s *statsCollector) drain() (
	[]catalog.AccessDelta, []catalog.TrafficDelta, []catalog.EntryTouch, []catalog.SeriesDelta,
) {
	s.mu.Lock()
	access := make([]catalog.AccessDelta, 0, len(s.access))
	for k, v := range s.access {
		access = append(access, catalog.AccessDelta{
			Project: k.project, Eco: k.eco, Name: k.name,
			Count: v.count, LastAccess: v.last,
		})
	}
	traffic := make([]catalog.TrafficDelta, 0, len(s.traffic))
	for _, v := range s.traffic {
		traffic = append(traffic, *v)
	}
	touch := make([]catalog.EntryTouch, 0, len(s.touch))
	for k, v := range s.touch {
		touch = append(touch, catalog.EntryTouch{
			EntryKey: k, Digest: v.digest, Hits: v.count, LastAccess: v.last,
		})
	}
	series := make([]catalog.SeriesDelta, 0, len(s.series))
	for k, v := range s.series {
		series = append(series, catalog.SeriesDelta{
			Bucket: time.Unix(k.bucket, 0).UTC(), Project: k.project, Eco: k.eco,
			Outcome: k.outcome, Count: v.count, Bytes: v.bytes,
		})
	}
	s.access = make(map[accessKey]*accessValue)
	s.traffic = make(map[trafficKey]*catalog.TrafficDelta)
	s.touch = make(map[catalog.EntryKey]*touchValue)
	s.series = make(map[seriesKey]*seriesValue)
	s.mu.Unlock()
	return access, traffic, touch, series
}

// Flush persists the accumulated window.
//
// On failure the window is dropped rather than re-buffered: retaining it would let
// memory grow without bound while the database is unavailable, and these are usage
// counters, not money.
//
// For the time series that loss shows up as a missing bucket rather than a slightly
// low total, which is why readers must render absent buckets as gaps. A chart that
// interpolates or plots zero there would state confidently that no traffic happened,
// when the truth is that nobody knows.
func (e *Engine) Flush() error {
	access, traffic, touch, series := e.stats.drain()
	if len(access) == 0 && len(traffic) == 0 && len(touch) == 0 && len(series) == 0 {
		return nil
	}
	return errors.Join(
		e.cat.RecordAccess(access, traffic),
		e.cat.TouchEntries(touch),
		e.cat.RecordSeries(series, nil),
	)
}
