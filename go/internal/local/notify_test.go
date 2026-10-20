package local

import (
	"strings"
	"testing"
	"time"

	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
)

// `pkgcache status` and the window read a move's record through the same function, and a
// "moving" record reads two ways: still running while nothing is serving the cache, and
// dead once a daemon is answering long after the move should have stopped it.
func TestReadMigrationTellsARunningMoveFromADeadOne(t *testing.T) {
	dir := t.TempDir()
	if _, found := ReadMigration(dir, false); found {
		t.Fatal("a cache that never moved reported a move")
	}
	if err := writeMigrationRecord(dir, controlapi.MigrationRecord{
		State: controlapi.MigrationMoving, From: dir, To: "/elsewhere",
		Started: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	record, found := ReadMigration(dir, false)
	if !found || record.State != controlapi.MigrationMoving || record.To != "/elsewhere" {
		t.Fatalf("with no daemon up: %+v, %v; want the move as recorded", record, found)
	}
	record, found = ReadMigration(dir, true)
	if !found || record.State != controlapi.MigrationFailed {
		t.Fatalf("with a daemon answering: %+v, %v; want a move that died", record, found)
	}
	if !strings.Contains(record.Error, MigrationLog(dir)) {
		t.Fatalf("the failure does not say where the move's output is: %q", record.Error)
	}
}
