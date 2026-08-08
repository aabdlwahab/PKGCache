package frompython

import (
	"context"
	"database/sql"
	"io/fs"
	"time"
)

type progressDB struct {
	ctx context.Context
	db  *sql.DB
}

func openProgress(ctx context.Context, path string) (*progressDB, error) {
	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS completed (
		id TEXT PRIMARY KEY,
		size INTEGER NOT NULL,
		mtime_ns INTEGER NOT NULL,
		digest TEXT NOT NULL DEFAULT '',
		completed_at INTEGER NOT NULL
	) WITHOUT ROWID`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &progressDB{ctx: ctx, db: db}, nil
}

// Done reports whether this exact file was already imported. Size and mtime both have
// to match: a source file rewritten between passes must be treated as new work.
func (p *progressDB) Done(id string, info fs.FileInfo) bool {
	var size, mtime int64
	err := p.db.QueryRowContext(p.ctx, `SELECT size, mtime_ns FROM completed WHERE id=?`, id).
		Scan(&size, &mtime)
	return err == nil && size == info.Size() && mtime == info.ModTime().UnixNano()
}

// LogicalDone reports whether a non-file unit of work — a ref, a stats table — was
// already applied. There is no size or mtime to compare, only the identity.
func (p *progressDB) LogicalDone(id string) bool {
	var one int
	return p.db.QueryRowContext(p.ctx, `SELECT 1 FROM completed WHERE id=?`, id).Scan(&one) == nil
}

// Mark records a file as imported, keyed on its size and mtime so a later pass can
// tell an unchanged file from a replaced one.
func (p *progressDB) Mark(id string, info fs.FileInfo, digest string) error {
	_, err := p.db.ExecContext(p.ctx, `INSERT INTO completed(id,size,mtime_ns,digest,completed_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns,
		digest=excluded.digest, completed_at=excluded.completed_at`,
		id, info.Size(), info.ModTime().UnixNano(), digest, time.Now().Unix())
	return err
}

// MarkLogical records a non-file unit of work as applied.
func (p *progressDB) MarkLogical(id, digest string) error {
	_, err := p.db.ExecContext(p.ctx, `INSERT INTO completed(id,size,mtime_ns,digest,completed_at)
		VALUES(?,0,0,?,?)
		ON CONFLICT(id) DO UPDATE SET digest=excluded.digest,
		completed_at=excluded.completed_at`, id, digest, time.Now().Unix())
	return err
}

// Close flushes and closes the durable progress database.
func (p *progressDB) Close() error { return p.db.Close() }
