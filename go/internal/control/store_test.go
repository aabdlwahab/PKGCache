package control

import (
	"path/filepath"
	"testing"
)

func TestControlMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	for pass := 0; pass < 2; pass++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open pass %d: %v", pass, err)
		}
		version, err := db.SchemaVersion()
		if err != nil {
			t.Fatal(err)
		}
		if version != schemaVersion() {
			t.Fatalf("schema version = %d, want %d", version, schemaVersion())
		}
		if err := db.Ping(); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlCRUDPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(Project{Name: "team-a", DataPlaneAuth: "public"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendAudit(AuditRecord{Action: "project.create", Target: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.Project("team-a")
	if err != nil || project.Name != "team-a" {
		t.Fatalf("project after restart = %+v, %v", project, err)
	}
	audit, err := db.ListAudit(10)
	if err != nil || len(audit) != 1 || audit[0].Target != "team-a" {
		t.Fatalf("audit after restart = %+v, %v", audit, err)
	}
}
