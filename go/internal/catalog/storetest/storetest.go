// Package storetest is the contract suite every catalog.Store implementation must
// pass.
//
// It exists because the interface, not the SQLite type, is what the rest of the
// codebase is written against. The existing catalog tests reach into *catalog.DB and so
// prove things about one implementation; these run through the interface only, which is
// what makes them a specification rather than a description. A second backend — the
// Postgres one the architecture leaves room for — becomes a matter of passing this
// suite instead of re-deriving the intended behaviour from the SQLite code.
package storetest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// Factory opens a fresh, empty store. The suite calls it once per case and is
// responsible for nothing else; registering cleanup is the factory's job.
type Factory func(t *testing.T) catalog.Store

// Run executes the whole contract against one implementation.
func Run(t *testing.T, open Factory) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, catalog.Store)
	}{
		{"EntryRoundTrip", entryRoundTrip},
		{"MissingEntryIsErrNotFound", missingEntry},
		{"CommitEntryEnforcesByteQuota", byteQuota},
		{"CommitEntryEnforcesArtifactQuota", artifactQuota},
		{"TouchAdvancesRecencyAndHits", touchAdvances},
		{"TouchWillNotResurrectADeletedEntry", touchNoResurrect},
		{"EvictionWalkIsLeastRecentlyUsedFirst", evictionOrder},
		{"DeleteProjectRemovesOnlyThatProject", deleteProject},
		{"RefFreshnessAndRevalidation", refs},
		{"ArtifactInventoryQuery", artifacts},
		{"SnapshotLineageAndHead", snapshots},
		{"StatsFoldDeltas", stats},
		{"InvalidDigestIsRejected", invalidDigest},
		{"FlushIsSynchronous", flushSynchronous},
		{"SeriesKeepsBucketAndOutcome", seriesDimensions},
		{"SeriesGroupingCollapsesDimensions", seriesGrouping},
		{"SeriesLeavesEmptyBucketsAbsent", seriesGaps},
		{"CompactionFoldsAndIsIdempotent", seriesCompaction},
		{"StorageSampleIsAGaugeNotACounter", storageSample},
		{"StorageTotalsSeparateLogicalFromStored", storageTotals},
		{"EntryAgesBucketByLastRead", entryAges},
		{"UpstreamSeriesDerivesMeanAndMax", upstreamSeries},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, open(t))
		})
	}
}

// ---- helpers ---------------------------------------------------------------

func digestOf(text string) blob.Digest {
	sum := sha256.Sum256([]byte(text))
	return blob.Digest(hex.EncodeToString(sum[:]))
}

func key(project, eco, name string) catalog.EntryKey {
	return catalog.EntryKey{Project: project, Eco: eco, Key: name}
}

// put writes one entry through the interface, with a blob row to reference.
func put(t *testing.T, store catalog.Store, k catalog.EntryKey, body string, at time.Time) blob.Digest {
	t.Helper()
	digest := digestOf(body)
	if err := store.UpsertBlob(catalog.Blob{
		Digest: digest, Size: int64(len(body)), CreatedAt: at, LastAccess: at,
	}); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	if err := store.CommitEntry(catalog.Entry{
		EntryKey: k, Digest: digest, Size: int64(len(body)),
		CachedAt: at, LastAccess: at,
	}, nil, catalog.Quota{}, false); err != nil {
		t.Fatalf("CommitEntry: %v", err)
	}
	return digest
}

// ---- cases -----------------------------------------------------------------

