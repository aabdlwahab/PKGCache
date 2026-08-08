package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/upstream"
)

// Errors from the write path.
var (
	// ErrExists means write-once semantics refused an overwrite.
	ErrExists = errors.New("engine: already exists")
	// ErrTooLarge means the body exceeded the configured cap.
	ErrTooLarge = errors.New("engine: content too large")
	// ErrChecksum means the caller declared a digest the body did not match.
	ErrChecksum = errors.New("engine: checksum mismatch")
	// ErrReadOnly means writes are refused because the project is offline. The
	// air-gapped side is a mirror: an upload there would be silently discarded by
	// the next import.
	ErrReadOnly = errors.New("engine: read-only while offline")
)

// PutOptions controls an upload.
type PutOptions struct {
	// MediaType is stored with the entry.
	MediaType string
	// Overwrite permits replacing existing content. Uploads are write-once by
	// default so a repeated CI job cannot silently change a released artifact.
	Overwrite bool
	// MaxBytes rejects a body larger than this. Zero means unlimited.
	MaxBytes int64
	// ExpectDigest verifies the upload against a caller-supplied sha256.
	ExpectDigest blob.Digest
	// Artifact records an inventory row on success.
	Artifact *catalog.Artifact
	// Origin describes where the upload came from, for the inventory row.
	Origin string
}

// PutResult describes stored content.
type PutResult struct {
	Digest  blob.Digest
	Size    int64
	Created bool // false when an existing entry was replaced
}

// Put stores an uploaded body under a cache key.
//
// The body is hashed as it streams, so an upload is deduplicated against everything
// the host already holds: pushing the same build artifact to ten projects costs one
// copy on disk. Verification happens before the entry is written, so a mismatched
// checksum leaves nothing behind.
func (e *Engine) Put(project, eco, key string, body io.Reader, o PutOptions) (PutResult, error) {
	if e.offline(project) {
		return PutResult{}, ErrReadOnly
	}
	entryKey := catalog.EntryKey{Project: project, Eco: eco, Key: key}

	existing, existErr := e.cat.GetEntry(entryKey)
	exists := existErr == nil
	if exists && !o.Overwrite {
		return PutResult{}, fmt.Errorf("%w: %s", ErrExists, key)
	}

	w, err := e.blobs.Create()
	if err != nil {
		return PutResult{}, err
	}
	defer func() { _ = w.Abort() }() // no-op after Commit

	src := body
	if o.MaxBytes > 0 {
		// Read one byte past the cap so exceeding it is detectable rather than
		// silently truncating the upload.
		src = io.LimitReader(body, o.MaxBytes+1)
	}
	written, err := io.Copy(w, src)
	if err != nil {
		return PutResult{}, fmt.Errorf("engine: reading upload: %w", err)
	}
	if o.MaxBytes > 0 && written > o.MaxBytes {
		return PutResult{}, fmt.Errorf("%w: over %d bytes", ErrTooLarge, o.MaxBytes)
	}
	if o.ExpectDigest != "" && w.Digest() != o.ExpectDigest {
		return PutResult{}, fmt.Errorf("%w: expected %s, got %s", ErrChecksum, o.ExpectDigest, w.Digest())
	}

	digest, size, err := w.Commit()
	if err != nil {
		return PutResult{}, err
	}

	now := time.Now()
	var artifact *catalog.Artifact
	if o.Artifact != nil {
		a := *o.Artifact
		a.Project, a.Eco = project, eco
		a.Digest, a.Size, a.CachedAt = digest, size, now
		if a.Origin == "" {
			a.Origin = o.Origin
		}
		artifact = &a
	}
	replaceArtifact := artifact != nil && exists && existing.Digest != digest
	err = e.blobs.WithBlob(digest, func(_ blob.Stat) error {
		return e.cat.CommitEntry(catalog.Entry{
			EntryKey: entryKey, Digest: digest, Size: size,
			MediaType: o.MediaType, CachedAt: now, LastAccess: now,
		}, artifact, e.quota(project), replaceArtifact)
	})
	if err != nil {
		return PutResult{}, err
	}

	e.stats.recordTraffic(project, eco, false, size)
	return PutResult{Digest: digest, Size: size, Created: !exists}, nil
}

// Entry looks up a cache key without serving it.
func (e *Engine) Entry(project, eco, key string) (catalog.Entry, error) {
	return e.cat.GetEntry(catalog.EntryKey{Project: project, Eco: eco, Key: key})
}

// ListEntries returns cached keys under a prefix.
func (e *Engine) ListEntries(project, eco, prefix string) ([]catalog.Entry, error) {
	return e.cat.ListEntries(catalog.EntryQuery{Project: project, Eco: eco, Prefix: prefix})
}

// BlobExists reports whether immutable content is already present in the shared
// CAS. Digest-addressed protocols use this to answer metadata queries without
// creating an ecosystem-specific catalog entry first.
func (e *Engine) BlobExists(d blob.Digest) bool { return e.blobs.Exists(d) }

// DeleteEntry removes a cache key and any inventory row keyed on it.
func (e *Engine) DeleteEntry(project, eco, key string) error {
	return e.cat.DeleteEntry(catalog.EntryKey{Project: project, Eco: eco, Key: key})
}

// Exchange performs a small, non-cacheable protocol exchange through the shared
// outbound pool. It is for request-dependent control messages such as Git LFS
// batch negotiation, not artifact bodies.
func (e *Engine) Exchange(
	ctx context.Context, project string, req upstream.Request, maxBytes int64,
) (status int, header http.Header, body []byte, err error) {
	if e.offline(project) {
		return 0, nil, nil, upstream.ErrOffline
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	resp, cancel, err := e.pool.Open(ctx, req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, resp.Header.Clone(), nil,
			&UpstreamHTTPError{URL: req.URL, Status: resp.StatusCode}
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return resp.StatusCode, resp.Header.Clone(), nil,
			fmt.Errorf("engine: read upstream exchange: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return resp.StatusCode, resp.Header.Clone(), nil,
			fmt.Errorf("%w: upstream exchange over %d bytes", ErrTooLarge, maxBytes)
	}
	e.pool.CountBytes(req.Eco, req.URL, int64(len(body)))
	return resp.StatusCode, resp.Header.Clone(), body, nil
}

// RecordArtifact adds an inventory row.
func (e *Engine) RecordArtifact(a catalog.Artifact) error { return e.cat.PutArtifact(a) }

// DeleteArtifacts removes every version of one inventory name.
func (e *Engine) DeleteArtifacts(project, eco, name string) error {
	return e.cat.DeleteArtifacts(project, eco, name)
}

// DeleteArtifactVersion removes all architecture rows for one package version.
func (e *Engine) DeleteArtifactVersion(project, eco, name, version string) error {
	return e.cat.DeleteArtifactVersion(project, eco, name, version)
}

// DeleteRef removes one mutable pointer.
func (e *Engine) DeleteRef(project, eco, name string) error {
	return e.cat.DeleteRef(catalog.RefKey{Project: project, Eco: eco, Name: name})
}

// ManagedDir returns the directory an ecosystem owns for a project.
func (e *Engine) ManagedDir(eco, project string) (string, error) {
	return e.blobs.ManagedDir(eco, project)
}

// Blobs exposes the store for the few callers that need direct access — the
// snapshot writer and the garbage collector.
func (e *Engine) Blobs() *blob.Store { return e.blobs }

// Catalog exposes the metadata store, for the same reason.
func (e *Engine) Catalog() catalog.Store { return e.cat }
