package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// A "document" is a small upstream file whose content the cache must read rather
// than merely relay: a PyPI simple index, an npm packument, an apt Release file. The
// adapter parses it and usually rewrites URLs inside it before serving, so unlike a
// wheel or a layer it is buffered rather than streamed.
//
// Documents are mutable, which is what separates them from everything else in the
// store. The previous design handled that four different ways — OCI had an oci_tags
// table, git had git_refs, apt wrote .meta sidecar files beside each index, and npm
// simply re-fetched the packument on every request. All four are the same thing: a
// name pointing at immutable content, with a freshness policy. Here that is a Ref,
// and this is the one code path that maintains one.

// defaultDocMaxBytes bounds a buffered document. grpcio's PyPI index is around 6 MB
// with ten thousand files in it, so the cap must be generous — but unbounded would
// let a broken or hostile origin exhaust memory.
const defaultDocMaxBytes = 64 << 20

// DocSpec describes a document to fetch, cache and keep fresh.
type DocSpec struct {
	Project string
	Eco     string
	// Name identifies the mutable pointer, e.g. "simple/numpy".
	Name string
	// Key is where the bytes live in the byte cache. Defaults to Name.
	Key string
	// URL is the upstream document.
	URL string
	// TTL is how long a cached copy is trusted before revalidating. Zero means
	// revalidate every time, which is right for content with no stability guarantee.
	TTL time.Duration
	// Immutable makes the entry permanent once fetched. OCI manifests requested by
	// digest are small documents, but unlike indexes they never need revalidation.
	Immutable bool
	// Expect applies the same integrity constraints as the streaming path. OCI
	// manifests are buffered so the adapter can inspect them, but a digest-addressed
	// response must still be rejected before it reaches the blob store.
	Expect Expect
	// Headers are sent upstream — an Accept selecting PEP 691 JSON, for instance.
	Headers http.Header
	// Fallbacks are the other origins this document can be fetched from, in order.
	//
	// Documents need this more than artifacts do: an index is what a cold cache asks
	// for first, so a chain whose middle tier could not serve indexes would fall back
	// for tarballs and fail for the packument that names them.
	Fallbacks []upstream.Fallback
	// Credential authenticates to a private index.
	Credential *upstream.Credential
	// MaxBytes overrides the buffering cap.
	MaxBytes int64
}

func (d DocSpec) key() string {
	if d.Key != "" {
		return d.Key
	}
	return d.Name
}

// Document is a fetched document plus how it was obtained. Adapters mostly ignore
// the provenance flags; they exist so the console can explain why an index looks old.
type Document struct {
	Body      []byte
	MediaType string
	Digest    blob.Digest
	// FromCache reports that no upstream request was made at all.
	FromCache bool
	// Revalidated reports that upstream was asked and answered 304.
	Revalidated bool
	// Stale reports that upstream was unreachable and this is the last known good
	// copy. Serving it is deliberate: an index a few minutes old beats a failed
	// build when the origin has a blip.
	Stale bool
}