func entryRoundTrip(t *testing.T, store catalog.Store) {
	at := time.Now().Truncate(time.Second)
	k := key("global", "npm", "left-pad/-/left-pad-1.0.0.tgz")
	digest := put(t, store, k, "tarball", at)

	got, err := store.GetEntry(k)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.Digest != digest || got.Size != int64(len("tarball")) {
		t.Fatalf("entry = %+v", got)
	}
	if !got.LastAccess.Equal(at) {
		t.Fatalf("last access = %v, want %v", got.LastAccess, at)
	}

	// A second commit of the same key must replace, not duplicate.
	replaced := put(t, store, k, "different tarball", at)
	got, err = store.GetEntry(k)
	if err != nil || got.Digest != replaced {
		t.Fatalf("entry after replace = %+v, %v", got, err)
	}
	entries, err := store.ListEntries(catalog.EntryQuery{Project: "global", Eco: "npm"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("listed %d entries, want 1: %v", len(entries), err)
	}

	if err := store.DeleteEntry(k); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	if _, err := store.GetEntry(k); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

func missingEntry(t *testing.T, store catalog.Store) {
	if _, err := store.GetEntry(key("global", "npm", "absent")); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func byteQuota(t *testing.T, store catalog.Store) {
	at := time.Now()
	body := "0123456789"
	digest := digestOf(body)
	if err := store.UpsertBlob(catalog.Blob{
		Digest: digest, Size: int64(len(body)), CreatedAt: at, LastAccess: at,
	}); err != nil {
		t.Fatal(err)
	}
	err := store.CommitEntry(catalog.Entry{
		EntryKey: key("limited", "files", "big"), Digest: digest,
		Size: int64(len(body)), CachedAt: at, LastAccess: at,
	}, nil, catalog.Quota{Bytes: 4}, false)
	if !errors.Is(err, catalog.ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
	var quota *catalog.QuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("err %v does not carry usage a 507 can report", err)
	}
	if quota.Limit != 4 || quota.Attempt != int64(len(body)) {
		t.Fatalf("quota = %+v", quota)
	}
	// Nothing may be left behind: an over-quota commit is all-or-nothing.
	count, usage, err := store.CountEntries("limited")
	if err != nil || count != 0 || usage != 0 {
		t.Fatalf("count=%d usage=%d err=%v", count, usage, err)
	}
}

func artifactQuota(t *testing.T, store catalog.Store) {
	at := time.Now()
	commit := func(name string) error {
		body := "body-" + name
		digest := digestOf(body)
		if err := store.UpsertBlob(catalog.Blob{
			Digest: digest, Size: int64(len(body)), CreatedAt: at, LastAccess: at,
		}); err != nil {
			t.Fatal(err)
		}
		return store.CommitEntry(catalog.Entry{
			EntryKey: key("limited", "files", name), Digest: digest,
			Size: int64(len(body)), CachedAt: at, LastAccess: at,
		}, &catalog.Artifact{Name: name, Version: "1.0", CachedAt: at},
			catalog.Quota{Artifacts: 1}, false)
	}
	if err := commit("first"); err != nil {
		t.Fatalf("first artifact within quota: %v", err)
	}
	if err := commit("second"); !errors.Is(err, catalog.ErrQuota) {
		t.Fatalf("second artifact err = %v, want ErrQuota", err)
	}
}

func touchAdvances(t *testing.T, store catalog.Store) {
	early := time.Now().Add(-time.Hour).Truncate(time.Second)
	later := time.Now().Truncate(time.Second)
	k := key("global", "pypi", "numpy.whl")
	digest := put(t, store, k, "wheel", early)

	if err := store.TouchEntries([]catalog.EntryTouch{
		{EntryKey: k, Digest: digest, Hits: 3, LastAccess: later},
	}); err != nil {
		t.Fatalf("TouchEntries: %v", err)
	}
	got, err := store.GetEntry(k)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastAccess.Equal(later) {
		t.Fatalf("last access = %v, want %v", got.LastAccess, later)
	}
	if got.Hits != 3 {
		t.Fatalf("hits = %d, want 3", got.Hits)
	}
	// A touch must never move recency backwards.
	if err := store.TouchEntries([]catalog.EntryTouch{
		{EntryKey: k, Digest: digest, Hits: 1, LastAccess: early},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.GetEntry(k); err != nil || !got.LastAccess.Equal(later) {
		t.Fatalf("recency moved backwards: %v, %v", got.LastAccess, err)
	}
	if got.Hits != 4 {
		t.Fatalf("hits = %d, want 4", got.Hits)
	}
}

func touchNoResurrect(t *testing.T, store catalog.Store) {
	at := time.Now()
	k := key("global", "files", "gone")
	digest := put(t, store, k, "content", at)
	if err := store.DeleteEntry(k); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchEntries([]catalog.EntryTouch{
		{EntryKey: k, Digest: digest, Hits: 1, LastAccess: at},
	}); err != nil {
		t.Fatalf("TouchEntries on a deleted key: %v", err)
	}
	if _, err := store.GetEntry(k); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatal("a touch recreated a deleted entry")
	}
}

func evictionOrder(t *testing.T, store catalog.Store) {
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	oldest := key("p", "files", "oldest")
	middle := key("p", "files", "middle")
	newest := key("p", "files", "newest")
	put(t, store, oldest, "a", base)
	put(t, store, middle, "bb", base.Add(time.Hour))
	put(t, store, newest, "ccc", base.Add(2*time.Hour))

	// Reading the oldest-written entry makes it the most recently used one.
	if err := store.TouchEntries([]catalog.EntryTouch{{
		EntryKey: oldest, Digest: digestOf("a"), Hits: 1, LastAccess: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}

	var order []string
	if err := store.WalkEvictionCandidates("p", func(entry catalog.Entry) error {
		order = append(order, entry.Key)
		return nil
	}); err != nil {
		t.Fatalf("WalkEvictionCandidates: %v", err)
	}
	want := []string{"middle", "newest", "oldest"}
	if len(order) != len(want) {
		t.Fatalf("walked %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("walked %v, want %v (least recently used first)", order, want)
		}
	}
}

func deleteProject(t *testing.T, store catalog.Store) {
	at := time.Now()
	put(t, store, key("keep", "files", "a"), "shared", at)
	put(t, store, key("drop", "files", "a"), "shared", at)
	put(t, store, key("drop", "files", "b"), "exclusive", at)

	removed, err := store.DeleteProject("drop")
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d entries, want 2", removed)
	}
	if _, err := store.GetEntry(key("keep", "files", "a")); err != nil {
		t.Fatalf("the other project's entry was removed: %v", err)
	}
	// Content shared with a surviving project must stay referenced.
	referenced, err := store.IsBlobReferenced(digestOf("shared"))
	if err != nil || !referenced {
		t.Fatalf("shared blob referenced = %v, %v", referenced, err)
	}
	referenced, err = store.IsBlobReferenced(digestOf("exclusive"))
	if err != nil || referenced {
		t.Fatalf("exclusive blob still referenced = %v, %v", referenced, err)
	}
}

func refs(t *testing.T, store catalog.Store) {
	now := time.Now()
	k := catalog.RefKey{Project: "global", Eco: "oci", Name: "tag/dockerhub/alpine/3.20"}
	ref := catalog.Ref{
		RefKey: k, Target: "manifest/" + digestOf("manifest").String(),
		ETag: `"abc"`, FetchedAt: now, TTL: time.Minute,
	}
	if err := store.PutRef(ref); err != nil {
		t.Fatalf("PutRef: %v", err)
	}
	got, err := store.GetRef(k)
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if got.Target != ref.Target || got.ETag != ref.ETag {
		t.Fatalf("ref = %+v", got)
	}
	if !got.Fresh(now.Add(30 * time.Second)) {
		t.Fatal("ref within its TTL reported stale")
	}
	if got.Fresh(now.Add(2 * time.Minute)) {
		t.Fatal("ref past its TTL reported fresh")
	}

	listed, err := store.ListRefs("global", "oci", "tag/dockerhub/")
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %d refs: %v", len(listed), err)
	}
	if other, err := store.ListRefs("global", "oci", "tag/ghcr/"); err != nil || len(other) != 0 {
		t.Fatalf("prefix filter leaked %d refs: %v", len(other), err)
	}
	if err := store.DeleteRef(k); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRef(k); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("deleted ref err = %v", err)
	}
}

func artifacts(t *testing.T, store catalog.Store) {
	at := time.Now()
	for _, spec := range []struct{ name, version string }{
		{"numpy", "1.26.0"}, {"numpy", "2.0.0"}, {"torch", "2.3.0"},
	} {
		if err := store.PutArtifact(catalog.Artifact{
			Project: "global", Eco: "pypi", Name: spec.name, Version: spec.version,
			Digest: digestOf(spec.name + spec.version), Size: 10, CachedAt: at,
		}); err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}
	}
	found, total, err := store.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: "pypi", Search: "num",
	})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if total != 2 || len(found) != 2 {
		t.Fatalf("search found %d/%d, want 2", len(found), total)
	}
	if err := store.DeleteArtifactVersion("global", "pypi", "numpy", "1.26.0"); err != nil {
		t.Fatal(err)
	}
	if _, total, err = store.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: "pypi", Search: "num",
	}); err != nil || total != 1 {
		t.Fatalf("after version delete total = %d: %v", total, err)
	}
	if err := store.DeleteArtifacts("global", "pypi", "numpy"); err != nil {
		t.Fatal(err)
	}
	if _, total, err = store.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: "pypi", Search: "num",
	}); err != nil || total != 0 {
		t.Fatalf("after name delete total = %d: %v", total, err)
	}
}

