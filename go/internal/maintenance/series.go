package maintenance

import (
	"context"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/diskusage"
)

// SeriesInterval is how often storage is sampled and the traffic series is folded.
// It matches the coarsest bucket the compactor writes into, so a fold never has to
// reach back through more than one hour of fine buckets it has already seen.
const SeriesInterval = time.Hour

// SampleStorage records one observation of how much is stored and how much room is
// left.
//
// Growth is sampled rather than derived. Summing blobs.created_at would look
// equivalent and cost nothing, but GC deletes those rows — a derived history would
// quietly rewrite its own past every time the collector ran, and a storage chart that
// revises yesterday is worse than no chart.
func (s *Service) SampleStorage(now time.Time) error {
	totals, err := s.Catalog.StorageTotals()
	if err != nil {
		return err
	}
	free, total, err := diskusage.Usage(s.Blobs.Root())
	if err != nil {
		// Skip the whole sample rather than storing zeros. A missing point draws as a
		// gap; a zero draws as a full disk, and something would page on it.
		return err
	}
	return s.Catalog.SampleStorage(catalog.StorageSample{
		Bucket:     now,
		BlobCount:  totals.BlobCount,
		BlobBytes:  totals.BlobBytes,
		EntryCount: totals.EntryCount,
		EntryBytes: totals.EntryBytes,
		FSFree:     free,
		FSTotal:    total,
	})
}

// maintainSeries is the scheduled pass: sample, then compact. Failures are logged by
// the caller's own accounting and never abort the schedule — losing one hour of
// history is not a reason to stop keeping history.
func (s *Service) maintainSeries(now time.Time) error {
	sampleErr := s.SampleStorage(now)
	if err := s.Catalog.CompactSeries(now); err != nil {
		return err
	}
	return sampleErr
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// startSeries samples once immediately so a freshly started instance shows a point
// rather than an empty chart for its first hour, then keeps to the interval.
func (s *Service) startSeries(ctx context.Context) {
	_ = s.maintainSeries(s.clock())
	s.schedule(ctx, SeriesInterval, func(context.Context) {
		_ = s.maintainSeries(s.clock())
	})
}