// Document fetches, caches and revalidates an upstream document.
//
// Order of preference:
//
//  1. a fresh cached copy, with no upstream request at all
//  2. offline: the last known copy, whatever its age
//  3. conditional revalidation — a 304 keeps the bytes and just restarts the clock
//  4. a full fetch
//  5. upstream failed but we hold a copy: serve it stale rather than fail the build
//
// Concurrent callers for the same document collapse into one fetch. That is the fix
// for a specific production stall: uv requests a project's index and then
// immediately requests each of its files, and the previous implementation re-fetched
// and re-parsed the index along the way. For grpcio — 6 MB, ten thousand entries —
// that stalled the event loop under a concurrent CUDA install and timed clients out.
func (e *Engine) Document(ctx context.Context, spec DocSpec) (*Document, error) {
	sfKey := spec.Project + "\x00" + spec.Eco + "\x00" + spec.Name
	v, err, _ := e.docs.Do(sfKey, func() (any, error) {
		return e.document(ctx, spec)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Document), nil
}

func (e *Engine) document(ctx context.Context, spec DocSpec) (*Document, error) {
	refKey := catalog.RefKey{Project: spec.Project, Eco: spec.Eco, Name: spec.Name}
	entryKey := catalog.EntryKey{Project: spec.Project, Eco: spec.Eco, Key: spec.key()}
	now := time.Now()

	ref, haveRef := e.lookupRef(refKey)
	cached, haveCached := e.loadDocument(entryKey)

	// Digest-addressed documents are permanent. They do not need a mutable ref and
	// must not contact upstream again once the verified bytes are present.
	if spec.Immutable && haveCached {
		cached.FromCache = true
		return cached, nil
	}

	// 1. fresh enough to use as-is
	if haveRef && haveCached && ref.Fresh(now) {
		cached.FromCache = true
		return cached, nil
	}

	// 2. offline: last known, at any age
	if e.offline(spec.Project) {
		if haveCached {
			cached.FromCache, cached.Stale = true, true
			return cached, nil
		}
		return nil, fmt.Errorf("%w: document %s/%s/%s", ErrNotCached, spec.Project, spec.Eco, spec.Name)
	}

	req := upstream.Request{
		URL:        spec.URL,
		Headers:    cloneHeader(spec.Headers),
		Credential: spec.Credential,
		Eco:        spec.Eco,
		Fallbacks:  spec.Fallbacks,
	}
	// 3. conditional revalidation — cheap, and the common case for a stable index.
	if haveRef && haveCached {
		if ref.ETag != "" {
			req.Headers.Set("If-None-Match", ref.ETag)
		}
		if ref.LastModified != "" {
			req.Headers.Set("If-Modified-Since", ref.LastModified)
		}
	}

	resp, cancel, err := e.pool.Open(ctx, req)
	if err != nil {
		return e.staleOr(cached, haveCached, err)
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && haveCached {
		// Unchanged: keep the bytes, restart the freshness clock. This is what makes
		// a short TTL cheap rather than wasteful.
		e.putRef(refKey, ref.Target, cached.MediaType, resp, spec.TTL, now)
		cached.Revalidated = true
		return cached, nil
	}
	if resp.StatusCode != http.StatusOK {
		return e.staleOr(cached, haveCached,
			&UpstreamHTTPError{URL: spec.URL, Status: resp.StatusCode})
	}

	// 4. full fetch
	limit := spec.MaxBytes
	if limit <= 0 {
		limit = defaultDocMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return e.staleOr(cached, haveCached, fmt.Errorf("engine: reading document %s: %w", spec.URL, err))
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("engine: document %s exceeds the %d byte cap", spec.URL, limit)
	}
	if spec.Expect.Size > 0 && int64(len(body)) != spec.Expect.Size {
		return nil, fmt.Errorf("%w: expected %d bytes, got %d",
			ErrSizeMismatch, spec.Expect.Size, len(body))
	}
	sum := sha256.Sum256(body)
	got := blob.Digest(hex.EncodeToString(sum[:]))
	if spec.Expect.Digest != "" && got != spec.Expect.Digest {
		return nil, fmt.Errorf("%w: %s expected %s, got %s",
			ErrDigestMismatch, spec.URL, spec.Expect.Digest, got)
	}

	digest, err := e.storeBytes(body)
	if err != nil {
		return nil, err
	}
	mediaType := resp.Header.Get("Content-Type")

	_ = e.cat.PutEntry(catalog.Entry{
		EntryKey: entryKey, Digest: digest, Size: int64(len(body)),
		MediaType: mediaType, CachedAt: now, LastAccess: now,
	})
	if !spec.Immutable {
		e.putRef(refKey, digest.String(), mediaType, resp, spec.TTL, now)
	}
	e.pool.CountBytes(spec.Eco, spec.URL, int64(len(body)))

	return &Document{Body: body, MediaType: mediaType, Digest: digest}, nil
}

// staleOr serves the last known copy when upstream fails, or surfaces the failure
// when there is nothing cached. Availability beats freshness for an index: a build
// that succeeds against a slightly old index is better than one that fails.
func (e *Engine) staleOr(cached *Document, haveCached bool, err error) (*Document, error) {
	if haveCached {
		cached.FromCache, cached.Stale = true, true
		return cached, nil
	}
	return nil, err
}

func (e *Engine) lookupRef(k catalog.RefKey) (catalog.Ref, bool) {
	ref, err := e.cat.GetRef(k)
	if err != nil {
		return catalog.Ref{}, false
	}
	return ref, true
}

// loadDocument reads a cached document's bytes back out of the blob store.
func (e *Engine) loadDocument(k catalog.EntryKey) (*Document, bool) {
	entry, err := e.cat.GetEntry(k)
	if err != nil {
		return nil, false
	}
	f, _, err := e.blobs.Open(entry.Digest)
	if err != nil {
		// The catalog and the store disagree; drop the row so the next request
		// re-fetches rather than failing repeatedly.
		_ = e.cat.DeleteEntry(k)
		return nil, false
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return &Document{Body: body, MediaType: entry.MediaType, Digest: entry.Digest}, true
}

func (e *Engine) putRef(
	k catalog.RefKey, target, mediaType string, resp *http.Response, ttl time.Duration, now time.Time,
) {
	_ = e.cat.PutRef(catalog.Ref{
		RefKey:       k,
		Target:       target,
		MediaType:    mediaType,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    now,
		TTL:          ttl,
	})
}

// storeBytes puts a small in-memory buffer into the blob store, deduplicating it
// against everything already held.
func (e *Engine) storeBytes(body []byte) (blob.Digest, error) {
	w, err := e.blobs.Create()
	if err != nil {
		return "", err
	}
	defer func() { _ = w.Abort() }()
	if _, err := w.Write(body); err != nil {
		return "", err
	}
	digest, _, err := w.Commit()
	return digest, err
}

// ---- refs -----------------------------------------------------------------

// Ref returns a mutable pointer, or false when none is recorded.
func (e *Engine) Ref(project, eco, name string) (catalog.Ref, bool) {
	return e.lookupRef(catalog.RefKey{Project: project, Eco: eco, Name: name})
}

// SetRef records a mutable pointer. Adapters call this for names they resolve
// themselves — an OCI tag whose digest came from a manifest response, or a git
// branch whose commit came from a fetch.
func (e *Engine) SetRef(r catalog.Ref) error {
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now()
	}
	return e.cat.PutRef(r)
}

// ListRefs returns a project's refs for one ecosystem, optionally prefix-filtered.
// This is how the offline side answers "which tags does this mirror hold?".
func (e *Engine) ListRefs(project, eco, prefix string) ([]catalog.Ref, error) {
	return e.cat.ListRefs(project, eco, prefix)
}

// PutBytes stores a small buffer under a cache key and returns its digest. For
// adapters that generate content rather than fetching it.
func (e *Engine) PutBytes(project, eco, key string, body []byte, mediaType string) (blob.Digest, error) {
	digest, err := e.storeBytes(body)
	if err != nil {
		return "", err
	}
	now := time.Now()
	err = e.cat.PutEntry(catalog.Entry{
		EntryKey:  catalog.EntryKey{Project: project, Eco: eco, Key: key},
		Digest:    digest,
		Size:      int64(len(body)),
		MediaType: mediaType,
		CachedAt:  now, LastAccess: now,
	})
	return digest, err
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	return h.Clone()
}

// docGroup deduplicates concurrent document fetches. Documents need no progressive
// delivery, so plain singleflight is right here rather than the Fetch machinery.
type docGroup = singleflight.Group
