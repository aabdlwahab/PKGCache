package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

type harness struct {
	blobs *blob.Store
	cat   *catalog.DB
	now   time.Time
	svc   *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	blobs, err := blob.Open(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(root, "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	now := time.Now().UTC()
	return &harness{
		blobs: blobs, cat: cat, now: now,
		svc: &Service{Catalog: cat, Blobs: blobs, Now: func() time.Time { return now }},
	}
}

func (h *harness) put(t *testing.T, project, key, body string, access time.Time) blob.Digest {
	t.Helper()
	writer, err := h.blobs.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strings.NewReader(body).WriteTo(writer); err != nil {
		t.Fatal(err)
	}
	digest, size, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.CommitEntry(catalog.Entry{
		EntryKey: catalog.EntryKey{Project: project, Eco: "files", Key: key},
		Digest:   digest, Size: size, CachedAt: access, LastAccess: access,
	}, nil, catalog.Quota{}, false); err != nil {
		t.Fatal(err)
	}
	return digest
}

func age(t *testing.T, store *blob.Store, digest blob.Digest, at time.Time) {
	t.Helper()
	path, err := store.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestGCDeletedProjectReclaimsExclusiveAndPreservesShared(t *testing.T) {
	h := newHarness(t)
	old := h.now.Add(-3 * time.Hour)
	exclusive := h.put(t, "gone", "exclusive", "exclusive", old)
	shared := h.put(t, "gone", "shared-a", "shared", old)
	h.put(t, "kept", "shared-b", "shared", old)
	age(t, h.blobs, exclusive, old)
	age(t, h.blobs, shared, old)
	if _, err := h.cat.DeleteProject("gone"); err != nil {
		t.Fatal(err)
	}

	result, err := h.svc.GC(context.Background(), GCOptions{Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || h.blobs.Exists(exclusive) {
		t.Fatalf("result=%+v exclusive exists=%v", result, h.blobs.Exists(exclusive))
	}
	if !h.blobs.Exists(shared) {
		t.Fatal("shared blob was collected")
	}
}

func TestSnapshotPinsContentAcrossGCAndEviction(t *testing.T) {
	h := newHarness(t)
	old := h.now.Add(-24 * time.Hour)
	content := h.put(t, "p", "artifact", "pinned content", old)

	writer, err := h.blobs.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = snapshot.WriteManifest(writer, snapshot.Header{
		Project: "p", Created: old,
	}, func(yield func(snapshot.Entry) error) error {
		return yield(snapshot.Entry{
			Eco: "files", Key: "artifact", Digest: content, Size: 14,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, size, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cat.UpsertBlob(catalog.Blob{
		Digest: manifest, Size: size, CreatedAt: old, LastAccess: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.PutSnapshot(catalog.Snapshot{
		ID: string(manifest), Project: "p", Manifest: manifest,
		EntryCount: 1, TotalBytes: 14, CreatedAt: old, Subject: "pin",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.DeleteEntry(catalog.EntryKey{
		Project: "p", Eco: "files", Key: "artifact",
	}); err != nil {
		t.Fatal(err)
	}
	age(t, h.blobs, content, old)
	age(t, h.blobs, manifest, old)

	if _, err := h.svc.GC(context.Background(), GCOptions{Grace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !h.blobs.Exists(content) || !h.blobs.Exists(manifest) {
		t.Fatal("snapshot-pinned content was collected")
	}
}

func TestEvictionUsesLRUAndHoldsTarget(t *testing.T) {
	h := newHarness(t)
	oldest := h.put(t, "p", "a", "aaaa", h.now.Add(-3*time.Hour))
	h.put(t, "p", "b", "bbbb", h.now.Add(-2*time.Hour))
	h.put(t, "p", "c", "cccc", h.now.Add(-time.Hour))

	result, err := h.svc.Evict(context.Background(), EvictOptions{TargetBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.AfterBytes > 8 || result.EvictedEntries != 1 {
		t.Fatalf("result=%+v", result)
	}
	if h.blobs.Exists(oldest) {
		t.Fatal("oldest blob was not evicted")
	}
}

// Eviction must rank by last *read*, not by insertion order. The two orderings agree
// on a cache nobody has read from, which is why insert-time-only coverage passed while
// the hottest entry in a real cache was the first one evicted.
func TestEvictionRanksByLastReadNotInsertOrder(t *testing.T) {
	h := newHarness(t)
	oldestWrite := h.put(t, "p", "a", "aaaa", h.now.Add(-3*time.Hour))
	middle := h.put(t, "p", "b", "bbbb", h.now.Add(-2*time.Hour))
	h.put(t, "p", "c", "cccc", h.now.Add(-time.Hour))

	// "a" was written first but is being read constantly: it is the hottest entry.
	if err := h.cat.TouchEntries([]catalog.EntryTouch{{
		EntryKey:   catalog.EntryKey{Project: "p", Eco: "files", Key: "a"},
		Digest:     oldestWrite,
		Hits:       9,
		LastAccess: h.now,
	}}); err != nil {
		t.Fatal(err)
	}

	result, err := h.svc.Evict(context.Background(), EvictOptions{TargetBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.EvictedEntries != 1 {
		t.Fatalf("result=%+v", result)
	}
	if !h.blobs.Exists(oldestWrite) {
		t.Fatal("the most recently read entry was evicted; eviction is still insert-ordered")
	}
	if h.blobs.Exists(middle) {
		t.Fatal("the least recently read entry survived")
	}

	entry, err := h.cat.GetEntry(catalog.EntryKey{Project: "p", Eco: "files", Key: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Hits != 9 || !entry.LastAccess.Equal(h.now.Truncate(time.Second)) {
		t.Fatalf("touch did not persist: hits=%d last_access=%v", entry.Hits, entry.LastAccess)
	}
}

// A touch for a key that no longer exists must not resurrect it: the blob it pointed
// at may already have been collected.
func TestTouchDoesNotResurrectDeletedEntry(t *testing.T) {
	h := newHarness(t)
	key := catalog.EntryKey{Project: "p", Eco: "files", Key: "gone"}
	digest := h.put(t, "p", "gone", "gone", h.now)
	if err := h.cat.DeleteEntry(key); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.TouchEntries([]catalog.EntryTouch{{
		EntryKey: key, Digest: digest, Hits: 1, LastAccess: h.now,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.GetEntry(key); err == nil {
		t.Fatal("a touch recreated a deleted entry")
	}
}

func TestGCDryRunDoesNotMutate(t *testing.T) {
	h := newHarness(t)
	old := h.now.Add(-2 * time.Hour)
	digest := h.put(t, "p", "orphan", "orphan", old)
	if err := h.cat.DeleteEntry(catalog.EntryKey{
		Project: "p", Eco: "files", Key: "orphan",
	}); err != nil {
		t.Fatal(err)
	}
	age(t, h.blobs, digest, old)
	result, err := h.svc.GC(context.Background(), GCOptions{
		Grace: time.Hour, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates != 1 || !h.blobs.Exists(digest) {
		t.Fatalf("result=%+v exists=%v", result, h.blobs.Exists(digest))
	}
}
