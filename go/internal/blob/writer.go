package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"
)

// Writer streams bytes into the store, hashing them as they go.
//
// The digest is computed inline during the single pass that writes the file, so a
// 2.5 GB wheel is never read twice and never held in memory. This is why every
// download — not only the ones whose digest is known up front — ends up
// deduplicated: by the time the bytes land we already know what they are.
//
// A Writer is not safe for concurrent use. One Writer belongs to one fetch.
type Writer struct {
	store   *Store
	file    *os.File
	hash    hash.Hash
	written int64
	closed  bool // Commit or Abort has run
}

// Create opens a new staging file to write into.
func (s *Store) Create() (*Writer, error) {
	f, err := os.CreateTemp(s.staging, "*"+partSuffix)
	if err != nil {
		return nil, fmt.Errorf("blob: create staging file: %w", err)
	}
	return &Writer{store: s, file: f, hash: sha256.New()}, nil
}

// Write implements io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("blob: write after close")
	}
	n, err := w.file.Write(p)
	if n > 0 {
		// Hash exactly what reached the file, so a short write can never desync the
		// digest from the bytes on disk.
		_, _ = w.hash.Write(p[:n])
		w.written += int64(n)
	}
	if err != nil {
		return n, fmt.Errorf("blob: write staging: %w", err)
	}
	return n, nil
}

// Written is the number of bytes accepted so far. Readers tailing an in-flight fetch
// use this to know how far they may read.
func (w *Writer) Written() int64 { return w.written }

// Digest is the digest of the bytes written so far. Only meaningful once the stream
// is complete; before that it is the digest of a prefix.
func (w *Writer) Digest() Digest {
	return Digest(hex.EncodeToString(w.hash.Sum(nil)))
}

// StagingPath is the in-flight file's location, so progressive readers can tail it.
func (w *Writer) StagingPath() string { return w.file.Name() }

// Commit makes the written bytes durable and publishes them under their digest.
//
// The sequence matters: fsync the data first, then link the name into place, then
// fsync the directory holding that name. A crash at any point leaves either no blob
// or a complete one — never a truncated file under a digest that does not match it.
//
// Commit is idempotent with respect to content. If another writer published the same
// digest first, the existing blob wins and this one's staging file is discarded:
// identical bytes by definition, and keeping the original inode preserves the
// invariant that a blob's inode never changes once created.
func (w *Writer) Commit() (Digest, int64, error) {
	return w.commit(true)
}

// CommitImported is the migration-only form of Commit. When identical content
// already exists it does not refresh that inode's timestamps. A legacy CAS entry
// may still be hardline to the source tree while the live Python service runs;
// touching the destination inode would therefore mutate source metadata and make a
// resume pass believe thousands of immutable files had changed.
//
// The importer runs before the destination daemon and its online collector start,
// so the GC-grace refresh performed by ordinary Commit is not needed there.
func (w *Writer) CommitImported() (Digest, int64, error) {
	return w.commit(false)
}

func (w *Writer) commit(refreshExisting bool) (Digest, int64, error) {
	if w.closed {
		return "", 0, fmt.Errorf("blob: commit after close")
	}
	w.closed = true
	staging := w.file.Name()

	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		_ = os.Remove(staging)
		return "", 0, fmt.Errorf("blob: sync staging: %w", err)
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(staging)
		return "", 0, fmt.Errorf("blob: close staging: %w", err)
	}

	d := w.Digest()
	target, err := w.store.path(d)
	if err != nil {
		_ = os.Remove(staging)
		return "", 0, err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		_ = os.Remove(staging)
		return "", 0, fmt.Errorf("blob: create shard dir: %w", err)
	}
	// World-readable: an operator, a backup job, or a sidecar must be able to read
	// the cache without a chmod dance.
	if err := os.Chmod(staging, blobMode); err != nil {
		_ = os.Remove(staging)
		return "", 0, fmt.Errorf("blob: chmod staging: %w", err)
	}

	// Serialize name publication with online deletion. When identical content
	// already exists, refresh its mtime so the GC grace period also protects the
	// short commit→catalog-link window of this new writer.
	w.store.lifecycle.Lock()
	linkErr := os.Link(staging, target)
	var publishErr error
	if linkErr == nil {
		publishErr = syncDir(dir)
	} else if os.IsExist(linkErr) {
		if refreshExisting {
			now := time.Now()
			publishErr = os.Chtimes(target, now, now)
			if os.IsNotExist(publishErr) {
				publishErr = nil
			}
		}
	}
	w.store.lifecycle.Unlock()
	switch {
	case linkErr == nil:
		// Published. Fsync the directory so the name survives a power loss.
		if publishErr != nil {
			_ = os.Remove(staging)
			return "", 0, publishErr
		}
	case os.IsExist(linkErr):
		if publishErr != nil {
			_ = os.Remove(staging)
			return "", 0, fmt.Errorf("blob: refresh %s: %w", d, publishErr)
		}
	default:
		_ = os.Remove(staging)
		return "", 0, fmt.Errorf("blob: link %s: %w", d, linkErr)
	}

	// Leaking this on a crash is harmless — CleanStaging sweeps it at next startup.
	_ = os.Remove(staging)
	return d, w.written, nil
}

// Abort discards the staging file. Safe to call after Commit (a no-op), so callers
// can `defer w.Abort()` unconditionally.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	name := w.file.Name()
	_ = w.file.Close()
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob: remove staging: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename or link into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("blob: open dir for sync: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("blob: sync dir: %w", err)
	}
	return nil
}
