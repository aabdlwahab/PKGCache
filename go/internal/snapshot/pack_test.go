package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
)

func TestDeltaPackRoundTripAndExactApply(t *testing.T) {
	sourceStore, sourceCatalog := testStorage(t)
	a := testBlob(t, sourceStore, sourceCatalog, "alpha")
	b := testBlob(t, sourceStore, sourceCatalog, "bravo")
	c := testBlob(t, sourceStore, sourceCatalog, "charlie")
	created := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	base := testSnapshot(t, sourceStore, sourceCatalog, "global", "", created, []Entry{
		{Eco: "npm", Key: "a", Digest: a, Size: 5},
		{Eco: "pypi", Key: "b", Digest: b, Size: 5},
	})
	target := testSnapshot(t, sourceStore, sourceCatalog, "global", base.ID,
		created.Add(time.Minute), []Entry{
			{Eco: "pypi", Key: "b", Digest: b, Size: 5},
			{Eco: "pypi", Key: "c", Digest: c, Size: 7},
		})

	var transfer bytes.Buffer
	pack, err := WritePack(context.Background(), &transfer, sourceCatalog, sourceStore,
		ExportOptions{Project: "global", Base: base.ID, Target: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Blobs != 1 || pack.Bytes != 7 {
		t.Fatalf("delta = %d blobs/%d bytes, want 1/7", pack.Blobs, pack.Bytes)
	}

	destStore, destCatalog := testStorage(t)
	testCopyBlob(t, sourceStore, destStore, destCatalog, a)
	testCopyBlob(t, sourceStore, destStore, destCatalog, b)
	testCopyBlob(t, sourceStore, destStore, destCatalog, base.Manifest)
	if err := destCatalog.CommitSnapshot(base.catalog()); err != nil {
		t.Fatal(err)
	}
	imported, err := ReadPack(context.Background(), bytes.NewReader(transfer.Bytes()),
		destCatalog, destStore, ImportOptions{Project: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Target != target.ID {
		t.Fatalf("target = %s", imported.Target)
	}
	entries, err := destCatalog.ListEntries(catalog.EntryQuery{Project: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Key != "b" || entries[1].Key != "c" {
		t.Fatalf("applied entries = %+v", entries)
	}
	if head, _ := destCatalog.GetHead("global"); head != target.ID {
		t.Fatalf("HEAD = %s", head)
	}

	// Host B can roll back because the base's blobs were already present before the
	// delta arrived. The restored mapping is byte-for-byte the original base tree.
	baseFile, _, err := destStore.Open(base.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	baseIterator, err := NewIterator(baseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := destCatalog.ApplySnapshot("global", base.ID,
		func(yield func(catalog.Entry) error) error {
			_, _, err := baseIterator.Walk(func(entry Entry) error {
				return yield(catalog.Entry{
					EntryKey: catalog.EntryKey{
						Project: "global", Eco: entry.Eco, Key: entry.Key,
					},
					Digest: entry.Digest, Size: entry.Size,
				})
			})
			return err
		}); err != nil {
		t.Fatal(err)
	}
	_ = baseIterator.Close()
	_ = baseFile.Close()
	restored, err := destCatalog.ListEntries(catalog.EntryQuery{Project: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 || restored[0].Digest != a || restored[1].Digest != b {
		t.Fatalf("rolled-back tree = %+v", restored)
	}
}

func TestFullPackRejectsCorruptionAndNonFastForward(t *testing.T) {
	store, cat := testStorage(t)
	digest := testBlob(t, store, cat, "payload")
	meta := testSnapshot(t, store, cat, "global", "",
		time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC), []Entry{
			{Eco: "files", Key: "payload", Digest: digest, Size: 7},
		})
	var good bytes.Buffer
	if _, err := WritePack(context.Background(), &good, cat, store,
		ExportOptions{Project: "global", Target: meta.ID}); err != nil {
		t.Fatal(err)
	}
	corrupt := corruptFirstBlob(t, good.Bytes())
	destStore, destCatalog := testStorage(t)
	if _, err := ReadPack(context.Background(), bytes.NewReader(corrupt),
		destCatalog, destStore, ImportOptions{Project: "global"}); err == nil ||
		!strings.Contains(err.Error(), "corrupt blob") {
		t.Fatalf("corrupt import error = %v", err)
	}
	if destStore.Exists(digest) {
		t.Fatal("corrupt bytes were published under the expected digest")
	}

	otherStore, otherCatalog := testStorage(t)
	if err := otherCatalog.SetHead("global", strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPack(context.Background(), bytes.NewReader(good.Bytes()),
		otherCatalog, otherStore, ImportOptions{Project: "global"}); err == nil ||
		!strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("non-fast-forward error = %v", err)
	}
}

func testStorage(t *testing.T) (*blob.Store, *catalog.DB) {
	t.Helper()
	root := t.TempDir()
	store, err := blob.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(root, "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return store, cat
}

func testBlob(
	t *testing.T, store *blob.Store, cat *catalog.DB, content string,
) blob.Digest {
	t.Helper()
	writer, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
	digest, size, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := cat.UpsertBlob(catalog.Blob{
		Digest: digest, Size: size, CreatedAt: now, LastAccess: now,
	}); err != nil {
		t.Fatal(err)
	}
	return digest
}

func testSnapshot(
	t *testing.T,
	store *blob.Store,
	cat *catalog.DB,
	project, parent string,
	created time.Time,
	entries []Entry,
) Meta {
	t.Helper()
	writer, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	count, bytes, err := WriteManifest(writer, Header{
		Project: project, Created: created,
	}, func(yield func(Entry) error) error {
		for _, entry := range entries {
			if err := yield(entry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, manifestSize, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertBlob(catalog.Blob{
		Digest: digest, Size: manifestSize, CreatedAt: created, LastAccess: created,
	}); err != nil {
		t.Fatal(err)
	}
	meta := Meta{
		ID: string(digest), Project: project, Parent: parent, Manifest: digest,
		EntryCount: count, TotalBytes: bytes, CreatedAt: created, Subject: "test",
	}
	if err := cat.CommitSnapshot(meta.catalog()); err != nil {
		t.Fatal(err)
	}
	return meta
}

func testCopyBlob(
	t *testing.T,
	source, dest *blob.Store,
	cat *catalog.DB,
	digest blob.Digest,
) {
	t.Helper()
	file, _, err := source.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer, err := dest.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(writer, file); err != nil {
		t.Fatal(err)
	}
	got, size, err := writer.Commit()
	if err != nil || got != digest {
		t.Fatalf("copy = %s/%v", got, err)
	}
	now := time.Now()
	if err := cat.UpsertBlob(catalog.Blob{
		Digest: digest, Size: size, CreatedAt: now, LastAccess: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func corruptFirstBlob(t *testing.T, source []byte) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(source))
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	corrupted := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !corrupted && strings.HasPrefix(header.Name, "blobs/") {
			body[0] ^= 0xff
			corrupted = true
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !corrupted {
		t.Fatal("pack had no blob to corrupt")
	}
	return out.Bytes()
}
