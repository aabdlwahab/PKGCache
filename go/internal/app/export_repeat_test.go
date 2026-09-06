package app

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// waitJobSettled is waitJob for a job that is expected to fail: waitJob calls t.Fatal on
// a failure, which is right everywhere else and useless when the failure is the assertion.
func waitJobSettled(t *testing.T, instance *App, id int64) control.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		record, err := instance.Jobs.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		switch record.Status {
		case "done", "failed", "cancelled":
			return record
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %d timed out", id)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Exporting is a read, and a read must not write history.
//
// The widget's export button used to take a checkpoint before every export, which is what
// guaranteed a fresh default file name and so hid this: two exports of an unchanged
// project resolve to the same target, so they resolve to the same default name. With the
// checkpoint gone the second one collided and failed, which would have traded a list full
// of "widget export" rows for an export that works once.
func TestRepeatedDefaultExportSucceedsAndAddsNoCheckpoint(t *testing.T) {
	instance := newApp(t)

	checkpoint, err := instance.Jobs.Submit("global", "checkpoint", "tester",
		map[string]any{"message": "mine"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, checkpoint.ID)

	before, err := instance.Catalog.ListSnapshots("global", 100)
	if err != nil {
		t.Fatal(err)
	}

	// Three times, because the bug appears on the second and a fix that only survives one
	// repeat is not a fix.
	for attempt := 0; attempt < 3; attempt++ {
		export, err := instance.Jobs.Submit("global", "export", "tester", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		// waitJob fails the test on a failed job, which is the assertion: before this,
		// attempts two and three died on "already exists; remove or rename it first".
		record := waitJob(t, instance, export.ID)
		if !strings.Contains(record.Log, "wrote ") {
			t.Fatalf("export %d never said where the pack went:\n%s", attempt, record.Log)
		}
	}

	after, err := instance.Catalog.ListSnapshots("global", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("exporting created %d checkpoint(s); it must create none",
			len(after)-len(before))
	}

	// And every pack it wrote is named by the convention, through the real job rather
	// than the naming function on its own.
	entries, err := os.ReadDir(
		filepath.Join(instance.Config.Current().DataDir, "shuttle", "out"))
	if err != nil {
		t.Fatal(err)
	}
	packs := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}
		packs++
		if !packNamePattern.MatchString(entry.Name()) {
			t.Fatalf("pack %q does not follow the naming convention", entry.Name())
		}
	}
	if packs == 0 {
		t.Fatal("three exports wrote no pack")
	}
}

// pkgreg-<project>-<full|delta>-<timestamp>-<ids>.tar
var packNamePattern = regexp.MustCompile(
	`^pkgreg-[a-z0-9._-]+-` +
		`(full-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}` +
		`|delta-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{12}-[0-9a-f]{12})` +
		`\.tar$`)

// The escape hatch stays shut. A name somebody typed is a place they chose, and being
// told it is taken is the answer they want — the silence is only for a name this job
// picked itself, which encodes the checkpoint and so describes the file already there.
func TestRepeatedNamedExportStillRefuses(t *testing.T) {
	instance := newApp(t)

	checkpoint, err := instance.Jobs.Submit("global", "checkpoint", "tester",
		map[string]any{"message": "mine"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, checkpoint.ID)

	first, err := instance.Jobs.Submit("global", "export", "tester",
		map[string]any{"file": "chosen.tar"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, first.ID)

	second, err := instance.Jobs.Submit("global", "export", "tester",
		map[string]any{"file": "chosen.tar"})
	if err != nil {
		t.Fatal(err)
	}
	record := waitJobSettled(t, instance, second.ID)
	if record.Status != "failed" {
		t.Fatalf("a second export to a chosen name %s; want failed", record.Status)
	}
	if !strings.Contains(record.Error, "already exists") {
		t.Fatalf("refusal lost its reason: %q", record.Error)
	}
}
