package snapshot

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/race"
)

func TestCheckpoint32K119GBCatalogUnderTenSeconds(t *testing.T) {
	if race.Enabled {
		t.Skip(race.SkipReason)
	}
	const entries = 32_000
	root := t.TempDir()
	cat, err := catalog.Open(catalog.Options{
		Path: filepath.Join(root, "catalog.db"), BatchSize: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	digest := blob.Digest(fmt.Sprintf("%064x", 1))
	now := time.Now()
	if err := cat.UpsertBlob(catalog.Blob{
		Digest: digest, Size: 1, CreatedAt: now, LastAccess: now,
	}); err != nil {
		t.Fatal(err)
	}
	size := int64(119<<30) / entries
	for index := range entries {
		if err := cat.PutEntry(catalog.Entry{
			EntryKey: catalog.EntryKey{
				Project: "global", Eco: "files", Key: fmt.Sprintf("%06d.bin", index),
			},
			Digest: digest, Size: size,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.Flush(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var manifest bytes.Buffer
	count, _, err := WriteManifest(&manifest, Header{
		Project: "global", Created: now,
	}, func(yield func(Entry) error) error {
		return cat.WalkEntries("global", func(entry catalog.Entry) error {
			return yield(Entry{
				Eco: entry.Eco, Key: entry.Key, Digest: entry.Digest, Size: entry.Size,
			})
		})
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if count != entries {
		t.Fatalf("checkpointed %d entries", count)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("checkpoint took %s, want <10s", elapsed)
	}
	t.Logf("checkpointed 32k catalog rows representing 119 GiB in %s", elapsed)
}
