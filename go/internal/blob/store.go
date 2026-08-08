package blob

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Directory layout under the store root:
//
//	blobs/sha256/<aa>/<bb>/<hex>   committed content, 0644, immutable
//	blobs/staging/<random>.part    in-flight writes
//	managed/<eco>/<project>/…      directories an ecosystem owns (git mirrors)
//
// Two levels of fan-out keep directory sizes reasonable well past the millions.
const (
	blobsDir   = "blobs"
	sha256Dir  = "sha256"
	stagingDir = "staging"
	managedDir = "managed"
	partSuffix = ".part"

	blobMode = 0o644
	dirMode  = 0o755
)

// Stat is the metadata the store knows about a blob without consulting the catalog.
type Stat struct {
	Size    int64
	ModTime time.Time
}

// Store is a content-addressed blob store on a local filesystem.
//
// Local is a real requirement, not an assumption: commits rely on rename and link
// being atomic within a directory, and cross-project deduplication relies on
// hardlinks. Neither holds on NFS.
type Store struct {
	root    string // <data-dir>
	blobs   string // <data-dir>/blobs/sha256
	staging string // <data-dir>/blobs/staging
	managed string // <data-dir>/managed
	// lifecycle serialises publication of an existing blob with maintenance
	// deletion. Open readers keep working after unlink on Unix; catalog links must
	// not be created after a collector has removed the pathname.
	lifecycle sync.RWMutex
}

// Open prepares the store under root, creating the layout if absent.
func Open(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve root: %w", err)
	}
	s := &Store{
		root:    abs,
		blobs:   filepath.Join(abs, blobsDir, sha256Dir),
		staging: filepath.Join(abs, blobsDir, stagingDir),
		managed: filepath.Join(abs, managedDir),
	}
	for _, d := range []string{s.blobs, s.staging, s.managed} {
		if err := os.MkdirAll(d, dirMode); err != nil {
			return nil, fmt.Errorf("blob: create %s: %w", d, err)
		}
	}
	return s, nil
}

// Root is the store's data directory.
func (s *Store) Root() string { return s.root }

// path is the committed location of d. Callers must have validated d; the guard
// here is defence in depth against a malformed digest read back from the catalog.
func (s *Store) path(d Digest) (string, error) {
	if !d.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidDigest, string(d))
	}
	h := string(d)
	return filepath.Join(s.blobs, h[0:2], h[2:4], h), nil
}

// Path returns the on-disk location of a blob. Exposed for the git role, which needs
// a real path to hand to a subprocess, and for tests.
func (s *Store) Path(d Digest) (string, error) { return s.path(d) }

