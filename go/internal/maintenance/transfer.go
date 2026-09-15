package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// Copying or moving packages from one project to another.
//
// Content is shared by digest, so neither copies a byte: both write rows in the other
// project pointing at bytes the store already holds. What makes it more than an INSERT is
// what a package needs beside its file to be served somewhere else — npm reaches a tarball
// through its packument, an image is a manifest and its layers — and that is each
// ecosystem's own knowledge, asked of it through Companions rather than written here.
//
// Synchronous, like Remove, and for the same reason: it is the handful of packages
// somebody ticked in a list.

// TransferOptions names packages to copy or move between two projects.
type TransferOptions struct {
	From, To string
	// Digests is what to transfer, by digest for the reason RemoveOptions gives.
	Digests map[blob.Digest]struct{}
	// Move takes the packages out of From once To holds them.
	Move bool
	// Companions names the keys that must travel with an entry, and is asked again about
	// each one it names. Nil means nothing travels but the selected entries themselves.
	Companions func(eco, key string, read func() ([]byte, error)) ([]string, error)
}

// TransferResult reports what one transfer did.
type TransferResult struct {
	// Packages counts the selected digests To now holds.
	Packages int64 `json:"packages"`
	// Copied counts entries written into To, and Companions how many of those travelled
	// alongside a selected package rather than being selected.
	Copied     int64 `json:"copied"`
	Companions int64 `json:"companions"`
	// AlreadyThere counts entries To already held with the same content.
	AlreadyThere int64 `json:"already_there"`
	// Conflicts counts entries To holds under the same key with different content. Those
	// are left alone, and a move leaves the source's copy in place too: taking it away
	// would leave the package nowhere.
	Conflicts int64 `json:"conflicts"`
	// Removed counts entries a move took out of From, and Pinned those it could not
	// because a checkpoint holds them.
	Removed int64 `json:"removed"`
	Pinned  int64 `json:"pinned"`
	Missing int64 `json:"missing"`
}

// maxCompanionEntries bounds how far companions may spread. An image index of a dozen
// platforms is a few hundred entries; ten thousand means an answer that keeps naming more.
const maxCompanionEntries = 10000

// maxCompanionRead bounds a document an ecosystem reads to answer Companions. Manifests
// and indexes are kilobytes; anything past this is not one.
const maxCompanionRead = 8 << 20

// Transfer copies, or moves, the named packages from one project to another.
func (s *Service) Transfer(ctx context.Context, options TransferOptions) (TransferResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	var result TransferResult
	switch {
	case options.From == "" || options.To == "":
		return result, errors.New("transfer: both projects are required")
	case options.From == options.To:
		return result, fmt.Errorf("transfer: %s is already where these packages are", options.To)
	case len(options.Digests) == 0:
		return result, nil
	case len(options.Digests) > MaxRemove:
		return result, fmt.Errorf("transfer: %d digests is more than one call may carry (%d)",
			len(options.Digests), MaxRemove)
	}

	// Collected before anything is written. The walk is a read over the rows this is about
	// to add to and remove from.
	var selected []catalog.Entry
	seen := make(map[blob.Digest]struct{}, len(options.Digests))
	if err := s.Catalog.WalkEntries(options.From, func(entry catalog.Entry) error {
		if _, wanted := options.Digests[entry.Digest]; wanted {
			selected = append(selected, entry)
			seen[entry.Digest] = struct{}{}
		}
		return ctx.Err()
	}); err != nil {
		return result, fmt.Errorf("transfer: %w", err)
	}
	result.Missing = int64(len(options.Digests) - len(seen))

	planned, companions, err := s.withCompanions(ctx, selected, options.Companions)
	if err != nil {
		return result, err
	}

	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	quota := s.projectQuota(options.To)
	landed := make(map[blob.Digest]struct{})
	conflicted := make(map[catalog.EntryKey]struct{})
	for _, entry := range planned {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		target := catalog.EntryKey{Project: options.To, Eco: entry.Eco, Key: entry.Key}
		existing, err := s.Catalog.GetEntry(target)
		switch {
		case err == nil && existing.Digest == entry.Digest:
			result.AlreadyThere++
			landed[entry.Digest] = struct{}{}
			continue
		case err == nil:
			result.Conflicts++
			conflicted[entry.EntryKey] = struct{}{}
			continue
		case !errors.Is(err, catalog.ErrNotFound):
			return result, fmt.Errorf("transfer: %w", err)
		}
		// Under the blob's lifecycle lock, as the engine links a dedup hit: a collector
		// running now must not delete the bytes between this row being decided on and
		// being written.
		err = s.Blobs.WithBlob(entry.Digest, func(blob.Stat) error {
			return s.Catalog.CommitEntry(catalog.Entry{
				EntryKey: target, Digest: entry.Digest, Size: entry.Size,
				MediaType: entry.MediaType, CachedAt: entry.CachedAt, LastAccess: now,
			}, nil, quota, false)
		})
		if err != nil {
			return result, fmt.Errorf("transfer: %s %s into %s: %w",
				entry.Eco, entry.Key, options.To, err)
		}
		result.Copied++
		if _, companion := companions[entry.EntryKey]; companion {
			result.Companions++
		}
		landed[entry.Digest] = struct{}{}
		// A document's freshness record goes with it, so the other project treats the
		// index it received as the age it is rather than as never fetched.
		if ref, err := s.Catalog.GetRef(catalog.RefKey{
			Project: options.From, Eco: entry.Eco, Name: entry.Key,
		}); err == nil {
			ref.Project = options.To
			if _, err := s.Catalog.GetRef(ref.RefKey); errors.Is(err, catalog.ErrNotFound) {
				if err := s.Catalog.PutRef(ref); err != nil {
					return result, fmt.Errorf("transfer: %w", err)
				}
			}
		}
	}

	// The inventory rows, for the packages that arrived. Read before a move removes them.
	artifacts, _, err := s.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: options.From})
	if err != nil {
		return result, fmt.Errorf("transfer: %w", err)
	}
	for _, artifact := range artifacts {
		if _, wanted := options.Digests[artifact.Digest]; !wanted {
			continue
		}
		if _, arrived := landed[artifact.Digest]; !arrived {
			continue
		}
		artifact.Project = options.To
		if err := s.Catalog.PutArtifact(artifact); err != nil {
			return result, fmt.Errorf("transfer: %w", err)
		}
	}
	for digest := range options.Digests {
		if _, arrived := landed[digest]; arrived {
			result.Packages++
		}
	}

	if !options.Move {
		return result, nil
	}
	// Only the selected entries leave. Companions stay: the index that led to this
	// package also leads to that project's other versions of it.
	pins, err := s.snapshotPins(ctx)
	if err != nil {
		return result, err
	}
	for _, entry := range selected {
		if _, clash := conflicted[entry.EntryKey]; clash {
			continue
		}
		if _, pinned := pins[entry.Digest]; pinned {
			result.Pinned++
			continue
		}
		// Eviction rather than deletion: it takes the inventory rows with the entry. The
		// blob it reports on is still referenced by To, so nothing is reclaimed.
		if _, _, _, err := s.Catalog.EvictEntry(entry.EntryKey); err != nil &&
			!errors.Is(err, catalog.ErrNotFound) {
			return result, fmt.Errorf("transfer: %w", err)
		}
		result.Removed++
	}
	return result, nil
}

