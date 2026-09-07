package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// A bar has to reach the end and it has to have moved on the way. Both are asserted,
// because the two failures look identical from the outside — a bar that never fills and
// one that jumps from nothing to done are both "the progress bar is broken".
func TestExportAndImportReportProgress(t *testing.T) {
	store, cat := testStorage(t)
	created := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	var entries []Entry
	const blobs = 250
	for i := 0; i < blobs; i++ {
		digest := testBlob(t, store, cat, fmt.Sprintf("blob-%03d", i))
		entries = append(entries, Entry{
			Eco: "pypi", Key: fmt.Sprintf("k%03d", i), Digest: digest, Size: 8,
		})
	}
	target := testSnapshot(t, store, cat, "global", "", created, entries)

	var written [][2]int64
	var transfer bytes.Buffer
	pack, err := WritePack(context.Background(), &transfer, cat, store, ExportOptions{
		Project: "global", Target: target.ID,
		Progress: func(done, total int64) { written = append(written, [2]int64{done, total}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProgress(t, "export", written, pack.Blobs)

	destStore, destCatalog := testStorage(t)
	var read [][2]int64
	if _, err := ReadPack(context.Background(), bytes.NewReader(transfer.Bytes()),
		destCatalog, destStore, ImportOptions{
			Project:  "global",
			Progress: func(done, total int64) { read = append(read, [2]int64{done, total}) },
		}); err != nil {
		t.Fatal(err)
	}
	assertProgress(t, "import", read, pack.Blobs)
}

func assertProgress(t *testing.T, what string, frames [][2]int64, blobs int64) {
	t.Helper()
	if len(frames) < 2 {
		t.Fatalf("%s reported %d frame(s); a bar needs more than an ending", what, len(frames))
	}
	// Bounded at about a hundred whatever the size, so a large pack cannot flood the bus.
	if len(frames) > 120 {
		t.Errorf("%s reported %d frames for %d blobs; the step is not proportional",
			what, len(frames), blobs)
	}
	last := frames[len(frames)-1]
	if last[0] != blobs || last[1] != blobs {
		t.Errorf("%s ended at %d of %d, want %d of %d", what, last[0], last[1], blobs, blobs)
	}
	previous := int64(0)
	for _, frame := range frames {
		if frame[1] != blobs {
			t.Fatalf("%s reported a total of %d, want %d — the denominator must not move",
				what, frame[1], blobs)
		}
		if frame[0] <= previous {
			t.Fatalf("%s went from %d to %d; progress must only move forward",
				what, previous, frame[0])
		}
		previous = frame[0]
	}
}
