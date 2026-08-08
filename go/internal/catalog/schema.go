package catalog

import (
	"database/sql"
	"fmt"
)

// migrations are applied in order and never edited once released. To change the
// schema, append a new one — a deployment that has already run migration N must be
// able to reach N+1 without the operator doing anything.
var migrations = []struct {
	version int
	stmts   []string
}{
	{
		version: 1,
		stmts: []string{
			// ---- blobs: the content index --------------------------------------
			// Refcounts are deliberately NOT stored. They are derived by the garbage
			// collector from entries + artifacts + snapshot manifests: a maintained
			// counter is one more thing that can drift out of sync with reality, and
			// at this cardinality the mark phase is a cheap index scan.
			`CREATE TABLE IF NOT EXISTS blobs (
			   sha256      TEXT PRIMARY KEY,
			   size        INTEGER NOT NULL,
			   created_at  INTEGER NOT NULL,
			   last_access INTEGER NOT NULL
			 ) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS ix_blobs_access ON blobs(last_access)`,

			// ---- entries: the byte cache ---------------------------------------
			`CREATE TABLE IF NOT EXISTS entries (
			   project     TEXT NOT NULL,
			   eco         TEXT NOT NULL,
			   key         TEXT NOT NULL,
			   sha256      TEXT NOT NULL REFERENCES blobs(sha256),
			   size        INTEGER NOT NULL,
			   media_type  TEXT NOT NULL DEFAULT '',
			   cached_at   INTEGER NOT NULL,
			   last_access INTEGER NOT NULL,
			   hits        INTEGER NOT NULL DEFAULT 0,
			   PRIMARY KEY (project, eco, key)
			 ) WITHOUT ROWID`,
			// The GC mark phase and the "which projects share this blob" question.
			`CREATE INDEX IF NOT EXISTS ix_entries_sha ON entries(sha256)`,
			// The eviction scan.
			`CREATE INDEX IF NOT EXISTS ix_entries_access ON entries(last_access)`,

			// ---- refs: mutable names -------------------------------------------
			`CREATE TABLE IF NOT EXISTS refs (
			   project       TEXT NOT NULL,
			   eco           TEXT NOT NULL,
			   name          TEXT NOT NULL,
			   target        TEXT NOT NULL,
			   media_type    TEXT NOT NULL DEFAULT '',
			   etag          TEXT NOT NULL DEFAULT '',
			   last_modified TEXT NOT NULL DEFAULT '',
			   fetched_at    INTEGER NOT NULL,
			   ttl_seconds   INTEGER NOT NULL,
			   PRIMARY KEY (project, eco, name)
			 ) WITHOUT ROWID`,

			// ---- artifacts: the semantic inventory -----------------------------
			`CREATE TABLE IF NOT EXISTS artifacts (
			   project   TEXT NOT NULL,
			   eco       TEXT NOT NULL,
			   name      TEXT NOT NULL,
			   version   TEXT NOT NULL,
			   arch      TEXT NOT NULL DEFAULT '',
			   sha256    TEXT NOT NULL DEFAULT '',
			   size      INTEGER NOT NULL DEFAULT 0,
			   origin    TEXT NOT NULL DEFAULT '',
			   cached_at INTEGER NOT NULL,
			   extra     TEXT,
			   PRIMARY KEY (project, eco, name, version, arch)
			 ) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS ix_artifacts_name ON artifacts(name)`,
			`CREATE INDEX IF NOT EXISTS ix_artifacts_size ON artifacts(size DESC)`,
			`CREATE INDEX IF NOT EXISTS ix_artifacts_cached ON artifacts(cached_at DESC)`,

			// ---- usage tallies --------------------------------------------------
			`CREATE TABLE IF NOT EXISTS access_stats (
			   project     TEXT NOT NULL,
			   eco         TEXT NOT NULL,
			   name        TEXT NOT NULL,
			   count       INTEGER NOT NULL DEFAULT 0,
			   last_access INTEGER NOT NULL DEFAULT 0,
			   PRIMARY KEY (project, eco, name)
			 ) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS traffic_stats (
			   project    TEXT NOT NULL,
			   eco        TEXT NOT NULL,
			   hit_count  INTEGER NOT NULL DEFAULT 0,
			   hit_bytes  INTEGER NOT NULL DEFAULT 0,
			   miss_count INTEGER NOT NULL DEFAULT 0,
			   miss_bytes INTEGER NOT NULL DEFAULT 0,
			   PRIMARY KEY (project, eco)
			 ) WITHOUT ROWID`,

			// ---- snapshots -------------------------------------------------------
			`CREATE TABLE IF NOT EXISTS snapshots (
			   id              TEXT PRIMARY KEY,
			   project         TEXT NOT NULL,
			   parent          TEXT NOT NULL DEFAULT '',
			   manifest_sha256 TEXT NOT NULL,
			   entry_count     INTEGER NOT NULL,
			   total_bytes     INTEGER NOT NULL,
			   created_at      INTEGER NOT NULL,
			   subject         TEXT NOT NULL,
			   author          TEXT NOT NULL DEFAULT ''
			 )`,
			`CREATE INDEX IF NOT EXISTS ix_snapshots_project ON snapshots(project, created_at DESC)`,
			`CREATE TABLE IF NOT EXISTS heads (
			   project     TEXT PRIMARY KEY,
			   snapshot_id TEXT NOT NULL
			 ) WITHOUT ROWID`,
		},
	},
	{
		version: 2,
		stmts: []string{
			// ---- time series -----------------------------------------------------
			// traffic_stats above holds lifetime totals; these hold shape over time.
			//
			// The `outcome` column is the point of the table. The engine already
			// resolves every request to one of five steps and labels its Prometheus
			// counter with it, but nothing durable kept it — so the console could
			// report a hit rate and still not say whether a hit came from a local
			// entry, a coalesced in-flight fetch, or a peer.
			//
			// Rows are written at span=300 and folded upward by CompactSeries, so the
			// resolution degrades with age instead of the history being deleted.
			`CREATE TABLE IF NOT EXISTS traffic_series (
			   span    INTEGER NOT NULL,
			   bucket  INTEGER NOT NULL,
			   project TEXT NOT NULL,
			   eco     TEXT NOT NULL,
			   outcome TEXT NOT NULL,
			   count   INTEGER NOT NULL DEFAULT 0,
			   bytes   INTEGER NOT NULL DEFAULT 0,
			   PRIMARY KEY (span, bucket, project, eco, outcome)
			 ) WITHOUT ROWID`,
			// Compaction and expiry both scan (span, bucket), which the primary key
			// already leads with. This index serves the console's per-project read.
			`CREATE INDEX IF NOT EXISTS ix_traffic_series_project
			   ON traffic_series(project, span, bucket)`,

			// Growth has to be sampled, not derived. Summing blobs.created_at looks
			// equivalent and is not: GC deletes those rows, so a derived history would
			// silently rewrite itself every time the collector ran.
			`CREATE TABLE IF NOT EXISTS storage_series (
			   bucket      INTEGER PRIMARY KEY,
			   blob_count  INTEGER NOT NULL DEFAULT 0,
			   blob_bytes  INTEGER NOT NULL DEFAULT 0,
			   entry_count INTEGER NOT NULL DEFAULT 0,
			   entry_bytes INTEGER NOT NULL DEFAULT 0,
			   fs_free     INTEGER NOT NULL DEFAULT 0,
			   fs_total    INTEGER NOT NULL DEFAULT 0
			 ) WITHOUT ROWID`,

			// Upstream health, hourly only. Sum and max rather than a histogram:
			// latency distribution is Prometheus's job, and duplicating it here would
			// cost far more than the question the console actually asks, which is
			// "which upstream is slow or failing".
			`CREATE TABLE IF NOT EXISTS upstream_series (
			   span     INTEGER NOT NULL,
			   bucket   INTEGER NOT NULL,
			   project  TEXT NOT NULL,
			   upstream TEXT NOT NULL,
			   requests INTEGER NOT NULL DEFAULT 0,
			   errors   INTEGER NOT NULL DEFAULT 0,
			   bytes    INTEGER NOT NULL DEFAULT 0,
			   ms_sum   INTEGER NOT NULL DEFAULT 0,
			   ms_max   INTEGER NOT NULL DEFAULT 0,
			   PRIMARY KEY (span, bucket, project, upstream)
			 ) WITHOUT ROWID`,
		},
	},
}

// schemaVersion is the version a fresh database is migrated to.
func schemaVersion() int { return migrations[len(migrations)-1].version }

// migrate brings db up to the latest schema. Idempotent: running it against an
// already-current database is a no-op, so it is safe to call unconditionally at every
// startup.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("catalog: create schema_version: %w", err)
	}
	var current int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("catalog: read schema version: %w", err)
	}
	if current > schemaVersion() {
		return fmt.Errorf("catalog: database is at schema v%d but this binary only knows v%d "+
			"— downgrading is not supported", current, schemaVersion())
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("catalog: begin migration v%d: %w", m.version, err)
		}
		for _, s := range m.stmts {
			if _, err := tx.Exec(s); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("catalog: migration v%d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES (?)`, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("catalog: record migration v%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("catalog: commit migration v%d: %w", m.version, err)
		}
	}
	return nil
}
