// Package maintenance owns online storage reclamation and its scheduler.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/job"
	"github.com/aabdlwahab/PKGCache/internal/diskusage"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

// Service coordinates collectors so scheduled, API and CLI runs never overlap.
type Service struct {
	Catalog catalog.Store
	Blobs   *blob.Store
	Config  *config.Store
	Metrics *obs.Metrics
	Now     func() time.Time

	runMu sync.Mutex
	wg    sync.WaitGroup
}

// GCOptions configures a collection pass. Grace protects blobs committed within the
// window, which is what makes collecting safe while writers are running.
type GCOptions struct {
	Grace  time.Duration
	DryRun bool
}

// GCResult reports what one collection pass examined and reclaimed. Candidates counts
// blobs that looked unreferenced; Deleted counts those still unreferenced at the moment
// of removal, so the difference is concurrent publication that was correctly spared.
type GCResult struct {
	Scanned        int64 `json:"scanned"`
	Candidates     int64 `json:"candidates"`
	Deleted        int64 `json:"deleted"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
	Pinned         int64 `json:"pinned"`
}

// GC performs an online mark-and-sweep. The grace period protects newly committed
// blobs; DeleteIf and the final catalog recheck protect concurrent publication.
func (s *Service) GC(ctx context.Context, options GCOptions) (GCResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.Now == nil {
		s.Now = time.Now
	}
	if options.Grace < 0 {
		return GCResult{}, errors.New("gc: grace cannot be negative")
	}
	pins, err := s.snapshotPins(ctx)
	if err != nil {
		return GCResult{}, err
	}
	cutoff := s.Now().Add(-options.Grace)
	var result GCResult
	err = s.Blobs.Walk(func(digest blob.Digest, stat blob.Stat) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Scanned++
		if _, pinned := pins[digest]; pinned {
			result.Pinned++
			return nil
		}
		if stat.ModTime.After(cutoff) {
			return nil
		}
		referenced, err := s.Catalog.IsBlobReferenced(digest)
		if err != nil || referenced {
			return err
		}
		result.Candidates++
		if options.DryRun {
			result.ReclaimedBytes += stat.Size
			return nil
		}
		deleted, err := s.Blobs.DeleteIf(digest, func(current blob.Stat) (bool, error) {
			if current.ModTime.After(cutoff) {
				return false, nil
			}
			if _, pinned := pins[digest]; pinned {
				return false, nil
			}
			live, err := s.Catalog.IsBlobReferenced(digest)
			return !live, err
		})
		if err != nil || !deleted {
			return err
		}
		if err := s.Catalog.DeleteBlobRecord(digest); err != nil {
			return err
		}
		result.Deleted++
		result.ReclaimedBytes += stat.Size
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("gc: %w", err)
	}
	if !options.DryRun {
		s.updateMetrics(result.ReclaimedBytes)
	}
	return result, nil
}

// EvictOptions configures an eviction pass. TargetBytes and MinFreeBytes are size
// goals; TTL evicts by idle time. An empty Project evicts across the instance.
type EvictOptions struct {
	Project      string
	TargetBytes  int64
	MinFreeBytes int64
	TTL          time.Duration
	DryRun       bool
}

// EvictResult reports one eviction pass. BeforeBytes and AfterBytes bracket store
// usage, and Pinned counts entries a checkpoint protected from removal.
type EvictResult struct {
	Scanned        int64 `json:"scanned"`
	EvictedEntries int64 `json:"evicted_entries"`
	DeletedBlobs   int64 `json:"deleted_blobs"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
	Pinned         int64 `json:"pinned"`
	BeforeBytes    int64 `json:"before_bytes"`
	AfterBytes     int64 `json:"after_bytes"`
}

