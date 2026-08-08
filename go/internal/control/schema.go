package control

import (
	"database/sql"
	"fmt"
)

var migrations = []struct {
	version int
	stmts   []string
}{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS projects (
			   name TEXT PRIMARY KEY, owner TEXT, created_at INTEGER NOT NULL,
			   offline INTEGER NOT NULL DEFAULT 0,
			   quota_bytes INTEGER NOT NULL DEFAULT 0,
			   quota_artifacts INTEGER NOT NULL DEFAULT 0,
			   data_plane_auth TEXT NOT NULL DEFAULT 'public'
			 ) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS instance_flags (
			   key TEXT PRIMARY KEY, value TEXT NOT NULL
			 ) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS users (
			   username TEXT PRIMARY KEY, role TEXT NOT NULL,
			   salt TEXT NOT NULL, hash TEXT NOT NULL,
			   reports_to TEXT, created_at INTEGER NOT NULL
			 ) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS tokens (
			   id TEXT PRIMARY KEY, secret_sha256 TEXT NOT NULL,
			   project TEXT, eco TEXT, scope TEXT NOT NULL,
			   label TEXT, created_by TEXT, created_at INTEGER NOT NULL,
			   expires_at INTEGER, last_used INTEGER
			 ) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS ix_tokens_scope ON tokens(project, eco, scope)`,
			`CREATE TABLE IF NOT EXISTS credentials (
			   id INTEGER PRIMARY KEY, label TEXT NOT NULL,
			   kind TEXT NOT NULL, sealed BLOB NOT NULL
			 )`,
			`CREATE TABLE IF NOT EXISTS upstreams (
			   id INTEGER PRIMARY KEY, project TEXT, eco TEXT NOT NULL,
			   name TEXT NOT NULL, url TEXT NOT NULL, kind TEXT NOT NULL,
			   priority INTEGER NOT NULL DEFAULT 0,
			   enabled INTEGER NOT NULL DEFAULT 1,
			   credential_id INTEGER REFERENCES credentials(id),
			   UNIQUE (project, eco, name)
			 )`,
			`CREATE TABLE IF NOT EXISTS jobs (
			   id INTEGER PRIMARY KEY AUTOINCREMENT, project TEXT, action TEXT NOT NULL,
			   status TEXT NOT NULL, params TEXT, started_at INTEGER,
			   finished_at INTEGER, error TEXT, actor TEXT
			 )`,
			`CREATE INDEX IF NOT EXISTS ix_jobs_project ON jobs(project, id DESC)`,
			`CREATE TABLE IF NOT EXISTS audit (
			   id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
			   actor TEXT, action TEXT NOT NULL, target TEXT, detail TEXT, client_ip TEXT
			 )`,
			`CREATE INDEX IF NOT EXISTS ix_audit_ts ON audit(ts DESC)`,
		},
	},
	{
		version: 2,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS job_logs (
			   job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			   seq INTEGER NOT NULL, line TEXT NOT NULL,
			   PRIMARY KEY(job_id, seq)
			 ) WITHOUT ROWID`,
		},
	},
	{
		version: 3,
		stmts: []string{
			`ALTER TABLE projects ADD COLUMN rate_limit INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE projects ADD COLUMN rate_burst INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE tokens ADD COLUMN rate_limit INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE tokens ADD COLUMN rate_burst INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		// Project access beyond the single owner.
		//
		// Before this, a project had exactly one operator — its owner — and the only
		// way for a second admin to work on it was to hand the whole project over or
		// to make everyone a superuser. Both are worse than the problem: the first
		// loses the original owner, the second grants the audit log, every other
		// tenant and account management along with it.
		//
		// No foreign keys on purpose. projects.name is the global project's one
		// absentee — that tenant is implicit and never stored — so a reference would
		// make it the one project that can never be shared. Cascades are done
		// explicitly in DeleteProject and DeleteUser instead.
		version: 4,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS project_grants (
			   project TEXT NOT NULL, username TEXT NOT NULL,
			   level TEXT NOT NULL, granted_by TEXT, created_at INTEGER NOT NULL,
			   PRIMARY KEY (project, username)
			 ) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS ix_project_grants_user ON project_grants(username)`,
		},
	},
}

func schemaVersion() int { return migrations[len(migrations)-1].version }

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS control_schema_version (
		version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("control: create schema version: %w", err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0)
		FROM control_schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("control: read schema version: %w", err)
	}
	if current > schemaVersion() {
		return fmt.Errorf("control: database is at schema v%d but this binary only knows v%d",
			current, schemaVersion())
	}
	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("control: begin migration v%d: %w", migration.version, err)
		}
		for _, statement := range migration.stmts {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("control: migration v%d: %w", migration.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO control_schema_version(version) VALUES (?)`,
			migration.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("control: record migration v%d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("control: commit migration v%d: %w", migration.version, err)
		}
	}
	return nil
}
