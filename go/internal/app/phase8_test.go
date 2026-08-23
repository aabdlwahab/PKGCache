package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/control"
)

func TestPhase8JobsCheckpointRollbackAndExport(t *testing.T) {
	instance := newApp(t)
	first := store(t, instance, "global", "files", "one", []byte("one"))
	store(t, instance, "global", "npm", "two", []byte("two"))
	managed, err := instance.Blobs.ManagedDir("git", "global")
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(managed, "example.test", "owner", "repo.git")
	if err := os.MkdirAll(filepath.Join(mirror, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "config"),
		[]byte("[core]\n\tbare = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checkpoint, err := instance.Jobs.Submit("global", "checkpoint", "tester",
		map[string]any{"message": "baseline"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, checkpoint.ID)
	history, err := instance.Catalog.ListSnapshots("global", 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	target := history[0]

	if err := instance.Catalog.DeleteEntry(catalog.EntryKey{
		Project: "global", Eco: "files", Key: "one",
	}); err != nil {
		t.Fatal(err)
	}
	store(t, instance, "global", "files", "three", []byte("three"))
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"),
		[]byte("ref: refs/heads/changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "junk"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	rollback, err := instance.Jobs.Submit("global", "rollback", "tester",
		map[string]any{"snapshot": target.ID})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, rollback.ID)
	entries, err := instance.Catalog.ListEntries(catalog.EntryQuery{Project: "global"})
	if err != nil {
		t.Fatal(err)
	}
	foundOne, foundThree := false, false
	for _, entry := range entries {
		foundOne = foundOne || entry.Key == "one" && entry.Digest == first
		foundThree = foundThree || entry.Key == "three"
	}
	if !foundOne || foundThree {
		t.Fatalf("rollback entries = %+v", entries)
	}
	headBytes, err := os.ReadFile(filepath.Join(mirror, "HEAD"))
	if err != nil || string(headBytes) != "ref: refs/heads/main\n" {
		t.Fatalf("managed HEAD = %q, %v", headBytes, err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "junk")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed rollback retained junk: %v", err)
	}

	exported, err := instance.Jobs.Submit("global", "export", "tester",
		map[string]any{"file": "roundtrip.tar"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, exported.ID)
	path := filepath.Join(instance.Config.Current().DataDir, "shuttle", "out", "roundtrip.tar")
	if stat, err := os.Stat(path); err != nil || stat.Size() == 0 {
		t.Fatalf("export = %v, %v", stat, err)
	}

	// A second host starts empty, imports the full pack, and receives both catalog
	// content and the descriptor-owned bare mirror tree.
	second := newApp(t)
	packBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inPath := filepath.Join(second.Config.Current().DataDir, "shuttle", "in", "roundtrip.tar")
	if err := os.WriteFile(inPath, packBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	imported, err := second.Jobs.Submit("global", "import", "tester",
		map[string]any{"file": "roundtrip.tar"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, second, imported.ID)
	secondEntries, err := second.Catalog.ListEntries(catalog.EntryQuery{Project: "global"})
	if err != nil || len(secondEntries) != 2 {
		t.Fatalf("second-host entries = %+v, %v", secondEntries, err)
	}
	secondManaged, err := second.Blobs.ManagedDir("git", "global")
	if err != nil {
		t.Fatal(err)
	}
	secondHead, err := os.ReadFile(filepath.Join(
		secondManaged, "example.test", "owner", "repo.git", "HEAD"))
	if err != nil || string(secondHead) != "ref: refs/heads/main\n" {
		t.Fatalf("second-host managed HEAD = %q, %v", secondHead, err)
	}
}

func waitJob(t *testing.T, instance *App, id int64) control.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		record, err := instance.Jobs.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		switch record.Status {
		case "done":
			return record
		case "failed", "cancelled":
			t.Fatalf("job %d %s: %s\n%s", id, record.Status, record.Error, record.Log)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %d timed out", id)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