// Open returns an open file for d. The caller closes it.
func (s *Store) Open(d Digest) (*os.File, Stat, error) {
	p, err := s.path(d)
	if err != nil {
		return nil, Stat{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Stat{}, fmt.Errorf("%w: %s", ErrNotFound, d)
		}
		return nil, Stat{}, fmt.Errorf("blob: open %s: %w", d, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, fmt.Errorf("blob: stat %s: %w", d, err)
	}
	return f, Stat{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Stat reports a blob's metadata and whether it exists.
func (s *Store) Stat(d Digest) (Stat, bool) {
	p, err := s.path(d)
	if err != nil {
		return Stat{}, false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return Stat{}, false
	}
	return Stat{Size: fi.Size(), ModTime: fi.ModTime()}, true
}

// Exists reports whether the store holds d.
func (s *Store) Exists(d Digest) bool {
	_, ok := s.Stat(d)
	return ok
}

// Delete removes a blob. Deleting an absent blob is not an error, so a garbage
// collection pass racing with another is harmless.
//
// Callers must have established that nothing references it — that is the garbage
// collector's job, not this method's.
func (s *Store) Delete(d Digest) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.delete(d)
}

func (s *Store) delete(d Digest) error {
	p, err := s.path(d)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob: delete %s: %w", d, err)
	}
	return nil
}

// WithBlob runs publish while d is guaranteed to remain present. It closes the
// Exists→catalog-link race between the request path and online maintenance.
func (s *Store) WithBlob(d Digest, publish func(Stat) error) error {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	st, ok := s.Stat(d)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	return publish(st)
}

// ImportKnown publishes source under a caller-supplied digest without hashing it.
//
// This is intentionally narrow: it exists for migration from the Python CAS, whose
// filenames are already sha256 digests and whose immutable files may be linked into
// the new store. A same-filesystem source becomes a hardlink. Cross-filesystem
// migration makes one streamed copy, but still does not spend a second pass hashing
// 119 GB of already-addressed content.
func (s *Store) ImportKnown(d Digest, source string) (Stat, error) {
	if !d.Valid() {
		return Stat{}, fmt.Errorf("blob: %w", ErrInvalidDigest)
	}
	info, err := os.Stat(source)
	if err != nil {
		return Stat{}, fmt.Errorf("blob: stat import source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Stat{}, fmt.Errorf("blob: import source is not a regular file: %s", source)
	}
	target, err := s.path(d)
	if err != nil {
		return Stat{}, err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return Stat{}, fmt.Errorf("blob: create import shard: %w", err)
	}

	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if existing, err := os.Stat(target); err == nil {
		if existing.Size() != info.Size() {
			return Stat{}, fmt.Errorf("blob: existing %s has size %d, source has %d",
				d, existing.Size(), info.Size())
		}
		return Stat{Size: existing.Size(), ModTime: existing.ModTime()}, nil
	} else if !os.IsNotExist(err) {
		return Stat{}, fmt.Errorf("blob: stat import target: %w", err)
	}

	if err := os.Link(source, target); err == nil {
		if err := syncDir(dir); err != nil {
			_ = os.Remove(target)
			return Stat{}, err
		}
		return Stat{Size: info.Size(), ModTime: info.ModTime()}, nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return Stat{}, fmt.Errorf("blob: link imported %s: %w", d, err)
	}

	// Different mounts cannot share an inode. Copy to a same-directory temporary,
	// fsync it, then hardlink-publish exactly as Writer.Commit does.
	in, err := os.Open(source)
	if err != nil {
		return Stat{}, fmt.Errorf("blob: open import source: %w", err)
	}
	defer func() { _ = in.Close() }()
	temp, err := os.CreateTemp(dir, ".import-*")
	if err != nil {
		return Stat{}, fmt.Errorf("blob: create import staging: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	written, copyErr := io.CopyBuffer(temp, in, make([]byte, 1<<20))
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return Stat{}, fmt.Errorf("blob: copy import source: %w", copyErr)
	}
	if written != info.Size() {
		return Stat{}, fmt.Errorf("blob: source changed while importing %s", source)
	}
	if err := os.Chmod(tempName, blobMode); err != nil {
		return Stat{}, fmt.Errorf("blob: chmod import staging: %w", err)
	}
	if err := os.Link(tempName, target); err != nil {
		if !os.IsExist(err) {
			return Stat{}, fmt.Errorf("blob: publish imported %s: %w", d, err)
		}
	}
	if err := syncDir(dir); err != nil {
		return Stat{}, err
	}
	return Stat{Size: written, ModTime: info.ModTime()}, nil
}

// DeleteIf atomically rechecks eligibility immediately before unlinking. The
// callback normally flushes and queries the catalog; returning false preserves d.
func (s *Store) DeleteIf(d Digest, eligible func(Stat) (bool, error)) (bool, error) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	st, ok := s.Stat(d)
	if !ok {
		return false, nil
	}
	yes, err := eligible(st)
	if err != nil || !yes {
		return false, err
	}
	if err := s.delete(d); err != nil {
		return false, err
	}
	return true, nil
}

// Walk visits every committed blob. Entries whose names are not valid digests are
// skipped rather than reported: they are not blobs, so they are not ours to police.
func (s *Store) Walk(fn func(Digest, Stat) error) error {
	return filepath.WalkDir(s.blobs, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			// A directory vanishing mid-walk means a concurrent GC. Keep going.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		// A name that is not a digest is not a blob: staging leftovers and stray files
		// are skipped, not reported.
		d, ok := parseDigestName(e.Name())
		if !ok {
			return nil
		}
		fi, ferr := e.Info()
		if ferr != nil {
			if os.IsNotExist(ferr) {
				return nil
			}
			return ferr
		}
		return fn(d, Stat{Size: fi.Size(), ModTime: fi.ModTime()})
	})
}

// Usage reports the blob count and total bytes held. Feeds /metrics and the console
// storage panel.
func (s *Store) Usage() (count, bytes int64, err error) {
	err = s.Walk(func(_ Digest, st Stat) error {
		count++
		bytes += st.Size
		return nil
	})
	return count, bytes, err
}

// CleanStaging removes in-flight write files left behind by a crash. Called at
// startup, where there are by definition no live writers.
//
// A running process must not call this: it would delete the staging files of its own
// in-flight downloads.
func (s *Store) CleanStaging() (removed int, err error) {
	entries, err := os.ReadDir(s.staging)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("blob: read staging: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		if rmErr := os.Remove(filepath.Join(s.staging, e.Name())); rmErr == nil {
			removed++
		}
	}
	return removed, nil
}

// ManagedDir returns the directory an ecosystem owns for a project, creating it if
// needed. Used by the git role, whose bare mirrors are live repositories and cannot
// be content-addressed as a unit.
//
// eco and project are validated as single safe path segments, so a project named
// from an untrusted URL cannot escape the managed tree.
func (s *Store) ManagedDir(eco, project string) (string, error) {
	if err := safeSegment(eco); err != nil {
		return "", fmt.Errorf("blob: managed dir eco: %w", err)
	}
	if err := safeSegment(project); err != nil {
		return "", fmt.Errorf("blob: managed dir project: %w", err)
	}
	p := filepath.Join(s.managed, eco, project)
	if err := os.MkdirAll(p, dirMode); err != nil {
		return "", fmt.Errorf("blob: create managed dir: %w", err)
	}
	return p, nil
}

// safeSegment rejects anything that is not a single, ordinary path component.
func safeSegment(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("invalid path segment %q", s)
	}
	if strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("invalid path segment %q", s)
	}
	return nil
}