// Evict applies TTL first and then LRU until the target and filesystem floor hold.
func (s *Service) Evict(ctx context.Context, options EvictOptions) (EvictResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if options.TargetBytes < 0 || options.MinFreeBytes < 0 || options.TTL < 0 {
		return EvictResult{}, errors.New("evict: policy values cannot be negative")
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	pins, err := s.snapshotPins(ctx)
	if err != nil {
		return EvictResult{}, err
	}
	_, usage, err := s.Blobs.Usage()
	if err != nil {
		return EvictResult{}, err
	}
	free, err := filesystemFree(s.Blobs.Root())
	if err != nil {
		return EvictResult{}, err
	}
	result := EvictResult{BeforeBytes: usage, AfterBytes: usage}
	expireBefore := s.Now().Add(-options.TTL)
	err = s.Catalog.WalkEvictionCandidates(options.Project, func(entry catalog.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Scanned++
		expired := options.TTL > 0 && entry.LastAccess.Before(expireBefore)
		overTarget := options.TargetBytes > 0 && result.AfterBytes > options.TargetBytes
		belowFloor := options.MinFreeBytes > 0 && free < options.MinFreeBytes
		if !expired && !overTarget && !belowFloor {
			return nil
		}
		if _, pinned := pins[entry.Digest]; pinned {
			result.Pinned++
			return nil
		}
		reason := "lru"
		if expired {
			reason = "ttl"
		} else if belowFloor {
			reason = "free-space"
		}
		if options.DryRun {
			result.EvictedEntries++
			return nil
		}
		digest, _, referenced, err := s.Catalog.EvictEntry(entry.EntryKey)
		if errors.Is(err, catalog.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		result.EvictedEntries++
		if s.Metrics != nil {
			s.Metrics.EvictedEntries.WithLabelValues(entry.Project, reason).Inc()
		}
		if referenced {
			return nil
		}
		stat, _ := s.Catalog.GetBlob(digest)
		deleted, err := s.Blobs.DeleteIf(digest, func(_ blob.Stat) (bool, error) {
			if _, pinned := pins[digest]; pinned {
				return false, nil
			}
			live, err := s.Catalog.IsBlobReferenced(digest)
			return !live, err
		})
		if err != nil || !deleted {
			return err
		}
		if err := s.Catalog.DeleteBlobRecord(digest); err != nil {
			return err
		}
		result.DeletedBlobs++
		result.ReclaimedBytes += stat.Size
		result.AfterBytes -= stat.Size
		free += stat.Size
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("evict: %w", err)
	}
	if !options.DryRun {
		s.updateMetrics(result.ReclaimedBytes)
	}
	return result, nil
}

func (s *Service) snapshotPins(ctx context.Context) (map[blob.Digest]struct{}, error) {
	pins := make(map[blob.Digest]struct{})
	err := s.Catalog.WalkSnapshots(func(checkpoint catalog.Snapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		pins[checkpoint.Manifest] = struct{}{}
		file, _, err := s.Blobs.Open(checkpoint.Manifest)
		if err != nil {
			return fmt.Errorf("snapshot %s manifest: %w", checkpoint.ID, err)
		}
		iterator, err := snapshot.NewIterator(file)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("snapshot %s manifest: %w", checkpoint.ID, err)
		}
		_, _, walkErr := iterator.Walk(func(entry snapshot.Entry) error {
			pins[entry.Digest] = struct{}{}
			return ctx.Err()
		})
		closeErr := iterator.Close()
		fileErr := file.Close()
		return errors.Join(walkErr, closeErr, fileErr)
	})
	return pins, err
}

func (s *Service) updateMetrics(reclaimed int64) {
	if s.Metrics == nil {
		return
	}
	if reclaimed > 0 {
		s.Metrics.GCReclaimed.Add(float64(reclaimed))
	}
	count, bytes, err := s.Blobs.Usage()
	if err == nil {
		s.Metrics.BlobCount.Set(float64(count))
		s.Metrics.BlobStoreBytes.Set(float64(bytes))
	}
}

func filesystemFree(path string) (int64, error) {
	free, _, err := diskusage.Usage(path)
	if err != nil {
		return 0, fmt.Errorf("evict: %w", err)
	}
	return free, nil
}

// Register exposes manual maintenance through the durable job queue.
func (s *Service) Register(manager *job.Manager) {
	manager.Register("gc", s.gcJob)
	manager.Register("evict", s.evictJob)
}

func (s *Service) gcJob(ctx context.Context, record control.Job, logf func(string)) error {
	cfg := s.Config.Current().Maintenance
	result, err := s.GC(ctx, GCOptions{
		Grace:  durationParam(record.Params, "grace", cfg.GCGrace),
		DryRun: boolParam(record.Params, "dry_run"),
	})
	logf(fmt.Sprintf(
		"scanned=%d candidates=%d deleted=%d reclaimed_bytes=%d pinned=%d",
		result.Scanned, result.Candidates, result.Deleted, result.ReclaimedBytes, result.Pinned,
	))
	return err
}

func (s *Service) evictJob(ctx context.Context, record control.Job, logf func(string)) error {
	cfg := s.Config.Current().Maintenance
	result, err := s.Evict(ctx, EvictOptions{
		Project: record.Project, TargetBytes: int64Param(record.Params, "target_bytes", cfg.EvictTargetBytes),
		MinFreeBytes: int64Param(record.Params, "min_free_bytes", cfg.EvictMinFreeBytes),
		TTL:          durationParam(record.Params, "ttl", cfg.EvictTTL),
		DryRun:       boolParam(record.Params, "dry_run"),
	})
	logf(fmt.Sprintf(
		"scanned=%d evicted_entries=%d deleted_blobs=%d reclaimed_bytes=%d before_bytes=%d after_bytes=%d pinned=%d",
		result.Scanned, result.EvictedEntries, result.DeletedBlobs, result.ReclaimedBytes,
		result.BeforeBytes, result.AfterBytes, result.Pinned,
	))
	return err
}

// Start runs interval schedules until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	cfg := s.Config.Current().Maintenance
	if cfg.GCInterval > 0 {
		s.schedule(ctx, cfg.GCInterval, func(run context.Context) {
			_, _ = s.GC(run, GCOptions{Grace: s.Config.Current().Maintenance.GCGrace})
		})
	}
	// The series keeps its own clock. Both halves are cheap, and neither should stop
	// because an operator turned GC or eviction off — the history is what explains
	// why they might want to turn one back on.
	s.startSeries(ctx)
	if cfg.EvictInterval > 0 &&
		(cfg.EvictTargetBytes > 0 || cfg.EvictMinFreeBytes > 0 || cfg.EvictTTL > 0) {
		s.schedule(ctx, cfg.EvictInterval, func(run context.Context) {
			current := s.Config.Current().Maintenance
			_, _ = s.Evict(run, EvictOptions{
				TargetBytes:  current.EvictTargetBytes,
				MinFreeBytes: current.EvictMinFreeBytes, TTL: current.EvictTTL,
			})
		})
	}
}

func (s *Service) schedule(ctx context.Context, interval time.Duration, run func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run(ctx)
			}
		}
	}()
}

// Wait joins scheduler goroutines after their context is cancelled.
func (s *Service) Wait() { s.wg.Wait() }

func durationParam(params map[string]any, key string, fallback time.Duration) time.Duration {
	if raw, ok := params[key].(string); ok && raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed
		}
	}
	return fallback
}

func int64Param(params map[string]any, key string, fallback int64) int64 {
	switch value := params[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	}
	return fallback
}

func boolParam(params map[string]any, key string) bool {
	value, _ := params[key].(bool)
	return value
}
