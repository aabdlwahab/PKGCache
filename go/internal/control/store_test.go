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

// The v5 migration rebuilds the upstreams table, and a rebuild is the one kind of
// migration that can quietly lose something. This is the upgrade an already-deployed
// server performs: a v4 database with rows in it, opened by a binary that knows v5.
//
// Ids are asserted alongside the rows because the control API addresses an upstream by id
// for PATCH and DELETE — renumbering them would invalidate every id a console is holding.
func TestUpstreamsSurviveTheV5Rebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	// A database as it stood before this change: migrated to v4 and no further.
	raw, err := openRawDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(raw, 4); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id        int64
		project   any
		eco, name string
		url, kind string
		priority  int
		enabled   bool
	}{
		{7, nil, "npm", "registry", "https://registry.npmjs.org", "origin", 0, true},
		{8, "work", "pypi", "root/pypi", "https://pypi.org/simple", "origin", 20, true},
		{9, "work", "npm", "registry", "https://cache.internal/work/npm", "origin", 10, false},
	}
	for _, row := range rows {
		if _, err := raw.Exec(
			`INSERT INTO upstreams(id, project, eco, name, url, kind, priority, enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, row.project, row.eco, row.name, row.url, row.kind, row.priority, row.enabled,
		); err != nil {
			t.Fatalf("seeding id %d: %v", row.id, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// The upgrade: opening it with this binary migrates it forward.
	db, err := Open(path)
	if err != nil {
		t.Fatalf("opening a v4 database with a v5 binary: %v", err)
	}
	defer func() { _ = db.Close() }()
	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion() {
		t.Fatalf("schema version = %d after the upgrade, want %d", version, schemaVersion())
	}

	for _, want := range rows {
		var (
			url, kind string
			priority  int
			enabled   bool
		)
		if err := db.sql.QueryRow(
			`SELECT url, kind, priority, enabled FROM upstreams WHERE id = ?`, want.id,
		).Scan(&url, &kind, &priority, &enabled); err != nil {
			t.Fatalf("row %d did not survive the rebuild: %v", want.id, err)
		}
		if url != want.url || kind != want.kind || priority != want.priority ||
			enabled != want.enabled {
			t.Errorf("row %d = %s %s p%d enabled=%v, want %s %s p%d enabled=%v",
				want.id, url, kind, priority, enabled,
				want.url, want.kind, want.priority, want.enabled)
		}
	}
	// The global project is stored as NULL, and a rebuild that turned it into an empty
	// string would silently detach every one of its upstreams.
	var globals int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM upstreams WHERE project IS NULL`).Scan(&globals); err != nil {
		t.Fatal(err)
	}
	if globals != 1 {
		t.Errorf("%d rows belong to the global project, want 1", globals)
	}

	// And the point of the migration: a named project may now hold a chain.
	if _, err := db.sql.Exec(
		`INSERT INTO upstreams(project, eco, name, url, kind, priority, enabled)
		 VALUES ('work', 'npm', 'registry', 'https://registry.npmjs.org', 'origin', 20, 1)`,
	); err != nil {
		t.Fatalf("a second origin for one index at another priority was refused: %v", err)
	}
	// While two at the same position remain a contradiction.
	if _, err := db.sql.Exec(
		`INSERT INTO upstreams(project, eco, name, url, kind, priority, enabled)
		 VALUES ('work', 'npm', 'registry', 'https://elsewhere', 'origin', 20, 1)`,
	); err == nil {
		t.Error("two origins at the same priority for one index were accepted")
	}
}

// An older binary must refuse a database it does not understand rather than operating on
// it: the v5 rebuild is forward-only, so a rollback is a restore and not a binary swap.
// This is the check that makes that true.
func TestAnOlderBinaryRefusesANewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := openRawDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	// A database from a future binary.
	if _, err := raw.Exec(
		`INSERT INTO control_schema_version(version) VALUES (?)`, schemaVersion()+1); err != nil {
		t.Fatal(err)
	}
	if err := migrate(raw); err == nil {
		t.Fatal("a database newer than this binary was accepted")
	}
}