// withCompanions returns the selected entries followed by everything they need beside
// them that the source project holds, and which of those were not selected.
func (s *Service) withCompanions(
	ctx context.Context,
	selected []catalog.Entry,
	companionsOf func(eco, key string, read func() ([]byte, error)) ([]string, error),
) ([]catalog.Entry, map[catalog.EntryKey]struct{}, error) {
	planned := append([]catalog.Entry(nil), selected...)
	included := make(map[catalog.EntryKey]struct{}, len(selected))
	for _, entry := range selected {
		included[entry.EntryKey] = struct{}{}
	}
	companions := make(map[catalog.EntryKey]struct{})
	if companionsOf == nil {
		return planned, companions, nil
	}
	for i := 0; i < len(planned); i++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entry := planned[i]
		digest := entry.Digest
		keys, err := companionsOf(entry.Eco, entry.Key, func() ([]byte, error) {
			return s.readSmall(digest)
		})
		if err != nil {
			return nil, nil, fmt.Errorf("transfer: what goes with %s %s: %w", entry.Eco, entry.Key, err)
		}
		for _, key := range keys {
			companionKey := catalog.EntryKey{Project: entry.Project, Eco: entry.Eco, Key: key}
			if _, already := included[companionKey]; already {
				continue
			}
			companion, err := s.Catalog.GetEntry(companionKey)
			if errors.Is(err, catalog.ErrNotFound) {
				// Not held here. The other project fetches it when it is first asked for.
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("transfer: %w", err)
			}
			if len(planned) >= maxCompanionEntries {
				return nil, nil, fmt.Errorf(
					"transfer: these packages name more than %d entries between them", maxCompanionEntries)
			}
			included[companionKey] = struct{}{}
			companions[companionKey] = struct{}{}
			planned = append(planned, companion)
		}
	}
	return planned, companions, nil
}

// readSmall returns a blob's bytes for an ecosystem that has to look inside a document to
// say what goes with it.
func (s *Service) readSmall(digest blob.Digest) ([]byte, error) {
	file, _, err := s.Blobs.Open(digest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxCompanionRead+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCompanionRead {
		return nil, fmt.Errorf("%s is larger than a document read to find companions", digest)
	}
	return body, nil
}

// projectQuota is the destination's limit, enforced by the same commit the engine uses.
func (s *Service) projectQuota(project string) catalog.Quota {
	if s.Config == nil {
		return catalog.Quota{}
	}
	settings := s.Config.Current().Projects[project]
	return catalog.Quota{Bytes: settings.QuotaBytes, Artifacts: settings.QuotaArtifacts}
}
