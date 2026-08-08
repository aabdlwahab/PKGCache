package catalog

import (
	"database/sql"
	"fmt"

	"strings"
	"time"
)

// Bucket floors a timestamp to the start of its span. Exported because the engine's
// in-memory collector keys by bucket and must agree with the store exactly.
func Bucket(t time.Time, span int64) time.Time {
	if span <= 0 {
		span = SpanFine
	}
	seconds := t.Unix()
	// Go's integer division truncates toward zero, which would round a pre-1970
	// timestamp the wrong way. Clocks that far off are a different problem, but the
	// floor should still be a floor.
	if seconds < 0 {
		seconds -= span - 1
	}
	return time.Unix(seconds/span*span, 0).UTC()
}

// RecordSeries folds one flush window into the fine-resolution series. Both slices
// are optional; the whole window is one transaction so a crash cannot leave traffic
// recorded without its upstream counterpart.
func (d *DB) RecordSeries(traffic []SeriesDelta, upstream []UpstreamDelta) error {
	if len(traffic) == 0 && len(upstream) == 0 {
		return nil
	}
	err := d.inTx(func(tx *sql.Tx) error {
		if len(traffic) > 0 {
			st, err := tx.Prepare(
				`INSERT INTO traffic_series(span, bucket, project, eco, outcome, count, bytes)
				 VALUES (?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(span, bucket, project, eco, outcome) DO UPDATE SET
				   count = count + excluded.count,
				   bytes = bytes + excluded.bytes`)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			for _, t := range traffic {
				if _, err := st.Exec(SpanFine, ts(Bucket(t.Bucket, SpanFine)),
					t.Project, t.Eco, t.Outcome, t.Count, t.Bytes); err != nil {
					return err
				}
			}
		}
		if len(upstream) > 0 {
			st, err := tx.Prepare(
				`INSERT INTO upstream_series(span, bucket, project, upstream,
				   requests, errors, bytes, ms_sum, ms_max)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(span, bucket, project, upstream) DO UPDATE SET
				   requests = requests + excluded.requests,
				   errors   = errors   + excluded.errors,
				   bytes    = bytes    + excluded.bytes,
				   ms_sum   = ms_sum   + excluded.ms_sum,
				   ms_max   = MAX(ms_max, excluded.ms_max)`)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			// Upstream health is only ever read hourly, so it is written hourly. There
			// is no compaction step for it and nothing to fold.
			for _, u := range upstream {
				if _, err := st.Exec(SpanHour, ts(Bucket(u.Bucket, SpanHour)),
					u.Project, u.Upstream, u.Requests, u.Errors, u.Bytes,
					u.MillisSum, u.MillisMax); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: record series: %w", err)
	}
	return nil
}

// TrafficSeries reads one resolution of the traffic history.
//
// Buckets with no traffic are absent rather than zero-filled. That distinction is
// load-bearing: a flush that failed drops its window (see engine.Flush), and a chart
// must be able to draw that as a gap instead of confidently asserting no traffic.
func (d *DB) TrafficSeries(q SeriesQuery) ([]TrafficPoint, error) {
	if err := d.Flush(); err != nil {
		return nil, err
	}
	span := q.Span
	if span == 0 {
		span = SpanFine
	}

	group := []string{"bucket"}
	for _, field := range strings.Split(q.GroupBy, ",") {
		switch strings.TrimSpace(field) {
		case "eco":
			group = append(group, "eco")
		case "outcome":
			group = append(group, "outcome")
		}
	}
	selected := map[string]bool{}
	for _, field := range group {
		selected[field] = true
	}

	columns := []string{"bucket"}
	for _, field := range []string{"eco", "outcome"} {
		if selected[field] {
			columns = append(columns, field)
		} else {
			columns = append(columns, "''")
		}
	}

	where := []string{"span = ?"}
	args := []any{span}
	if q.Project != "" {
		where, args = append(where, "project = ?"), append(args, q.Project)
	}
	if q.Eco != "" {
		where, args = append(where, "eco = ?"), append(args, q.Eco)
	}
	if !q.From.IsZero() {
		where, args = append(where, "bucket >= ?"), append(args, ts(Bucket(q.From, span)))
	}
	if !q.To.IsZero() {
		where, args = append(where, "bucket <= ?"), append(args, ts(q.To))
	}

	query := `SELECT ` + strings.Join(columns, ", ") + `, SUM(count), SUM(bytes)
	          FROM traffic_series WHERE ` + strings.Join(where, " AND ") + `
	          GROUP BY ` + strings.Join(group, ", ") + `
	          ORDER BY bucket, eco, outcome`

	rows, err := d.read.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: traffic series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	points := []TrafficPoint{}
	for rows.Next() {
		var (
			bucket int64
			point  TrafficPoint
		)
		if err := rows.Scan(&bucket, &point.Eco, &point.Outcome,
			&point.Count, &point.Bytes); err != nil {
			return nil, fmt.Errorf("catalog: traffic series: %w", err)
		}
		point.Bucket = fromTS(bucket)
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: traffic series: %w", err)
	}
	return points, nil
}

// UpstreamSeries reads per-upstream health. Mean latency is derived here rather than
// stored, so the sum stays addable across compaction and across upstreams.
func (d *DB) UpstreamSeries(q SeriesQuery) ([]UpstreamPoint, error) {
	if err := d.Flush(); err != nil {
		return nil, err
	}
	where := []string{"span = ?"}
	args := []any{SpanHour}
	if q.Project != "" {
		where, args = append(where, "project = ?"), append(args, q.Project)
	}
	if !q.From.IsZero() {
		where, args = append(where, "bucket >= ?"), append(args, ts(Bucket(q.From, SpanHour)))
	}
	if !q.To.IsZero() {
		where, args = append(where, "bucket <= ?"), append(args, ts(q.To))
	}

	rows, err := d.read.Query(
		`SELECT bucket, upstream, SUM(requests), SUM(errors), SUM(bytes),
		        SUM(ms_sum), MAX(ms_max)
		 FROM upstream_series WHERE `+strings.Join(where, " AND ")+`
		 GROUP BY bucket, upstream ORDER BY bucket, upstream`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: upstream series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	points := []UpstreamPoint{}
	for rows.Next() {
		var (
			bucket   int64
			sumMilli int64
			point    UpstreamPoint
		)
		if err := rows.Scan(&bucket, &point.Upstream, &point.Requests, &point.Errors,
			&point.Bytes, &sumMilli, &point.MaxMillis); err != nil {
			return nil, fmt.Errorf("catalog: upstream series: %w", err)
		}
		point.Bucket = fromTS(bucket)
		if point.Requests > 0 {
			point.MeanMillis = sumMilli / point.Requests
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: upstream series: %w", err)
	}
	return points, nil
}

// SampleStorage records one observation. Re-sampling inside the same hour replaces
// the row rather than accumulating: this is a gauge, not a counter.
func (d *DB) SampleStorage(s StorageSample) error {
	bucket := ts(Bucket(s.Bucket, SpanHour))
	_, err := d.write.Exec(
		`INSERT INTO storage_series(bucket, blob_count, blob_bytes,
		   entry_count, entry_bytes, fs_free, fs_total)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(bucket) DO UPDATE SET
		   blob_count = excluded.blob_count, blob_bytes = excluded.blob_bytes,
		   entry_count = excluded.entry_count, entry_bytes = excluded.entry_bytes,
		   fs_free = excluded.fs_free, fs_total = excluded.fs_total`,
		bucket, s.BlobCount, s.BlobBytes, s.EntryCount, s.EntryBytes, s.FSFree, s.FSTotal)
	if err != nil {
		return fmt.Errorf("catalog: sample storage: %w", err)
	}
	return nil
}

// StorageSeries reads the growth history.
func (d *DB) StorageSeries(from, to time.Time) ([]StorageSample, error) {
	where := []string{"1 = 1"}
	args := []any{}
	if !from.IsZero() {
		where, args = append(where, "bucket >= ?"), append(args, ts(Bucket(from, SpanHour)))
	}
	if !to.IsZero() {
		where, args = append(where, "bucket <= ?"), append(args, ts(to))
	}
	rows, err := d.read.Query(
		`SELECT bucket, blob_count, blob_bytes, entry_count, entry_bytes, fs_free, fs_total
		 FROM storage_series WHERE `+strings.Join(where, " AND ")+` ORDER BY bucket`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: storage series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	samples := []StorageSample{}
	for rows.Next() {
		var (
			bucket int64
			sample StorageSample
		)
		if err := rows.Scan(&bucket, &sample.BlobCount, &sample.BlobBytes,
			&sample.EntryCount, &sample.EntryBytes, &sample.FSFree,
			&sample.FSTotal); err != nil {
			return nil, fmt.Errorf("catalog: storage series: %w", err)
		}
		sample.Bucket = fromTS(bucket)
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: storage series: %w", err)
	}
	return samples, nil
}

// StorageTotals reports the store's size on both sides of deduplication.
func (d *DB) StorageTotals() (StorageTotals, error) {
	if err := d.Flush(); err != nil {
		return StorageTotals{}, err
	}
	var totals StorageTotals
	if err := d.read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs`,
	).Scan(&totals.BlobCount, &totals.BlobBytes); err != nil {
		return StorageTotals{}, fmt.Errorf("catalog: storage totals: %w", err)
	}
	if err := d.read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM entries`,
	).Scan(&totals.EntryCount, &totals.EntryBytes); err != nil {
		return StorageTotals{}, fmt.Errorf("catalog: storage totals: %w", err)
	}
	return totals, nil
}

// ageBounds are the histogram's edges in days. The last bucket is open-ended.
var ageBounds = []int{1, 7, 30, 90, 365}

// EntryAges buckets cached entries by how long it has been since anything read them.
// This is the same ordering eviction uses, so the histogram is a preview of what the
// next pass would take.
//
// The bucketing happens in SQL rather than in a scan loop here: on a large store this
// is the difference between aggregating in the database and streaming every row of
// the entries table across the driver to be counted.
func (d *DB) EntryAges(project string, now time.Time) ([]AgeBucket, error) {
	if err := d.Flush(); err != nil {
		return nil, err
	}
	last := len(ageBounds)

	// An entry with no recorded read is as cold as it gets. Falling through to the
	// arithmetic instead would date it to 1970 — the same bucket by luck, but for the
	// wrong reason, and wrong the moment the last bound changes.
	branches := []string{fmt.Sprintf("WHEN last_access = 0 THEN %d", last)}
	args := []any{}
	for index, bound := range ageBounds {
		branches = append(branches, fmt.Sprintf("WHEN ? - last_access < %d THEN %d",
			int64(bound)*86400, index))
		args = append(args, ts(now))
	}
	bucketing := "CASE " + strings.Join(branches, " ") + fmt.Sprintf(" ELSE %d END", last)

	where := ""
	if project != "" {
		where = " WHERE project = ?"
		args = append(args, project)
	}
	rows, err := d.read.Query(
		`SELECT `+bucketing+` AS age, COUNT(*), COALESCE(SUM(size), 0)
		 FROM entries`+where+` GROUP BY 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: entry ages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make([]AgeBucket, 0, last+1)
	for _, bound := range ageBounds {
		buckets = append(buckets, AgeBucket{Label: "under " + ageLabel(bound), MaxAgeDays: bound})
	}
	buckets = append(buckets, AgeBucket{Label: "over " + ageLabel(ageBounds[last-1])})

	for rows.Next() {
		var index int
		var entries, bytes int64
		if err := rows.Scan(&index, &entries, &bytes); err != nil {
			return nil, fmt.Errorf("catalog: entry ages: %w", err)
		}
		if index < 0 || index > last {
			continue
		}
		buckets[index].Entries, buckets[index].Bytes = entries, bytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: entry ages: %w", err)
	}
	return buckets, nil
}

func ageLabel(days int) string {
	switch {
	case days == 1:
		return "24h"
	case days < 30:
		return fmt.Sprintf("%dd", days)
	case days < 365:
		return fmt.Sprintf("%dm", days/30)
	default:
		return fmt.Sprintf("%dy", days/365)
	}
}

// CompactSeries folds expired fine buckets into hourly ones and expired hourly
// buckets into daily ones, then drops what has outlived the longest tier.
//
// Each fold and its delete share a transaction. That pairing is what makes the
// operation exactly-once: the ON CONFLICT clause adds to whatever is already in the
// coarser bucket, which would double-count on a re-run, and the only thing preventing
// a re-run from seeing the same source rows is that they were deleted alongside.
func (d *DB) CompactSeries(now time.Time) error {
	folds := []struct {
		from, to  int64
		retention time.Duration
	}{
		{SpanFine, SpanHour, RetainFine},
		{SpanHour, SpanDay, RetainHour},
	}
	for _, fold := range folds {
		cutoff := ts(Bucket(now.Add(-fold.retention), fold.to))
		err := d.inTx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(
				`INSERT INTO traffic_series(span, bucket, project, eco, outcome, count, bytes)
				 SELECT ?, bucket / ? * ?, project, eco, outcome, SUM(count), SUM(bytes)
				 FROM traffic_series WHERE span = ? AND bucket < ?
				 GROUP BY bucket / ?, project, eco, outcome
				 ON CONFLICT(span, bucket, project, eco, outcome) DO UPDATE SET
				   count = count + excluded.count,
				   bytes = bytes + excluded.bytes`,
				fold.to, fold.to, fold.to, fold.from, cutoff, fold.to); err != nil {
				return err
			}
			_, err := tx.Exec(
				`DELETE FROM traffic_series WHERE span = ? AND bucket < ?`, fold.from, cutoff)
			return err
		})
		if err != nil {
			return fmt.Errorf("catalog: compact series: %w", err)
		}
	}

	expiries := []struct {
		table  string
		span   int64
		retain time.Duration
	}{
		{"traffic_series", SpanDay, RetainDay},
		{"upstream_series", SpanHour, RetainHour},
	}
	for _, expiry := range expiries {
		cutoff := ts(now.Add(-expiry.retain))
		if _, err := d.write.Exec(
			`DELETE FROM `+expiry.table+` WHERE span = ? AND bucket < ?`,
			expiry.span, cutoff); err != nil {
			return fmt.Errorf("catalog: expire %s: %w", expiry.table, err)
		}
	}
	if _, err := d.write.Exec(
		`DELETE FROM storage_series WHERE bucket < ?`, ts(now.Add(-RetainDay))); err != nil {
		return fmt.Errorf("catalog: expire storage_series: %w", err)
	}
	return nil
}
