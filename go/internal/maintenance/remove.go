package maintenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// Removing named content, as opposed to evicting whatever is coldest.
//
// Evict is a policy: a target size, an age, and least-recently-used order. This is the
// other half somebody actually wants on a laptop — "I know which 2.5 GB wheel I am never
// going to build against again, take that one" — and there was no way to say it.
//
// It is deliberately not a job. Eviction sweeps the whole store and reports progress
// worth watching; this touches the handful of digests somebody selected, and a queue plus
// a poll between the click and the answer would be machinery around nothing.

// RemoveOptions names content to drop from one project.
type RemoveOptions struct {
	Project string
	// Digests is what to remove. By digest rather than by entry key because that is what
	// a person picked off a list: two keys in a project can point at the same bytes — a
	// mutable tag beside the version it resolves to — and removing one while leaving the
	// other would look like the click did nothing.
	Digests map[blob.Digest]struct{}
	DryRun  bool
}

// RemoveResult reports what one removal did, or would have done.
type RemoveResult struct {
	// Entries and Blobs count what was dropped; Pinned counts what a checkpoint held on
	// to, which is the one case where a removal somebody asked for does not happen.
	Entries        int64 `json:"entries"`
	Blobs          int64 `json:"blobs"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
	Pinned         int64 `json:"pinned"`
	// Missing counts digests that were named and not found, so a stale list in a browser
	// is reported rather than silently succeeding.
	Missing int64 `json:"missing"`
}

// MaxRemove bounds one call. A person selecting rows in a window cannot exceed this, and
// a caller that wants the whole store wants Evict.
const MaxRemove = 500

// Remove drops the named content from one project.
//
// A digest held by a checkpoint is refused rather than removed: the checkpoint is a
// promise that a pack exported from it can be restored, and quietly breaking that to
// satisfy a click is the wrong way round. It is reported so the caller can say so.
func (s *Service) Remove(ctx context.Context, options RemoveOptions) (RemoveResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	if options.Project == "" {
		return RemoveResult{}, errors.New("remove: a project is required")
	}
	if len(options.Digests) == 0 {
		return RemoveResult{}, nil
	}
	if len(options.Digests) > MaxRemove {
		return RemoveResult{}, fmt.Errorf(
			"remove: %d digests is more than one call may carry (%d); "+
				"select fewer, or use eviction for a whole store",
			len(options.Digests), MaxRemove)
	}
	pins, err := s.snapshotPins(ctx)
	if err != nil {
		return RemoveResult{}, err
	}

	var result RemoveResult
	// Tracks which of the requested digests were actually seen, so the ones that were not
	// can be reported instead of counted as done.
	seen := make(map[blob.Digest]struct{}, len(options.Digests))
	// A walk rather than a query per digest: the project's entries are indexed and this
	// is one pass, where five hundred point lookups are five hundred round trips into
	// SQLite for the same rows.
	err = s.Catalog.WalkEvictionCandidates(options.Project, func(entry catalog.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, wanted := options.Digests[entry.Digest]; !wanted {
			return nil
		}
		seen[entry.Digest] = struct{}{}
		if _, pinned := pins[entry.Digest]; pinned {
			result.Pinned++
			return nil
		}
		if options.DryRun {
			result.Entries++
			return nil
		}
		digest, _, referenced, err := s.Catalog.EvictEntry(entry.EntryKey)
		if errors.Is(err, catalog.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		result.Entries++
		if s.Metrics != nil {
			s.Metrics.EvictedEntries.WithLabelValues(entry.Project, "manual").Inc()
		}
		if referenced {
			// Another project, or another key here, still points at these bytes. Content
			// is shared by digest, so the entry goes and the blob stays.
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
		result.Blobs++
		result.ReclaimedBytes += stat.Size
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("remove: %w", err)
	}
	result.Missing = int64(len(options.Digests) - len(seen))
	if !options.DryRun {
		s.updateMetrics(result.ReclaimedBytes)
	}
	return result, nil
}