func snapshots(t *testing.T, store catalog.Store) {
	at := time.Now().Truncate(time.Second)
	manifest := digestOf("manifest")
	if err := store.UpsertBlob(catalog.Blob{
		Digest: manifest, Size: 5, CreatedAt: at, LastAccess: at,
	}); err != nil {
		t.Fatal(err)
	}
	first := catalog.Snapshot{
		ID: manifest.String(), Project: "global", Manifest: manifest,
		EntryCount: 1, TotalBytes: 5, CreatedAt: at, Subject: "first",
	}
	if err := store.CommitSnapshot(first); err != nil {
		t.Fatalf("CommitSnapshot: %v", err)
	}
	head, err := store.GetHead("global")
	if err != nil || head != first.ID {
		t.Fatalf("head = %q, %v", head, err)
	}
	got, err := store.GetSnapshot(first.ID)
	if err != nil || got.Subject != "first" {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	listed, err := store.ListSnapshots("global", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %d snapshots: %v", len(listed), err)
	}
	var walked int
	if err := store.WalkSnapshots(func(catalog.Snapshot) error {
		walked++
		return nil
	}); err != nil || walked != 1 {
		t.Fatalf("walked %d snapshots: %v", walked, err)
	}
	if _, err := store.GetSnapshot("nope"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("missing snapshot err = %v", err)
	}
}

func stats(t *testing.T, store catalog.Store) {
	at := time.Now()
	put(t, store, key("global", "npm", "a"), "content", at)
	if err := store.RecordAccess(
		[]catalog.AccessDelta{
			{Project: "global", Eco: "npm", Name: "left-pad", Count: 2, LastAccess: at},
			{Project: "global", Eco: "npm", Name: "left-pad", Count: 3, LastAccess: at},
		},
		[]catalog.TrafficDelta{
			{Project: "global", Eco: "npm", HitCount: 1, HitBytes: 10, MissCount: 2, MissBytes: 20},
		},
	); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	result, err := store.Stats(catalog.StatsQuery{Project: "global"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	var requests int64
	for _, row := range result.ByEco {
		if row.Eco == "npm" {
			requests = row.Requests
			if row.HitBytes != 10 || row.MissBytes != 20 {
				t.Fatalf("traffic = %+v", row)
			}
		}
	}
	// Deltas fold in: two windows for one name add up rather than overwrite.
	if requests != 5 {
		t.Fatalf("requests = %d, want 5", requests)
	}
}

func invalidDigest(t *testing.T, store catalog.Store) {
	at := time.Now()
	err := store.CommitEntry(catalog.Entry{
		EntryKey: key("global", "files", "bad"),
		Digest:   blob.Digest("../../etc/passwd"),
		Size:     1, CachedAt: at, LastAccess: at,
	}, nil, catalog.Quota{}, false)
	if err == nil {
		t.Fatal("a malformed digest was accepted")
	}
}

func flushSynchronous(t *testing.T, store catalog.Store) {
	at := time.Now()
	k := key("global", "files", "queued")
	digest := digestOf("queued")
	if err := store.UpsertBlob(catalog.Blob{
		Digest: digest, Size: 6, CreatedAt: at, LastAccess: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEntry(catalog.Entry{
		EntryKey: k, Digest: digest, Size: 6, CachedAt: at, LastAccess: at,
	}); err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// After Flush the row must be visible to a scan, not only to the read-through
	// cache: checkpoint and GC both read through the walk.
	var seen bool
	if err := store.WalkEntries("global", func(entry catalog.Entry) error {
		if entry.Key == k.Key {
			seen = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("a flushed entry was not visible to WalkEntries")
	}
}
