package catalog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, so the binary stays static

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

const driverName = "sqlite"

// Options configures a catalog database.
type Options struct {
	// Path to the database file. Its directory must exist.
	Path string
	// ReadPoolSize bounds concurrent readers. WAL allows readers to run alongside
	// the single writer, so this is real parallelism, not queueing.
	ReadPoolSize int
	// BatchInterval is how long an entry write may sit unflushed. Entry inserts are
	// batched because a `uv sync` burst produces thousands of them, and a blob is
	// already durable before its entry row is written — a lost insert costs a
	// re-fetch, never corruption.
	BatchInterval time.Duration
	// BatchSize forces a flush once this many entries are queued.
	BatchSize int
	// CacheSize is the number of entries held in the read-through LRU.
	CacheSize int
	// Now is injectable for tests.
	Now func() time.Time
}

func (o *Options) setDefaults() {
	if o.ReadPoolSize <= 0 {
		o.ReadPoolSize = 8
	}
	if o.BatchInterval <= 0 {
		o.BatchInterval = 100 * time.Millisecond
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if o.CacheSize <= 0 {
		o.CacheSize = 4096
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// DB is the SQLite-backed catalog.
//
// It holds two connection pools against the same file: one writer, and a pool of
// readers. WAL mode lets those readers run concurrently with the writer, so a stats
// query never blocks an in-flight cache commit. Serialising writes through a single
// connection is what keeps SQLITE_BUSY from ever appearing against ourselves.
type DB struct {
	opts  Options
	write *sql.DB
	read  *sql.DB

	// Entry writes are batched. `pending` holds what is queued but not yet on disk
	// so that a read immediately after a write still sees it — without this, a second
	// request for a just-cached artifact would miss and re-fetch it.
	mu      sync.Mutex
	pending map[EntryKey]Entry
	cache   *entryCache

	flushNow chan struct{}
	stop     chan struct{}
	stopped  sync.WaitGroup
	closed   bool
	flushErr error
}

var _ Store = (*DB)(nil)

// Open prepares the catalog at opts.Path, applying any pending migrations.
func Open(opts Options) (*DB, error) {
	opts.setDefaults()
	if opts.Path == "" {
		return nil, errors.New("catalog: Options.Path is required")
	}

	write, err := openPool(opts.Path, 1)
	if err != nil {
		return nil, err
	}
	if err := migrate(write); err != nil {
		_ = write.Close()
		return nil, err
	}
	read, err := openPool(opts.Path, opts.ReadPoolSize)
	if err != nil {
		_ = write.Close()
		return nil, err
	}

	db := &DB{
		opts:     opts,
		write:    write,
		read:     read,
		pending:  make(map[EntryKey]Entry),
		cache:    newEntryCache(opts.CacheSize),
		flushNow: make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	db.stopped.Add(1)
	go db.flushLoop()
	return db, nil
}

func openPool(path string, maxConns int) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: resolve path: %w", err)
	}
	// WAL for reader/writer concurrency; NORMAL because the blob is already fsynced
	// before its row is written, so a lost transaction costs a re-fetch, not data;
	// busy_timeout as a backstop against cross-process contention (the CLI).
	dsn := "file:" + url.PathEscape(abs) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("catalog: ping %s: %w", path, err)
	}
	return db, nil
}

// ---- blobs ---------------------------------------------------------------

// UpsertBlob records a blob. CreatedAt is preserved on conflict — the first sighting
// is the true creation time — while LastAccess moves forward.
func (d *DB) UpsertBlob(b Blob) error {
	if !b.Digest.Valid() {
		return fmt.Errorf("catalog: %w", blob.ErrInvalidDigest)
	}
	now := d.opts.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	if b.LastAccess.IsZero() {
		b.LastAccess = now
	}
	_, err := d.write.Exec(
		`INSERT INTO blobs(sha256, size, created_at, last_access) VALUES (?, ?, ?, ?)
		 ON CONFLICT(sha256) DO UPDATE SET
		   last_access = MAX(last_access, excluded.last_access),
		   size        = excluded.size`,
		string(b.Digest), b.Size, ts(b.CreatedAt), ts(b.LastAccess))
	if err != nil {
		return fmt.Errorf("catalog: upsert blob: %w", err)
	}
	return nil
}

// GetBlob returns a blob row, or ErrNotFound.
func (d *DB) GetBlob(dg blob.Digest) (Blob, error) {
	var b Blob
	var created, access int64
	var hex string
	err := d.read.QueryRow(
		`SELECT sha256, size, created_at, last_access FROM blobs WHERE sha256 = ?`,
		string(dg)).Scan(&hex, &b.Size, &created, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return Blob{}, fmt.Errorf("%w: blob %s", ErrNotFound, dg)
	}
	if err != nil {
		return Blob{}, fmt.Errorf("catalog: get blob: %w", err)
	}
	b.Digest = blob.Digest(hex)
	b.CreatedAt, b.LastAccess = fromTS(created), fromTS(access)
	return b, nil
}

// TouchBlobs advances the last-access time used by the eviction scan.
func (d *DB) TouchBlobs(ds []blob.Digest, at time.Time) error {
	if len(ds) == 0 {
		return nil
	}
	return d.inTx(func(tx *sql.Tx) error {
		st, err := tx.Prepare(`UPDATE blobs SET last_access = ? WHERE sha256 = ?`)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		for _, dg := range ds {
			if _, err := st.Exec(ts(at), string(dg)); err != nil {
				return err
			}
		}
		return nil
	})
}

// WalkBlobs visits every blob row, oldest access first.
func (d *DB) WalkBlobs(fn func(Blob) error) error {
	rows, err := d.read.Query(`SELECT sha256, size, created_at, last_access FROM blobs ORDER BY last_access`)
	if err != nil {
		return fmt.Errorf("catalog: walk blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b Blob
		var hex string
		var created, access int64
		if err := rows.Scan(&hex, &b.Size, &created, &access); err != nil {
			return fmt.Errorf("catalog: scan blob: %w", err)
		}
		b.Digest = blob.Digest(hex)
		b.CreatedAt, b.LastAccess = fromTS(created), fromTS(access)
		if err := fn(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

// UnreferencedBlobs returns blobs that no entry, artifact or snapshot manifest points
// at and that were created before olderThan.
//
// The grace period is essential, not cosmetic: a blob is committed to the store
// before its entry row is written, so a blob created seconds ago may be mid-fetch.
// Collecting it would delete content a request is about to serve.
func (d *DB) UnreferencedBlobs(olderThan time.Time) (digests []blob.Digest, err error) {
	if err := d.Flush(); err != nil { // queued entries are references too
		return nil, err
	}
	rows, err := d.read.Query(
		`SELECT b.sha256 FROM blobs b
		 WHERE b.created_at < ?
		   AND NOT EXISTS (SELECT 1 FROM entries   e WHERE e.sha256 = b.sha256)
		   AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.sha256 = b.sha256)
		   AND NOT EXISTS (SELECT 1 FROM snapshots s WHERE s.manifest_sha256 = b.sha256)`,
		ts(olderThan))
	if err != nil {
		return nil, fmt.Errorf("catalog: unreferenced blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []blob.Digest
	for rows.Next() {
		var hex string
		if err := rows.Scan(&hex); err != nil {
			return nil, fmt.Errorf("catalog: scan digest: %w", err)
		}
		out = append(out, blob.Digest(hex))
	}
	return out, rows.Err()
}

// ---- entries -------------------------------------------------------------

// GetEntry resolves a cache key. Checks the LRU, then the not-yet-flushed batch, then
// the database — so a read always sees this process's own writes.
func (d *DB) GetEntry(k EntryKey) (Entry, error) {
	d.mu.Lock()
	if e, ok := d.pending[k]; ok {
		d.mu.Unlock()
		return e, nil
	}
	if e, ok := d.cache.get(k); ok {
		d.mu.Unlock()
		return e, nil
	}
	d.mu.Unlock()

	var e Entry
	var hex string
	var cached, access int64
	err := d.read.QueryRow(
		`SELECT sha256, size, media_type, cached_at, last_access, hits
		   FROM entries WHERE project = ? AND eco = ? AND key = ?`,
		k.Project, k.Eco, k.Key).
		Scan(&hex, &e.Size, &e.MediaType, &cached, &access, &e.Hits)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: entry %s/%s/%s", ErrNotFound, k.Project, k.Eco, k.Key)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("catalog: get entry: %w", err)
	}
	e.EntryKey = k
	e.Digest = blob.Digest(hex)
	e.CachedAt, e.LastAccess = fromTS(cached), fromTS(access)

	d.mu.Lock()
	d.cache.put(e)
	d.mu.Unlock()
	return e, nil
}

// PutEntry queues an entry for persistence and makes it immediately visible to
// GetEntry. See Options.BatchInterval for why this is not a synchronous write.
func (d *DB) PutEntry(e Entry) error {
	if !e.Digest.Valid() {
		return fmt.Errorf("catalog: %w", blob.ErrInvalidDigest)
	}
	now := d.opts.Now()
	if e.CachedAt.IsZero() {
		e.CachedAt = now
	}
	if e.LastAccess.IsZero() {
		e.LastAccess = now
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	d.pending[e.EntryKey] = e
	d.cache.put(e)
	full := len(d.pending) >= d.opts.BatchSize
	err := d.flushErr
	d.mu.Unlock()

	if full {
		select {
		case d.flushNow <- struct{}{}:
		default:
		}
	}
	return err // surface a previous flush failure rather than swallowing it forever
}

// CommitEntry publishes an entry and its optional inventory row in one writer
// transaction. Project quotas are calculated in that same transaction, which makes
// concurrent commits serialize at the point where oversubscription could occur.
func (d *DB) CommitEntry(
	e Entry, artifact *Artifact, quota Quota, replaceArtifactName bool,
) error {
	if !e.Digest.Valid() {
		return fmt.Errorf("catalog: %w", blob.ErrInvalidDigest)
	}
	if err := d.Flush(); err != nil {
		return err
	}
	now := d.opts.Now()
	if e.CachedAt.IsZero() {
		e.CachedAt = now
	}
	if e.LastAccess.IsZero() {
		e.LastAccess = now
	}
	err := d.inTx(func(tx *sql.Tx) error {
		var usage, oldSize int64
		if err := tx.QueryRow(
			`SELECT COALESCE(SUM(size), 0) FROM entries WHERE project = ?`,
			e.Project).Scan(&usage); err != nil {
			return err
		}
		err := tx.QueryRow(
			`SELECT size FROM entries WHERE project = ? AND eco = ? AND key = ?`,
			e.Project, e.Eco, e.Key).Scan(&oldSize)
		if errors.Is(err, sql.ErrNoRows) {
			oldSize, err = 0, nil
		}
		if err != nil {
			return err
		}
		projected := usage - oldSize + e.Size
		if quota.Bytes > 0 && projected > quota.Bytes {
			return &QuotaError{
				Kind: "bytes", Usage: usage, Limit: quota.Bytes, Attempt: projected,
			}
		}

		if artifact != nil && quota.Artifacts > 0 {
			var count, exists int64
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM artifacts WHERE project = ?`, e.Project,
			).Scan(&count); err != nil {
				return err
			}
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM artifacts
				   WHERE project = ? AND eco = ? AND name = ? AND version = ? AND arch = ?`,
				e.Project, e.Eco, artifact.Name, artifact.Version, artifact.Arch,
			).Scan(&exists); err != nil {
				return err
			}
			projectedCount := count
			if exists == 0 {
				projectedCount++
			}
			if replaceArtifactName {
				var replaced int64
				if err := tx.QueryRow(
					`SELECT COUNT(*) FROM artifacts
					   WHERE project = ? AND eco = ? AND name = ?`,
					e.Project, e.Eco, artifact.Name,
				).Scan(&replaced); err != nil {
					return err
				}
				projectedCount = count - replaced + 1
			}
			if projectedCount > quota.Artifacts {
				return &QuotaError{
					Kind: "artifacts", Usage: count,
					Limit: quota.Artifacts, Attempt: projectedCount,
				}
			}
		}

		if _, err := tx.Exec(
			`INSERT INTO blobs(sha256, size, created_at, last_access) VALUES (?, ?, ?, ?)
			 ON CONFLICT(sha256) DO UPDATE SET
			   last_access=MAX(last_access, excluded.last_access), size=excluded.size`,
			string(e.Digest), e.Size, ts(e.CachedAt), ts(e.LastAccess),
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO entries(project, eco, key, sha256, size, media_type, cached_at, last_access, hits)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(project, eco, key) DO UPDATE SET
			   sha256=excluded.sha256, size=excluded.size, media_type=excluded.media_type,
			   cached_at=excluded.cached_at,
			   last_access=MAX(last_access, excluded.last_access),
			   hits=hits + excluded.hits`,
			e.Project, e.Eco, e.Key, string(e.Digest), e.Size, e.MediaType,
			ts(e.CachedAt), ts(e.LastAccess), e.Hits,
		); err != nil {
			return err
		}
		if artifact == nil {
			return nil
		}
		a := *artifact
		a.Project, a.Eco, a.Digest, a.Size = e.Project, e.Eco, e.Digest, e.Size
		if a.CachedAt.IsZero() {
			a.CachedAt = now
		}
		if replaceArtifactName {
			if _, err := tx.Exec(
				`DELETE FROM artifacts WHERE project = ? AND eco = ? AND name = ?`,
				a.Project, a.Eco, a.Name,
			); err != nil {
				return err
			}
		}
		var extra any
		if len(a.Extra) > 0 {
			encoded, err := json.Marshal(a.Extra)
			if err != nil {
				return err
			}
			extra = string(encoded)
		}
		_, err = tx.Exec(
			`INSERT INTO artifacts(project, eco, name, version, arch, sha256, size, origin, cached_at, extra)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(project, eco, name, version, arch) DO UPDATE SET
			   sha256=excluded.sha256, size=excluded.size, origin=excluded.origin,
			   cached_at=excluded.cached_at, extra=excluded.extra`,
			a.Project, a.Eco, a.Name, a.Version, a.Arch, string(a.Digest),
			a.Size, a.Origin, ts(a.CachedAt), extra,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("catalog: commit entry: %w", err)
	}
	d.mu.Lock()
	d.cache.put(e)
	d.mu.Unlock()
	return nil
}

// DeleteEntry removes one cache entry. The blob it referenced is left for the garbage
// collector, which is the only thing that knows whether anything else still wants it.
func (d *DB) DeleteEntry(k EntryKey) error {
	d.mu.Lock()
	delete(d.pending, k)
	d.cache.drop(k)
	d.mu.Unlock()

	if _, err := d.write.Exec(
		`DELETE FROM entries WHERE project = ? AND eco = ? AND key = ?`,
		k.Project, k.Eco, k.Key); err != nil {
		return fmt.Errorf("catalog: delete entry: %w", err)
	}
	return nil
}

// EvictEntry removes one entry and derived views, returning whether the referenced
// content is still live elsewhere.
func (d *DB) EvictEntry(k EntryKey) (digest blob.Digest, size int64, stillReferenced bool, err error) {
	if err := d.Flush(); err != nil {
		return "", 0, false, err
	}
	err = d.inTx(func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRow(
			`SELECT sha256, size FROM entries WHERE project = ? AND eco = ? AND key = ?`,
			k.Project, k.Eco, k.Key,
		).Scan(&raw, &size)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		digest = blob.Digest(raw)
		for _, statement := range []string{
			`DELETE FROM entries WHERE project = ? AND eco = ? AND key = ?`,
			`DELETE FROM refs WHERE project = ? AND eco = ? AND target = ?`,
		} {
			if _, err := tx.Exec(statement, k.Project, k.Eco, k.Key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`DELETE FROM artifacts
			   WHERE project = ? AND eco = ? AND sha256 = ?`,
			k.Project, k.Eco, raw,
		); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(
			`SELECT
			   EXISTS(SELECT 1 FROM entries WHERE sha256 = ?) OR
			   EXISTS(SELECT 1 FROM artifacts WHERE sha256 = ?) OR
			   EXISTS(SELECT 1 FROM snapshots WHERE manifest_sha256 = ?)`,
			raw, raw, raw,
		).Scan(&n); err != nil {
			return err
		}
		stillReferenced = n != 0
		return nil
	})
	if err != nil {
		return "", 0, false, fmt.Errorf("catalog: evict entry: %w", err)
	}
	d.mu.Lock()
	d.cache.drop(k)
	d.mu.Unlock()
	return digest, size, stillReferenced, nil
}

// DeleteProject removes every entry and artifact belonging to a project and reports
// how many entries went. The bytes become reclaimable by the next GC — which is the
// whole point of the design: in the previous one, deleting a project deliberately
// left its bytes on disk forever because nothing knew what else referenced them.
func (d *DB) DeleteProject(project string) (int64, error) {
	if err := d.Flush(); err != nil {
		return 0, err
	}
	var n int64
	err := d.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM entries WHERE project = ?`, project)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		for _, q := range []string{
			`DELETE FROM artifacts    WHERE project = ?`,
			`DELETE FROM refs         WHERE project = ?`,
			`DELETE FROM access_stats WHERE project = ?`,
			`DELETE FROM traffic_stats WHERE project = ?`,
			`DELETE FROM heads        WHERE project = ?`,
			`DELETE FROM snapshots    WHERE project = ?`,
		} {
			if _, err := tx.Exec(q, project); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("catalog: delete project: %w", err)
	}
	d.mu.Lock()
	d.cache.dropProject(project)
	d.mu.Unlock()
	return n, nil
}

// ListEntries returns entries matching q, ordered by key. Drives the files role's
// directory listing, which is a catalog query rather than a readdir — always
// consistent with what the cache will actually serve.
func (d *DB) ListEntries(q EntryQuery) ([]Entry, error) {
	if err := d.Flush(); err != nil {
		return nil, err
	}
	var (
		where []string
		args  []any
	)
	if q.Project != "" {
		where, args = append(where, "project = ?"), append(args, q.Project)
	}
	if q.Eco != "" {
		where, args = append(where, "eco = ?"), append(args, q.Eco)
	}
	if q.Prefix != "" {
		where, args = append(where, "key GLOB ?"), append(args, globEscape(q.Prefix)+"*")
	}
	sqlText := `SELECT project, eco, key, sha256, size, media_type, cached_at, last_access, hits FROM entries`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY key"
	if q.Limit > 0 {
		sqlText += " LIMIT ? OFFSET ?"
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := d.read.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		var e Entry
		var hex string
		var cached, access int64
		if err := rows.Scan(&e.Project, &e.Eco, &e.Key, &hex, &e.Size,
			&e.MediaType, &cached, &access, &e.Hits); err != nil {
			return nil, fmt.Errorf("catalog: scan entry: %w", err)
		}
		e.Digest = blob.Digest(hex)
		e.CachedAt, e.LastAccess = fromTS(cached), fromTS(access)
		out = append(out, e)
	}
	return out, rows.Err()
}

// WalkEntries streams one project's entries in manifest order. Unlike ListEntries,
// this never accumulates the result, which keeps checkpoints flat-memory even for
// very large catalogs.
func (d *DB) WalkEntries(project string, fn func(Entry) error) error {
	return d.walkEntries(
		`SELECT project, eco, key, sha256, size, media_type, cached_at, last_access, hits
		   FROM entries WHERE project = ? ORDER BY eco, key`,
		[]any{project}, fn)
}

// WalkEntriesEco streams one ecosystem in key order. Checkpoint uses this form to
// merge descriptor-owned managed directory snapshots into the global eco ordering.
func (d *DB) WalkEntriesEco(project, eco string, fn func(Entry) error) error {
	return d.walkEntries(
		`SELECT project, eco, key, sha256, size, media_type, cached_at, last_access, hits
		   FROM entries WHERE project = ? AND eco = ? ORDER BY key`,
		[]any{project, eco}, fn)
}

// WalkEvictionCandidates streams least-recently-used entries first.
func (d *DB) WalkEvictionCandidates(project string, fn func(Entry) error) error {
	query := `SELECT project, eco, key, sha256, size, media_type, cached_at, last_access, hits
	            FROM entries`
	var args []any
	if project != "" {
		query += ` WHERE project = ?`
		args = append(args, project)
	}
	query += ` ORDER BY last_access, project, eco, key`
	return d.walkEntries(query, args, fn)
}

func (d *DB) walkEntries(query string, args []any, fn func(Entry) error) error {
	if err := d.Flush(); err != nil {
		return err
	}
	rows, err := d.read.Query(query, args...)
	if err != nil {
		return fmt.Errorf("catalog: walk entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			e              Entry
			hex            string
			cached, access int64
		)
		if err := rows.Scan(&e.Project, &e.Eco, &e.Key, &hex, &e.Size,
			&e.MediaType, &cached, &access, &e.Hits); err != nil {
			return fmt.Errorf("catalog: scan entry: %w", err)
		}
		e.Digest = blob.Digest(hex)
		e.CachedAt, e.LastAccess = fromTS(cached), fromTS(access)
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: walk entries: %w", err)
	}
	return nil
}

// ApplySnapshot atomically replaces a project's live entry set and advances HEAD.
// Refs and artifacts are derived views whose source data is not in the compact
// manifest, so they are cleared rather than left pointing at rolled-back content.
func (d *DB) ApplySnapshot(project, snapshotID string, source EntrySource) error {
	return d.applySnapshot(project, "", snapshotID, false, source)
}

// ApplySnapshotFrom is ApplySnapshot with a transactional fast-forward guard.
func (d *DB) ApplySnapshotFrom(
	project, expectedHead, snapshotID string, source EntrySource,
) error {
	return d.applySnapshot(project, expectedHead, snapshotID, true, source)
}

func (d *DB) applySnapshot(
	project, expectedHead, snapshotID string, checkHead bool, source EntrySource,
) error {
	if project == "" || snapshotID == "" {
		return errors.New("catalog: project and snapshot id are required")
	}
	if err := d.Flush(); err != nil {
		return err
	}
	now := d.opts.Now()
	err := d.inTx(func(tx *sql.Tx) error {
		if checkHead {
			var current string
			err := tx.QueryRow(`SELECT snapshot_id FROM heads WHERE project = ?`, project).
				Scan(&current)
			if errors.Is(err, sql.ErrNoRows) {
				current, err = "", nil
			}
			if err != nil {
				return err
			}
			if current != expectedHead {
				return fmt.Errorf(
					"non-fast-forward: local HEAD is %q, pack requires base %q",
					current, expectedHead)
			}
		}
		for _, q := range []string{
			`DELETE FROM entries WHERE project = ?`,
			`DELETE FROM refs WHERE project = ?`,
			`DELETE FROM artifacts WHERE project = ?`,
		} {
			if _, err := tx.Exec(q, project); err != nil {
				return err
			}
		}
		stmt, err := tx.Prepare(
			`INSERT INTO entries(project, eco, key, sha256, size, media_type, cached_at, last_access, hits)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		if err := source(func(e Entry) error {
			if e.Project != "" && e.Project != project {
				return fmt.Errorf("entry belongs to project %q, not %q", e.Project, project)
			}
			if e.Eco == "" || e.Key == "" || !e.Digest.Valid() || e.Size < 0 {
				return errors.New("invalid snapshot entry")
			}
			if e.CachedAt.IsZero() {
				e.CachedAt = now
			}
			if e.LastAccess.IsZero() {
				e.LastAccess = now
			}
			_, err := stmt.Exec(project, e.Eco, e.Key, string(e.Digest), e.Size,
				e.MediaType, ts(e.CachedAt), ts(e.LastAccess), e.Hits)
			return err
		}); err != nil {
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO heads(project, snapshot_id) VALUES (?, ?)
			 ON CONFLICT(project) DO UPDATE SET snapshot_id = excluded.snapshot_id`,
			project, snapshotID)
		return err
	})
	if err != nil {
		return fmt.Errorf("catalog: apply snapshot: %w", err)
	}
	d.mu.Lock()
	d.cache.dropProject(project)
	d.mu.Unlock()
	return nil
}

// CountEntries reports a project's entry count and logical size. Note this is the sum
// of entry sizes, not disk usage: bytes shared with another project are counted here
// but stored once.
func (d *DB) CountEntries(project string) (count, bytes int64, err error) {
	if err := d.Flush(); err != nil {
		return 0, 0, err
	}
	err = d.read.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM entries WHERE project = ?`,
		project).Scan(&count, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("catalog: count entries: %w", err)
	}
	return count, bytes, nil
}

// ---- refs ----------------------------------------------------------------

// GetRef returns a mutable pointer, or ErrNotFound.
func (d *DB) GetRef(k RefKey) (Ref, error) {
	var r Ref
	var fetched, ttl int64
	err := d.read.QueryRow(
		`SELECT target, media_type, etag, last_modified, fetched_at, ttl_seconds
		   FROM refs WHERE project = ? AND eco = ? AND name = ?`,
		k.Project, k.Eco, k.Name).
		Scan(&r.Target, &r.MediaType, &r.ETag, &r.LastModified, &fetched, &ttl)
	if errors.Is(err, sql.ErrNoRows) {
		return Ref{}, fmt.Errorf("%w: ref %s/%s/%s", ErrNotFound, k.Project, k.Eco, k.Name)
	}
	if err != nil {
		return Ref{}, fmt.Errorf("catalog: get ref: %w", err)
	}
	r.RefKey = k
	r.FetchedAt = fromTS(fetched)
	r.TTL = time.Duration(ttl) * time.Second
	return r, nil
}

// PutRef stores or updates a mutable pointer.
func (d *DB) PutRef(r Ref) error {
	if r.FetchedAt.IsZero() {
		r.FetchedAt = d.opts.Now()
	}
	_, err := d.write.Exec(
		`INSERT INTO refs(project, eco, name, target, media_type, etag, last_modified, fetched_at, ttl_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project, eco, name) DO UPDATE SET
		   target=excluded.target, media_type=excluded.media_type, etag=excluded.etag,
		   last_modified=excluded.last_modified, fetched_at=excluded.fetched_at,
		   ttl_seconds=excluded.ttl_seconds`,
		r.Project, r.Eco, r.Name, r.Target, r.MediaType, r.ETag, r.LastModified,
		ts(r.FetchedAt), int64(r.TTL.Seconds()))
	if err != nil {
		return fmt.Errorf("catalog: put ref: %w", err)
	}
	return nil
}

// DeleteRef removes a mutable pointer.
func (d *DB) DeleteRef(k RefKey) error {
	if _, err := d.write.Exec(
		`DELETE FROM refs WHERE project = ? AND eco = ? AND name = ?`,
		k.Project, k.Eco, k.Name); err != nil {
		return fmt.Errorf("catalog: delete ref: %w", err)
	}
	return nil
}

// ListRefs returns a project's refs for one ecosystem, optionally prefix-filtered.
// This is how the offline side answers "which tags does this mirror hold?".
func (d *DB) ListRefs(project, eco, prefix string) ([]Ref, error) {
	args := []any{project, eco}
	q := `SELECT name, target, media_type, etag, last_modified, fetched_at, ttl_seconds
	        FROM refs WHERE project = ? AND eco = ?`
	if prefix != "" {
		q += " AND name GLOB ?"
		args = append(args, globEscape(prefix)+"*")
	}
	q += " ORDER BY name"

	rows, err := d.read.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Ref
	for rows.Next() {
		r := Ref{RefKey: RefKey{Project: project, Eco: eco}}
		var fetched, ttl int64
		if err := rows.Scan(&r.Name, &r.Target, &r.MediaType, &r.ETag,
			&r.LastModified, &fetched, &ttl); err != nil {
			return nil, fmt.Errorf("catalog: scan ref: %w", err)
		}
		r.FetchedAt = fromTS(fetched)
		r.TTL = time.Duration(ttl) * time.Second
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- artifacts -----------------------------------------------------------

// PutArtifact records or updates an inventory row.
func (d *DB) PutArtifact(a Artifact) error {
	if a.CachedAt.IsZero() {
		a.CachedAt = d.opts.Now()
	}
	var extra any
	if len(a.Extra) > 0 {
		b, err := json.Marshal(a.Extra)
		if err != nil {
			return fmt.Errorf("catalog: marshal artifact extra: %w", err)
		}
		extra = string(b)
	}
	_, err := d.write.Exec(
		`INSERT INTO artifacts(project, eco, name, version, arch, sha256, size, origin, cached_at, extra)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project, eco, name, version, arch) DO UPDATE SET
		   sha256=excluded.sha256, size=excluded.size, origin=excluded.origin,
		   cached_at=excluded.cached_at, extra=excluded.extra`,
		a.Project, a.Eco, a.Name, a.Version, a.Arch, string(a.Digest),
		a.Size, a.Origin, ts(a.CachedAt), extra)
	if err != nil {
		return fmt.Errorf("catalog: put artifact: %w", err)
	}
	return nil
}

// DeleteArtifacts removes every version of one package. Used by the files role, where
// re-uploading changed content must replace the inventory row rather than accumulate
// one row per distinct digest.
func (d *DB) DeleteArtifacts(project, eco, name string) error {
	if _, err := d.write.Exec(
		`DELETE FROM artifacts WHERE project = ? AND eco = ? AND name = ?`,
		project, eco, name); err != nil {
		return fmt.Errorf("catalog: delete artifacts: %w", err)
	}
	return nil
}

// DeleteArtifactVersion removes every architecture row for one version. OCI uses
// this before replacing a mutable tag's platform set, so an architecture removed
// from a refreshed image index does not remain in the inventory.
func (d *DB) DeleteArtifactVersion(project, eco, name, version string) error {
	if _, err := d.write.Exec(
		`DELETE FROM artifacts WHERE project = ? AND eco = ? AND name = ? AND version = ?`,
		project, eco, name, version); err != nil {
		return fmt.Errorf("catalog: delete artifact version: %w", err)
	}
	return nil
}

var artifactSortColumns = map[string]string{
	"name":    "name",
	"size":    "size DESC",
	"date":    "cached_at DESC",
	"version": "version",
}

// QueryArtifacts returns a filtered, sorted page of the inventory plus the total
// match count. An empty Project or Eco means "across all of them" — the cross-cutting
// question the previous per-(project, role) sharding could not express.
func (d *DB) QueryArtifacts(q ArtifactQuery) ([]Artifact, int, error) {
	var (
		where []string
		args  []any
	)
	if q.Project != "" {
		where, args = append(where, "project = ?"), append(args, q.Project)
	}
	if q.Eco != "" {
		where, args = append(where, "eco = ?"), append(args, q.Eco)
	}
	if q.Search != "" {
		where, args = append(where, "name LIKE ? ESCAPE '\\'"), append(args, "%"+likeEscape(q.Search)+"%")
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.read.QueryRow(`SELECT COUNT(*) FROM artifacts`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("catalog: count artifacts: %w", err)
	}

	order, ok := artifactSortColumns[q.Sort]
	if !ok {
		order = "name"
	}
	text := `SELECT project, eco, name, version, arch, sha256, size, origin, cached_at, extra
	           FROM artifacts` + clause + ` ORDER BY ` + order + `, name, version`
	pageArgs := args
	if q.PageSize > 0 {
		page := max(q.Page, 1)
		text += " LIMIT ? OFFSET ?"
		pageArgs = append(append([]any{}, args...), q.PageSize, (page-1)*q.PageSize)
	}

	rows, err := d.read.Query(text, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: query artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanArtifacts(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func scanArtifacts(rows *sql.Rows) ([]Artifact, error) {
	// Empty, not nil: every caller of this returns straight into a JSON body, where a
	// nil slice becomes null and forces the client to treat "none" as a special case.
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var hex string
		var cached int64
		var extra sql.NullString
		if err := rows.Scan(&a.Project, &a.Eco, &a.Name, &a.Version, &a.Arch,
			&hex, &a.Size, &a.Origin, &cached, &extra); err != nil {
			return nil, fmt.Errorf("catalog: scan artifact: %w", err)
		}
		a.Digest = blob.Digest(hex)
		a.CachedAt = fromTS(cached)
		if extra.Valid && extra.String != "" {
			_ = json.Unmarshal([]byte(extra.String), &a.Extra)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- snapshots -----------------------------------------------------------

// PutSnapshot records a checkpoint.
func (d *DB) PutSnapshot(s Snapshot) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = d.opts.Now()
	}
	_, err := d.write.Exec(
		`INSERT INTO snapshots(id, project, parent, manifest_sha256, entry_count, total_bytes, created_at, subject, author)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Project, s.Parent, string(s.Manifest), s.EntryCount, s.TotalBytes,
		ts(s.CreatedAt), s.Subject, s.Author)
	if err != nil {
		return fmt.Errorf("catalog: put snapshot: %w", err)
	}
	return nil
}

// CommitSnapshot records a checkpoint and advances HEAD in one transaction.
func (d *DB) CommitSnapshot(s Snapshot) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = d.opts.Now()
	}
	err := d.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO snapshots(id, project, parent, manifest_sha256, entry_count, total_bytes, created_at, subject, author)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.Project, s.Parent, string(s.Manifest), s.EntryCount, s.TotalBytes,
			ts(s.CreatedAt), s.Subject, s.Author); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO heads(project, snapshot_id) VALUES (?, ?)
			 ON CONFLICT(project) DO UPDATE SET snapshot_id = excluded.snapshot_id`,
			s.Project, s.ID)
		return err
	})
	if err != nil {
		return fmt.Errorf("catalog: commit snapshot: %w", err)
	}
	return nil
}

// GetSnapshot returns one checkpoint, or ErrNotFound.
func (d *DB) GetSnapshot(id string) (Snapshot, error) {
	var s Snapshot
	var hex string
	var created int64
	err := d.read.QueryRow(
		`SELECT id, project, parent, manifest_sha256, entry_count, total_bytes, created_at, subject, author
		   FROM snapshots WHERE id = ?`, id).
		Scan(&s.ID, &s.Project, &s.Parent, &hex, &s.EntryCount, &s.TotalBytes,
			&created, &s.Subject, &s.Author)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("%w: snapshot %s", ErrNotFound, id)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("catalog: get snapshot: %w", err)
	}
	s.Manifest = blob.Digest(hex)
	s.CreatedAt = fromTS(created)
	return s, nil
}

// ListSnapshots returns a project's checkpoints, newest first. This is the History
// panel — no git log required.
func (d *DB) ListSnapshots(project string, limit int) ([]Snapshot, error) {
	q := `SELECT id, project, parent, manifest_sha256, entry_count, total_bytes, created_at, subject, author
	        FROM snapshots WHERE project = ? ORDER BY created_at DESC, id DESC`
	args := []any{project}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.read.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var hex string
		var created int64
		if err := rows.Scan(&s.ID, &s.Project, &s.Parent, &hex, &s.EntryCount,
			&s.TotalBytes, &created, &s.Subject, &s.Author); err != nil {
			return nil, fmt.Errorf("catalog: scan snapshot: %w", err)
		}
		s.Manifest = blob.Digest(hex)
		s.CreatedAt = fromTS(created)
		out = append(out, s)
	}
	return out, rows.Err()
}

// WalkSnapshots streams every checkpoint. Maintenance opens each manifest and marks
// its content digests, making every retained checkpoint a pin.
func (d *DB) WalkSnapshots(fn func(Snapshot) error) error {
	rows, err := d.read.Query(
		`SELECT id, project, parent, manifest_sha256, entry_count, total_bytes,
		        created_at, subject, author
		   FROM snapshots ORDER BY project, created_at, id`)
	if err != nil {
		return fmt.Errorf("catalog: walk snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var snapshot Snapshot
		var digest string
		var created int64
		if err := rows.Scan(
			&snapshot.ID, &snapshot.Project, &snapshot.Parent, &digest,
			&snapshot.EntryCount, &snapshot.TotalBytes, &created,
			&snapshot.Subject, &snapshot.Author,
		); err != nil {
			return fmt.Errorf("catalog: scan snapshot: %w", err)
		}
		snapshot.Manifest = blob.Digest(digest)
		snapshot.CreatedAt = fromTS(created)
		if err := fn(snapshot); err != nil {
			return err
		}
	}
	return rows.Err()
}

// IsBlobReferenced performs the final online-GC recheck against live SQL roots.
// Snapshot contents are checked through the collector's immutable pin set.
func (d *DB) IsBlobReferenced(digest blob.Digest) (bool, error) {
	if err := d.Flush(); err != nil {
		return false, err
	}
	var exists int
	err := d.read.QueryRow(
		`SELECT
		   EXISTS(SELECT 1 FROM entries WHERE sha256 = ?) OR
		   EXISTS(SELECT 1 FROM artifacts WHERE sha256 = ?) OR
		   EXISTS(SELECT 1 FROM snapshots WHERE manifest_sha256 = ?)`,
		string(digest), string(digest), string(digest),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("catalog: check blob references: %w", err)
	}
	return exists != 0, nil
}

// DeleteBlobRecord removes metadata after the content pathname is gone.
func (d *DB) DeleteBlobRecord(digest blob.Digest) error {
	if _, err := d.write.Exec(`DELETE FROM blobs WHERE sha256 = ?`, string(digest)); err != nil {
		return fmt.Errorf("catalog: delete blob record: %w", err)
	}
	return nil
}

// SetHead marks the snapshot a project's live state corresponds to.
func (d *DB) SetHead(project, snapshotID string) error {
	_, err := d.write.Exec(
		`INSERT INTO heads(project, snapshot_id) VALUES (?, ?)
		 ON CONFLICT(project) DO UPDATE SET snapshot_id = excluded.snapshot_id`,
		project, snapshotID)
	if err != nil {
		return fmt.Errorf("catalog: set head: %w", err)
	}
	return nil
}

// GetHead returns a project's current snapshot id, or "" when it has never been
// checkpointed.
func (d *DB) GetHead(project string) (string, error) {
	var id string
	err := d.read.QueryRow(`SELECT snapshot_id FROM heads WHERE project = ?`, project).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("catalog: get head: %w", err)
	}
	return id, nil
}

// ---- stats ---------------------------------------------------------------

// RecordAccess folds one flush window's in-memory deltas into the persistent tallies,
// in a single transaction.
func (d *DB) RecordAccess(access []AccessDelta, traffic []TrafficDelta) error {
	if len(access) == 0 && len(traffic) == 0 {
		return nil
	}
	err := d.inTx(func(tx *sql.Tx) error {
		if len(access) > 0 {
			st, err := tx.Prepare(
				`INSERT INTO access_stats(project, eco, name, count, last_access) VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(project, eco, name) DO UPDATE SET
				   count = count + excluded.count,
				   last_access = MAX(last_access, excluded.last_access)`)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			for _, a := range access {
				if _, err := st.Exec(a.Project, a.Eco, a.Name, a.Count, ts(a.LastAccess)); err != nil {
					return err
				}
			}
		}
		if len(traffic) > 0 {
			st, err := tx.Prepare(
				`INSERT INTO traffic_stats(project, eco, hit_count, hit_bytes, miss_count, miss_bytes)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(project, eco) DO UPDATE SET
				   hit_count  = hit_count  + excluded.hit_count,
				   hit_bytes  = hit_bytes  + excluded.hit_bytes,
				   miss_count = miss_count + excluded.miss_count,
				   miss_bytes = miss_bytes + excluded.miss_bytes`)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			for _, t := range traffic {
				if _, err := st.Exec(t.Project, t.Eco, t.HitCount, t.HitBytes,
					t.MissCount, t.MissBytes); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: record access: %w", err)
	}
	return nil
}

// Stats returns the aggregate view behind the console's statistics tab.
func (d *DB) Stats(q StatsQuery) (StatsResult, error) {
	if err := d.Flush(); err != nil {
		return StatsResult{}, err
	}
	var (
		where []string
		args  []any
	)
	if q.Project != "" {
		where, args = append(where, "project = ?"), append(args, q.Project)
	}
	if q.Eco != "" {
		where, args = append(where, "eco = ?"), append(args, q.Eco)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	byEco := map[string]*EcoStats{}
	get := func(project, eco string) *EcoStats {
		k := project + "\x00" + eco
		if s, ok := byEco[k]; ok {
			return s
		}
		s := &EcoStats{Project: project, Eco: eco}
		byEco[k] = s
		return s
	}

	// Inventory: one GROUP BY instead of six HTTP calls and a Python merge.
	rows, err := d.read.Query(
		`SELECT project, eco, COUNT(*), COALESCE(SUM(size), 0) FROM artifacts`+clause+
			` GROUP BY project, eco`, args...)
	if err != nil {
		return StatsResult{}, fmt.Errorf("catalog: stats inventory: %w", err)
	}
	if err := scanRows(rows, func(rows *sql.Rows) error {
		var p, e string
		var c, s int64
		if err := rows.Scan(&p, &e, &c, &s); err != nil {
			return err
		}
		st := get(p, e)
		st.Count, st.Size = c, s
		return nil
	}); err != nil {
		return StatsResult{}, err
	}

	trows, err := d.read.Query(
		`SELECT project, eco, hit_count, hit_bytes, miss_count, miss_bytes FROM traffic_stats`+clause, args...)
	if err != nil {
		return StatsResult{}, fmt.Errorf("catalog: stats traffic: %w", err)
	}
	if err := scanRows(trows, func(rows *sql.Rows) error {
		var p, e string
		var hc, hb, mc, mb int64
		if err := rows.Scan(&p, &e, &hc, &hb, &mc, &mb); err != nil {
			return err
		}
		st := get(p, e)
		st.HitCount, st.HitBytes, st.MissCount, st.MissBytes = hc, hb, mc, mb
		return nil
	}); err != nil {
		return StatsResult{}, err
	}

	arows, err := d.read.Query(
		`SELECT project, eco, COALESCE(SUM(count), 0) FROM access_stats`+clause+` GROUP BY project, eco`, args...)
	if err != nil {
		return StatsResult{}, fmt.Errorf("catalog: stats requests: %w", err)
	}
	if err := scanRows(arows, func(rows *sql.Rows) error {
		var p, e string
		var n int64
		if err := rows.Scan(&p, &e, &n); err != nil {
			return err
		}
		get(p, e).Requests = n
		return nil
	}); err != nil {
		return StatsResult{}, err
	}

	res := StatsResult{ByEco: make([]EcoStats, 0, len(byEco))}
	for _, s := range byEco {
		res.ByEco = append(res.ByEco, *s)
	}
	sortEcoStats(res.ByEco)

	if res.Leaderboard, err = d.leaderboard(clause, args, 20); err != nil {
		return StatsResult{}, err
	}
	if res.TopLargest, err = d.artifactTop(clause, args, "size DESC", 15); err != nil {
		return StatsResult{}, err
	}
	if res.RecentAdded, err = d.artifactTop(clause, args, "cached_at DESC", 15); err != nil {
		return StatsResult{}, err
	}
	if err := d.read.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs`).
		Scan(&res.TotalBlobs, &res.TotalBytes); err != nil {
		return StatsResult{}, fmt.Errorf("catalog: stats blobs: %w", err)
	}
	return res, nil
}

func (d *DB) leaderboard(clause string, args []any, limit int) ([]PackageCount, error) {
	rows, err := d.read.Query(
		`SELECT eco, name, count, last_access FROM access_stats`+clause+
			` ORDER BY count DESC, name LIMIT ?`, append(append([]any{}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("catalog: leaderboard: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// Initialised rather than declared nil: a nil slice marshals to JSON null, and a
	// caller reading "no packages requested yet" as a missing field is the shape that
	// only ever shows up on a fresh instance.
	out := []PackageCount{}
	for rows.Next() {
		var p PackageCount
		var last int64
		if err := rows.Scan(&p.Eco, &p.Name, &p.Count, &last); err != nil {
			return nil, err
		}
		p.LastAccess = fromTS(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) artifactTop(clause string, args []any, order string, limit int) ([]Artifact, error) {
	rows, err := d.read.Query(
		`SELECT project, eco, name, version, arch, sha256, size, origin, cached_at, extra
		   FROM artifacts`+clause+` ORDER BY `+order+` LIMIT ?`,
		append(append([]any{}, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("catalog: artifact top: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanArtifacts(rows)
}

// ---- lifecycle -----------------------------------------------------------

// Ping verifies both pools are usable. Cheap enough for a readiness probe, which is
// why it is a real query rather than Stats.
func (d *DB) Ping() error {
	var v int
	if err := d.read.QueryRow(`SELECT 1`).Scan(&v); err != nil {
		return fmt.Errorf("catalog: read pool: %w", err)
	}
	if err := d.write.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("catalog: write pool: %w", err)
	}
	return nil
}

// Close flushes queued writes and releases both pools.
func (d *DB) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	close(d.stop)
	d.stopped.Wait()

	err := d.flushLocked() // drain whatever the loop did not
	if cerr := d.read.Close(); err == nil {
		err = cerr
	}
	if cerr := d.write.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *DB) inTx(fn func(*sql.Tx) error) error {
	tx, err := d.write.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- helpers -------------------------------------------------------------

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromTS(i int64) time.Time {
	if i == 0 {
		return time.Time{}
	}
	return time.Unix(i, 0).UTC()
}

// globEscape neutralises GLOB metacharacters so a user-supplied prefix is matched
// literally. GLOB has no ESCAPE clause, so the character class form is the only way.
func globEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']':
			b.WriteString("[" + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// likeEscape neutralises LIKE wildcards; paired with ESCAPE '\' at the call site.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func sortEcoStats(s []EcoStats) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && (s[j].Project < s[j-1].Project ||
			(s[j].Project == s[j-1].Project && s[j].Eco < s[j-1].Eco)); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// scanRows iterates a cursor to completion and closes it exactly once, including on a
// scan failure. It exists because the stats aggregate runs several queries in sequence
// over one connection: each cursor has to be finished and released before the next
// begins, and a bare `defer` inside the calling function would hold all of them open
// until it returned.
func scanRows(rows *sql.Rows, scan func(*sql.Rows) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
