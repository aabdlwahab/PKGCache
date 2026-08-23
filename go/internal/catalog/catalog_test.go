package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

func newDB(t testing.TB) *DB {
	t.Helper()
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func dg(s string) blob.Digest {
	h := sha256.Sum256([]byte(s))
	return blob.Digest(hex.EncodeToString(h[:]))
}

func entry(project, eco, key, content string) Entry {
	return Entry{
		EntryKey:  EntryKey{Project: project, Eco: eco, Key: key},
		Digest:    dg(content),
		Size:      int64(len(content)),
		MediaType: "application/octet-stream",
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	for i := range 3 {
		db, err := Open(Options{Path: path})
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

// The batch queue must not be observable: a read immediately after a write has to
// see it, or a second request for a just-cached artifact would re-fetch it.
func TestPutEntryIsImmediatelyReadable(t *testing.T) {
	db := newDB(t)
	e := entry("global", "pypi", "root/pypi/+f/numpy/numpy-2.0.whl", "wheel bytes")
	if err := db.PutEntry(e); err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if db.Pending() == 0 {
		t.Fatal("expected the write to be queued, not synchronous")
	}
	got, err := db.GetEntry(e.EntryKey)
	if err != nil {
		t.Fatalf("GetEntry before flush: %v", err)
	}
	if got.Digest != e.Digest || got.Size != e.Size {
		t.Fatalf("got %+v, want digest %s size %d", got, e.Digest, e.Size)
	}
}

func TestPutEntrySurvivesFlush(t *testing.T) {
	db := newDB(t)
	e := entry("global", "npm", "chalk/-/chalk-5.3.0.tgz", "tarball")
	if err := db.PutEntry(e); err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if db.Pending() != 0 {
		t.Fatalf("Pending = %d after Flush", db.Pending())
	}
	// Defeat the LRU so the row genuinely comes back off disk.
	db.mu.Lock()
	db.cache = newEntryCache(4096)
	db.mu.Unlock()

	got, err := db.GetEntry(e.EntryKey)
	if err != nil {
		t.Fatalf("GetEntry after flush: %v", err)
	}
	if got.Digest != e.Digest {
		t.Fatalf("digest = %s, want %s", got.Digest, e.Digest)
	}
	if got.CachedAt.IsZero() {
		t.Fatal("CachedAt was not defaulted")
	}
}

func TestGetEntryMissing(t *testing.T) {
	db := newDB(t)
	_, err := db.GetEntry(EntryKey{Project: "global", Eco: "npm", Key: "nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Universal dedup: the same bytes cached by two projects is one blob row.
func TestCrossProjectDedup(t *testing.T) {
	db := newDB(t)
	const body = "shared torch wheel"
	for _, p := range []string{"global", "team-a", "team-b"} {
		if err := db.PutEntry(entry(p, "pypi", "root/pypi/+f/torch/torch.whl", body)); err != nil {
			t.Fatalf("PutEntry %s: %v", p, err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var blobs int
	if err := db.WalkBlobs(func(Blob) error { blobs++; return nil }); err != nil {
		t.Fatalf("WalkBlobs: %v", err)
	}
	if blobs != 1 {
		t.Fatalf("blob rows = %d, want 1 (three projects, identical bytes)", blobs)
	}
	for _, p := range []string{"global", "team-a", "team-b"} {
		if _, _, err := db.CountEntries(p); err != nil {
			t.Fatalf("CountEntries %s: %v", p, err)
		}
	}
}

func TestCommitEntryQuotaSerializesConcurrentWriters(t *testing.T) {
	db := newDB(t)
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("body-%02d", i)
			errs <- db.CommitEntry(entry("limited", "files", body, body), nil,
				Quota{Bytes: 10}, false)
		}()
	}
	wg.Wait()
	close(errs)
	var accepted, rejected int
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQuota):
			rejected++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	count, bytes, err := db.CountEntries("limited")
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || rejected != writers-1 || count != 1 || bytes != 7 {
		t.Fatalf("accepted=%d rejected=%d count=%d bytes=%d",
			accepted, rejected, count, bytes)
	}
}

// Deleting a project must make its exclusive bytes collectable — the thing the
// previous path-first design could not do.
func TestDeleteProjectFreesExclusiveBlobs(t *testing.T) {
	db := newDB(t)
	shared := entry("team-a", "npm", "shared.tgz", "shared bytes")
	sharedB := entry("global", "npm", "shared.tgz", "shared bytes")
	only := entry("team-a", "npm", "private.tgz", "team-a only")

	for _, e := range []Entry{shared, sharedB, only} {
		if err := db.PutEntry(e); err != nil {
			t.Fatalf("PutEntry: %v", err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	n, err := db.DeleteProject("team-a")
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d entries, want 2", n)
	}

	unref, err := db.UnreferencedBlobs(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("UnreferencedBlobs: %v", err)
	}
	if len(unref) != 1 || unref[0] != only.Digest {
		t.Fatalf("unreferenced = %v, want exactly the team-a-only blob %s", unref, only.Digest)
	}
}

// The grace period exists because a blob is committed before its entry row is
// written: a brand-new blob may belong to a fetch that is still in flight.
func TestUnreferencedBlobsRespectsGracePeriod(t *testing.T) {
	db := newDB(t)
	d := dg("just committed, entry not yet written")
	if err := db.UpsertBlob(Blob{Digest: d, Size: 10}); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	got, err := db.UnreferencedBlobs(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("UnreferencedBlobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("collected a blob inside the grace period: %v", got)
	}
}

func TestRefFreshnessAndRoundTrip(t *testing.T) {
	db := newDB(t)
	r := Ref{
		RefKey:    RefKey{Project: "global", Eco: "oci", Name: "dockerhub/library/alpine:3.20"},
		Target:    dg("manifest").Prefixed(),
		MediaType: "application/vnd.oci.image.index.v1+json",
		ETag:      `W/"abc"`,
		FetchedAt: time.Now().Add(-30 * time.Second),
		TTL:       60 * time.Second,
	}
	if err := db.PutRef(r); err != nil {
		t.Fatalf("PutRef: %v", err)
	}
	got, err := db.GetRef(r.RefKey)
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if got.Target != r.Target || got.ETag != r.ETag || got.TTL != r.TTL {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.Fresh(time.Now()) {
		t.Fatal("ref should still be fresh 30s into a 60s TTL")
	}
	if got.Fresh(time.Now().Add(2 * time.Minute)) {
		t.Fatal("ref should be stale past its TTL")
	}

	// TTL 0 means "always revalidate", never "fresh forever".
	zero := Ref{RefKey: RefKey{Project: "global", Eco: "npm", Name: "chalk"}, Target: "x"}
	if err := db.PutRef(zero); err != nil {
		t.Fatalf("PutRef zero TTL: %v", err)
	}
	if z, _ := db.GetRef(zero.RefKey); z.Fresh(time.Now()) {
		t.Fatal("a zero TTL must never read as fresh")
	}
}

func TestListRefsPrefix(t *testing.T) {
	db := newDB(t)
	for _, n := range []string{"github.com/a/b:main", "github.com/a/b:v1", "gitlab.com/x/y:main"} {
		if err := db.PutRef(Ref{RefKey: RefKey{Project: "global", Eco: "git", Name: n}, Target: "sha"}); err != nil {
			t.Fatalf("PutRef: %v", err)
		}
	}
	got, err := db.ListRefs("global", "git", "github.com/")
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2", len(got))
	}
}

func TestListEntriesPrefixIsLiteral(t *testing.T) {
	db := newDB(t)
	for _, k := range []string{"builds/v1/app.tar.gz", "builds/v2/app.tar.gz", "other/x"} {
		if err := db.PutEntry(entry("global", "files", k, k)); err != nil {
			t.Fatalf("PutEntry: %v", err)
		}
	}
	// A GLOB metacharacter in the prefix must match literally, not as a wildcard.
	if err := db.PutEntry(entry("global", "files", "wild[card]/x", "w")); err != nil {
		t.Fatalf("PutEntry: %v", err)
	}

	got, err := db.ListEntries(EntryQuery{Project: "global", Eco: "files", Prefix: "builds/"})
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Key > got[1].Key {
		t.Fatal("entries must be key-ordered")
	}

	lit, err := db.ListEntries(EntryQuery{Project: "global", Eco: "files", Prefix: "wild[card]"})
	if err != nil {
		t.Fatalf("ListEntries literal: %v", err)
	}
	if len(lit) != 1 {
		t.Fatalf("GLOB metacharacters leaked: got %d, want 1", len(lit))
	}
}

func TestArtifactsQueryAndSearchEscaping(t *testing.T) {
	db := newDB(t)
	seed := []Artifact{
		{Project: "global", Eco: "pypi", Name: "numpy", Version: "2.0.0", Size: 18 << 20},
		{Project: "global", Eco: "pypi", Name: "torch", Version: "2.6.0", Size: 2500 << 20},
		{Project: "global", Eco: "npm", Name: "chalk", Version: "5.3.0", Size: 20 << 10},
		{Project: "team-a", Eco: "npm", Name: "100%_odd", Version: "1.0.0", Size: 1},
	}
	for _, a := range seed {
		a.Digest = dg(a.Name)
		if err := db.PutArtifact(a); err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}
	}

	// Cross-project, cross-ecosystem — the query the sharded design could not express.
	all, total, err := db.QueryArtifacts(ArtifactQuery{Sort: "size"})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if all[0].Name != "torch" {
		t.Fatalf("size sort put %q first", all[0].Name)
	}

	scoped, total, err := db.QueryArtifacts(ArtifactQuery{Project: "global", Eco: "pypi"})
	if err != nil {
		t.Fatalf("QueryArtifacts scoped: %v", err)
	}
	if total != 2 || len(scoped) != 2 {
		t.Fatalf("scoped total = %d, len = %d, want 2/2", total, len(scoped))
	}

	// "%" in a search term must be a literal, not a wildcard.
	esc, total, err := db.QueryArtifacts(ArtifactQuery{Search: "100%_"})
	if err != nil {
		t.Fatalf("QueryArtifacts search: %v", err)
	}
	if total != 1 || esc[0].Name != "100%_odd" {
		t.Fatalf("LIKE escaping failed: total=%d %+v", total, esc)
	}

	paged, total, err := db.QueryArtifacts(ArtifactQuery{Sort: "name", Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("QueryArtifacts paged: %v", err)
	}
	if total != 4 || len(paged) != 2 {
		t.Fatalf("page 2 of 2: total=%d len=%d", total, len(paged))
	}
}

func TestArtifactExtraRoundTrip(t *testing.T) {
	db := newDB(t)
	a := Artifact{
		Project: "global", Eco: "npm", Name: "chalk", Version: "5.3.0",
		Digest: dg("chalk"), Extra: map[string]any{"integrity": "sha512-abc"},
	}
	if err := db.PutArtifact(a); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	got, _, err := db.QueryArtifacts(ArtifactQuery{Project: "global", Eco: "npm"})
	if err != nil {
		t.Fatalf("QueryArtifacts: %v", err)
	}
	if got[0].Extra["integrity"] != "sha512-abc" {
		t.Fatalf("extra = %v", got[0].Extra)
	}
}

func TestStatsAggregation(t *testing.T) {
	db := newDB(t)
	for _, a := range []Artifact{
		{Project: "global", Eco: "pypi", Name: "numpy", Version: "2.0", Size: 100},
		{Project: "global", Eco: "pypi", Name: "torch", Version: "2.6", Size: 900},
		{Project: "global", Eco: "npm", Name: "chalk", Version: "5.3", Size: 50},
	} {
		a.Digest = dg(a.Name)
		if err := db.PutArtifact(a); err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}
	}
	err := db.RecordAccess(
		[]AccessDelta{
			{Project: "global", Eco: "pypi", Name: "numpy", Count: 7, LastAccess: time.Now()},
			{Project: "global", Eco: "npm", Name: "chalk", Count: 3, LastAccess: time.Now()},
		},
		[]TrafficDelta{
			{Project: "global", Eco: "pypi", HitCount: 10, HitBytes: 1000, MissCount: 2, MissBytes: 500},
		})
	if err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	// Deltas accumulate rather than overwrite.
	if err := db.RecordAccess([]AccessDelta{
		{Project: "global", Eco: "pypi", Name: "numpy", Count: 5, LastAccess: time.Now()},
	}, nil); err != nil {
		t.Fatalf("RecordAccess second window: %v", err)
	}

	got, err := db.Stats(StatsQuery{Project: "global"})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	byEco := map[string]EcoStats{}
	for _, s := range got.ByEco {
		byEco[s.Eco] = s
	}
	if byEco["pypi"].Count != 2 || byEco["pypi"].Size != 1000 {
		t.Fatalf("pypi inventory = %+v", byEco["pypi"])
	}
	if byEco["pypi"].HitCount != 10 || byEco["pypi"].HitBytes != 1000 {
		t.Fatalf("pypi traffic = %+v", byEco["pypi"])
	}
	if byEco["pypi"].Requests != 12 {
		t.Fatalf("pypi requests = %d, want 12 (7+5 accumulated)", byEco["pypi"].Requests)
	}
	if len(got.Leaderboard) == 0 || got.Leaderboard[0].Name != "numpy" {
		t.Fatalf("leaderboard = %+v", got.Leaderboard)
	}
	if len(got.TopLargest) == 0 || got.TopLargest[0].Name != "torch" {
		t.Fatalf("top largest = %+v", got.TopLargest)
	}
}

func TestSnapshotsAndHead(t *testing.T) {
	db := newDB(t)
	first := Snapshot{
		ID: "s1", Project: "global", Manifest: dg("m1"),
		EntryCount: 10, TotalBytes: 1000, Subject: "initial",
		CreatedAt: time.Now().Add(-time.Hour),
	}
	second := Snapshot{
		ID: "s2", Project: "global", Parent: "s1", Manifest: dg("m2"),
		EntryCount: 20, TotalBytes: 2000, Subject: "second",
		CreatedAt: time.Now(),
	}
	for _, s := range []Snapshot{first, second} {
		if err := db.PutSnapshot(s); err != nil {
			t.Fatalf("PutSnapshot %s: %v", s.ID, err)
		}
	}
	list, err := db.ListSnapshots("global", 10)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 2 || list[0].ID != "s2" {
		t.Fatalf("snapshots not newest-first: %+v", list)
	}
	if list[0].Parent != "s1" {
		t.Fatalf("parent pointer lost: %+v", list[0])
	}

	if head, _ := db.GetHead("global"); head != "" {
		t.Fatalf("head = %q before any SetHead", head)
	}
	if err := db.SetHead("global", "s2"); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	if head, _ := db.GetHead("global"); head != "s2" {
		t.Fatalf("head = %q, want s2", head)
	}

	// A snapshot manifest is itself a blob and must pin it against collection.
	if err := db.UpsertBlob(Blob{Digest: dg("m2"), Size: 42, CreatedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	unref, err := db.UnreferencedBlobs(time.Now())
	if err != nil {
		t.Fatalf("UnreferencedBlobs: %v", err)
	}
	for _, d := range unref {
		if d == dg("m2") {
			t.Fatal("a snapshot manifest blob was reported collectable")
		}
	}
}

func TestInvalidDigestRejected(t *testing.T) {
	db := newDB(t)
	if err := db.PutEntry(Entry{
		EntryKey: EntryKey{Project: "p", Eco: "npm", Key: "k"},
		Digest:   blob.Digest("../../etc/passwd"),
	}); err == nil {
		t.Fatal("PutEntry accepted an invalid digest")
	}
	if err := db.UpsertBlob(Blob{Digest: blob.Digest("nope")}); err == nil {
		t.Fatal("UpsertBlob accepted an invalid digest")
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	db := newDB(t)
	const writers, perWriter = 8, 50

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				k := fmt.Sprintf("pkg-%d-%d", w, i)
				if err := db.PutEntry(entry("global", "npm", k, k)); err != nil {
					t.Errorf("PutEntry: %v", err)
					return
				}
				if _, err := db.GetEntry(EntryKey{Project: "global", Eco: "npm", Key: k}); err != nil {
					t.Errorf("read-your-write failed for %s: %v", k, err)
					return
				}
			}
		}()
	}
	// Readers run concurrently against the same file — WAL makes this real parallelism.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := db.Stats(StatsQuery{Project: "global"}); err != nil {
					t.Errorf("Stats: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := db.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	count, _, err := db.CountEntries("global")
	if err != nil {
		t.Fatalf("CountEntries: %v", err)
	}
	if count != writers*perWriter {
		t.Fatalf("entries = %d, want %d", count, writers*perWriter)
	}
}

func TestCloseFlushesPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := Open(Options{Path: path, BatchInterval: time.Hour}) // never auto-flushes
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := entry("global", "files", "a.txt", "content")
	if err := db.PutEntry(e); err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.GetEntry(e.EntryKey); err != nil {
		t.Fatalf("Close did not flush the queue: %v", err)
	}
}

func TestPutAfterCloseFails(t *testing.T) {
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.PutEntry(entry("p", "npm", "k", "v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestEntryCacheLRU(t *testing.T) {
	c := newEntryCache(2)
	a := entry("p", "npm", "a", "a")
	b := entry("p", "npm", "b", "b")
	d := entry("p", "npm", "c", "c")

	c.put(a)
	c.put(b)
	if _, ok := c.get(a.EntryKey); !ok { // refresh a so b becomes the oldest
		t.Fatal("a should be cached")
	}
	c.put(d)
	if c.len() != 2 {
		t.Fatalf("len = %d, want 2", c.len())
	}
	if _, ok := c.get(b.EntryKey); ok {
		t.Fatal("b should have been evicted as least-recently-used")
	}
	if _, ok := c.get(a.EntryKey); !ok {
		t.Fatal("a should have survived")
	}

	c.dropProject("p")
	if c.len() != 0 {
		t.Fatalf("dropProject left %d entries", c.len())
	}
}
