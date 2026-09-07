package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// carry moves a pack from one cache's outbox to another's inbox, which is what a USB
// stick does.
func carry(t *testing.T, from, to *App, name string) {
	t.Helper()
	packed, err := os.ReadFile(
		filepath.Join(from.Config.Current().DataDir, "shuttle", "out", name))
	if err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(to.Config.Current().DataDir, "shuttle", "in")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, name), packed, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A key the pypi adapter can read a name and a version out of. The inventory is derived
// from keys like this one, so a test that used an opaque key would pass without proving
// anything.
const wheelKey = "root/pypi/+f/idna/idna-3.10-py3-none-any.whl"

func artifactNames(t *testing.T, instance *App, project string) []string {
	t.Helper()
	rows, _, err := instance.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: project, PageSize: 100, Page: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Name+" "+row.Version)
	}
	return out
}

// Rolling back used to empty the packages list.
//
// Applying a snapshot clears entries and artifacts together — both describe what the
// project holds — and only the entries were put back. So a rollback left every package
// it had just pinned serving correctly and listed nowhere, which reads as a rollback that
// threw the content away when it had done the opposite.
func TestRollbackKeepsTheInventoryItPinned(t *testing.T) {
	instance := newApp(t)
	store(t, instance, "global", "pypi", wheelKey, []byte("wheel bytes"))
	// The fetch path records the inventory row; this test seeds entries directly, so it
	// records the row the same way an ecosystem would.
	if err := instance.Catalog.PutArtifact(catalog.Artifact{
		Project: "global", Eco: "pypi", Name: "idna", Version: "3.10", Size: 11,
	}); err != nil {
		t.Fatal(err)
	}
	if got := artifactNames(t, instance, "global"); len(got) != 1 {
		t.Fatalf("before the checkpoint the inventory is %v", got)
	}

	checkpoint, err := instance.Jobs.Submit("global", "checkpoint", "tester",
		map[string]any{"message": "with idna"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, checkpoint.ID)
	history, err := instance.Catalog.ListSnapshots("global", 10)
	if err != nil || len(history) == 0 {
		t.Fatalf("history = %+v, %v", history, err)
	}

	// Something arrives after the checkpoint, then the rollback drops it.
	store(t, instance, "global", "pypi", "root/pypi/+f/certifi/certifi-2024.8.30-py3-none-any.whl",
		[]byte("later"))
	rollback, err := instance.Jobs.Submit("global", "rollback", "tester",
		map[string]any{"snapshot": history[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, instance, rollback.ID)

	got := artifactNames(t, instance, "global")
	if len(got) != 1 || got[0] != "idna 3.10" {
		t.Fatalf("after the rollback the inventory is %v, want just idna 3.10", got)
	}
}

// The same failure by the other route: a pack imported onto a fresh cache restored the
// entries and left the packages list empty, so the import looked like it had done nothing.
func TestImportRestoresTheInventory(t *testing.T) {
	source := newApp(t)
	digest := store(t, source, "global", "pypi", wheelKey, []byte("wheel bytes"))
	if err := source.Catalog.PutArtifact(catalog.Artifact{
		Project: "global", Eco: "pypi", Name: "idna", Version: "3.10",
		Digest: digest, Size: 11,
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Jobs.Submit("global", "checkpoint", "tester",
		map[string]any{"message": "with idna"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, source, checkpoint.ID)
	exported, err := source.Jobs.Submit("global", "export", "tester",
		map[string]any{"file": "inventory.tar"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, source, exported.ID)

	destination := newApp(t)
	carry(t, source, destination, "inventory.tar")
	imported, err := destination.Jobs.Submit("global", "import", "tester",
		map[string]any{"file": "inventory.tar"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, destination, imported.ID)

	got := artifactNames(t, destination, "global")
	if len(got) != 1 || got[0] != "idna 3.10" {
		t.Fatalf("the imported cache lists %v, want idna 3.10", got)
	}
}
