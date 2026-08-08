package catalog

import (
	"database/sql"
	"fmt"
	"time"
)

// Entry writes are batched.
//
// A `uv sync` of a large project fires thousands of small file requests in a burst,
// each producing one entry row. Writing them individually would mean thousands of
// transactions and thousands of fsyncs on the hot path.
//
// Batching is safe here specifically because of the write ordering: the blob is
// fsynced and linked into the store *before* its entry row is queued. If the process
// dies with rows still queued, the bytes are still on disk and intact — the only cost
// is that the next request for that key misses and re-fetches it. A lost entry insert
// can never produce a wrong answer, only a slower one. That is what buys the right to
// trade durability for throughput here, and it is why the same trick would not be
// acceptable for, say, an account record.

// flushLoop persists queued entries on a fixed cadence, or immediately when the
// batch is full or Flush is called.
func (d *DB) flushLoop() {
	defer d.stopped.Done()
	t := time.NewTicker(d.opts.BatchInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.recordFlush(d.flushLocked())
		case <-d.flushNow:
			d.recordFlush(d.flushLocked())
		}
	}
}

// recordFlush remembers a failure so the next PutEntry surfaces it. Dropping a batch
// is survivable; silently dropping every batch forever is not.
func (d *DB) recordFlush(err error) {
	d.mu.Lock()
	d.flushErr = err
	d.mu.Unlock()
}

// Flush persists any queued entries synchronously. Called before every read that
// must see a complete picture (listings, stats, GC, snapshots) and at shutdown.
func (d *DB) Flush() error {
	if err := d.flushLocked(); err != nil {
		d.recordFlush(err)
		return err
	}
	d.recordFlush(nil)
	return nil
}

// flushLocked drains the pending map into one transaction.
//
// The pending map is detached under the mutex before any SQL runs, so a slow write
// never blocks the request path. On failure the rows are put back — unless a newer
// write for the same key arrived meanwhile, which wins.
func (d *DB) flushLocked() error {
	d.mu.Lock()
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return nil
	}
	batch := d.pending
	d.pending = make(map[EntryKey]Entry, len(batch))
	d.mu.Unlock()

	if err := d.writeEntries(batch); err != nil {
		d.mu.Lock()
		for k, v := range batch {
			if _, superseded := d.pending[k]; !superseded {
				d.pending[k] = v
			}
		}
		d.mu.Unlock()
		return fmt.Errorf("catalog: flush entries: %w", err)
	}
	return nil
}

func (d *DB) writeEntries(batch map[EntryKey]Entry) error {
	return d.inTx(func(tx *sql.Tx) error {
		// An entry references a blob row, so upsert the blob first. The blob's bytes
		// are already committed to the store by this point; this only records them.
		blobStmt, err := tx.Prepare(
			`INSERT INTO blobs(sha256, size, created_at, last_access) VALUES (?, ?, ?, ?)
			 ON CONFLICT(sha256) DO UPDATE SET last_access = MAX(last_access, excluded.last_access)`)
		if err != nil {
			return err
		}
		defer func() { _ = blobStmt.Close() }()

		entryStmt, err := tx.Prepare(
			`INSERT INTO entries(project, eco, key, sha256, size, media_type, cached_at, last_access, hits)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(project, eco, key) DO UPDATE SET
			   sha256=excluded.sha256, size=excluded.size, media_type=excluded.media_type,
			   cached_at=excluded.cached_at,
			   last_access=MAX(last_access, excluded.last_access),
			   hits=hits + excluded.hits`)
		if err != nil {
			return err
		}
		defer func() { _ = entryStmt.Close() }()

		for k, e := range batch {
			if _, err := blobStmt.Exec(string(e.Digest), e.Size,
				ts(e.CachedAt), ts(e.LastAccess)); err != nil {
				return err
			}
			if _, err := entryStmt.Exec(k.Project, k.Eco, k.Key, string(e.Digest),
				e.Size, e.MediaType, ts(e.CachedAt), ts(e.LastAccess), e.Hits); err != nil {
				return err
			}
		}
		return nil
	})
}

// TouchEntries folds one flush window's cache-hit activity into the rows the
// eviction scan reads.
//
// UPDATE rather than upsert, deliberately: a key deleted or evicted between the hit
// and this flush must stay gone. Resurrecting it as a row pointing at a collected
// blob would make the next request a 404 for content we would otherwise re-fetch.
// The blob's own last_access advances too, so blob-level GC sees the same recency
// the entry does.
func (d *DB) TouchEntries(touches []EntryTouch) error {
	if len(touches) == 0 {
		return nil
	}
	err := d.inTx(func(tx *sql.Tx) error {
		entryStmt, err := tx.Prepare(
			`UPDATE entries SET last_access = MAX(last_access, ?), hits = hits + ?
			   WHERE project = ? AND eco = ? AND key = ?`)
		if err != nil {
			return err
		}
		defer func() { _ = entryStmt.Close() }()

		blobStmt, err := tx.Prepare(
			`UPDATE blobs SET last_access = MAX(last_access, ?) WHERE sha256 = ?`)
		if err != nil {
			return err
		}
		defer func() { _ = blobStmt.Close() }()

		for _, t := range touches {
			at := ts(t.LastAccess)
			if _, err := entryStmt.Exec(at, t.Hits, t.Project, t.Eco, t.Key); err != nil {
				return err
			}
			if t.Digest == "" {
				continue
			}
			if _, err := blobStmt.Exec(at, string(t.Digest)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: touch entries: %w", err)
	}
	// The read cache holds the pre-touch counters; drop them rather than reconstruct.
	d.mu.Lock()
	for _, t := range touches {
		d.cache.drop(t.EntryKey)
	}
	d.mu.Unlock()
	return nil
}

// Pending reports how many entry writes are queued. Tests and /metrics.
func (d *DB) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}
