// Package engine is the cache pipeline every ecosystem shares: single-flight
// fetching, progressive delivery, freshness, and the hit/dedup/miss decision.
//
// Ecosystem adapters describe what they want; this package does it. That is the
// boundary that keeps fetch, hash, commit and catalog logic out of six protocol
// handlers, where in the previous design it was repeated with small variations.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/obs"
	"github.com/brightskies/pkgreg/internal/upstream"
)

// Errors surfaced by a fetch.
var (
	// ErrDigestMismatch means upstream served bytes that do not match the digest the
	// index promised. The content is discarded, never published.
	ErrDigestMismatch = errors.New("engine: upstream content does not match the expected digest")
	// ErrSizeMismatch means the body was shorter or longer than declared — the
	// classic truncated transfer.
	ErrSizeMismatch = errors.New("engine: upstream content length does not match")
	// ErrUpstreamStatus means the origin refused.
	ErrUpstreamStatus = errors.New("engine: upstream returned an error status")
	// errCommitted means a reader attached after the fetch finished; the caller
	// should serve the committed blob instead. Internal.
	errCommitted = errors.New("engine: fetch already committed")
)

// UpstreamHTTPError retains an origin's HTTP status while still matching
// ErrUpstreamStatus through errors.Is. Protocol adapters such as apt must sometimes
// relay the exact status: a 404 for InRelease tells apt to try Release, while turning
// it into a generic 502 breaks repository discovery.
type UpstreamHTTPError struct {
	URL    string
	Status int
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("%s: %s returned %d", ErrUpstreamStatus, e.URL, e.Status)
}

// Unwrap makes errors.Is(err, ErrUpstreamStatus) continue to work.
func (e *UpstreamHTTPError) Unwrap() error { return ErrUpstreamStatus }

// UpstreamStatus extracts a retained origin status.
func UpstreamStatus(err error) (int, bool) {
	var statusErr *UpstreamHTTPError
	if !errors.As(err, &statusErr) {
		return 0, false
	}
	return statusErr.Status, true
}

// copyBufSize is the streaming chunk. Large enough that syscall overhead is
// negligible on a multi-gigabyte transfer, small enough that a thousand concurrent
// fetches cost tens of megabytes rather than gigabytes.
const copyBufSize = 64 << 10

var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBufSize); return &b }}

// Fetch is one in-flight download that any number of clients may read as it lands.
//
// The shape solves a specific problem: when ten build hosts ask for the same 2.5 GB
// wheel at once, the cache must fetch it once, and all ten must start receiving
// bytes immediately rather than waiting for the download to finish. So a single
// goroutine streams upstream into a staging file while readers tail-follow it.
type Fetch struct {
	Key string
	Eco string

	// Set before HeadersReady closes, read-only afterwards.
	Total     int64 // -1 when upstream declared no Content-Length
	MediaType string

	// HeadersReady closes once Total and MediaType are known and the staging file
	// exists — or immediately on an early failure, so a waiter never hangs.
	HeadersReady chan struct{}

	mu      sync.Mutex
	written int64
	done    bool
	err     error
	digest  blob.Digest
	size    int64
	// notify is closed and replaced on every state change. Closing a channel is a
	// broadcast to every waiter at once, which is what sync.Cond would otherwise
	// give us with more ceremony and no context support.
	notify chan struct{}

	staging  string
	readFile *os.File
	// refs counts the fetch itself plus each attached reader, so the shared read
	// descriptor is closed exactly once, after the last reader detaches. Readers use
	// ReadAt, which is safe concurrently because it does not touch a shared offset.
	refs   int
	closed bool

	headersOnce sync.Once
	onFinish    func()
}

func newFetch(key, eco string) *Fetch {
	return &Fetch{
		Key:          key,
		Eco:          eco,
		Total:        -1,
		HeadersReady: make(chan struct{}),
		notify:       make(chan struct{}),
		refs:         1, // the fetch itself
	}
}

// state snapshots what a reader needs, under one lock acquisition.
func (f *Fetch) state() (written int64, done bool, notify chan struct{}, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, f.done, f.notify, f.err
}

// Digest reports the committed digest. Only valid once the fetch is done and no
// error was recorded.
func (f *Fetch) Digest() (blob.Digest, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digest, f.size, f.err
}

// Wait blocks until the fetch finishes or ctx is cancelled.
func (f *Fetch) Wait(ctx context.Context) error {
	for {
		_, done, notify, err := f.state()
		if done {
			return err
		}
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// advance publishes progress to readers.
func (f *Fetch) advance(n int64) {
	f.mu.Lock()
	f.written += n
	f.broadcastLocked()
	f.mu.Unlock()
}

// broadcastLocked wakes every waiter. Caller holds mu.
func (f *Fetch) broadcastLocked() {
	close(f.notify)
	f.notify = make(chan struct{})
}

// finish records the terminal state exactly once and releases the fetch's own
// reference to the read descriptor.
func (f *Fetch) finish(d blob.Digest, size int64, err error) {
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		return
	}
	f.done, f.digest, f.size, f.err = true, d, size, err
	f.broadcastLocked()
	f.mu.Unlock()

	// Unblock anyone still waiting for headers we will now never produce.
	f.headersOnce.Do(func() { close(f.HeadersReady) })
	if f.onFinish != nil {
		f.onFinish()
	}
	f.release()
}

func (f *Fetch) publishHeaders(total int64, mediaType, staging string, rf *os.File) {
	f.mu.Lock()
	f.Total, f.MediaType, f.staging, f.readFile = total, mediaType, staging, rf
	f.mu.Unlock()
	f.headersOnce.Do(func() { close(f.HeadersReady) })
}

// acquire takes a reference to the shared read descriptor.
func (f *Fetch) acquire() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.readFile == nil {
		return false
	}
	f.refs++
	return true
}

func (f *Fetch) release() {
	f.mu.Lock()
	f.refs--
	shouldClose := f.refs <= 0 && !f.closed && f.readFile != nil
	if shouldClose {
		f.closed = true
	}
	rf := f.readFile
	f.mu.Unlock()
	if shouldClose {
		_ = rf.Close()
	}
}

// Reader returns a stream of the content as it arrives.
//
// Returns errCommitted when the fetch already finished successfully, in which case
// the caller serves the committed blob — which is both simpler and better, since
// http.ServeContent can then handle Range and conditional requests.
func (f *Fetch) Reader(ctx context.Context) (io.ReadCloser, error) {
	select {
	case <-f.HeadersReady:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	written, done, _, err := f.state()
	if done {
		if err != nil {
			return nil, err
		}
		return nil, errCommitted
	}
	if err != nil && written == 0 {
		return nil, err
	}
	if !f.acquire() {
		return nil, errCommitted
	}
	return &follower{fetch: f, ctx: ctx}, nil
}

// follower tail-follows the staging file.
type follower struct {
	fetch  *Fetch
	ctx    context.Context
	pos    int64
	closed bool
}

func (r *follower) Read(p []byte) (int, error) {
	for {
		written, done, notify, err := r.fetch.state()

		if r.pos < written {
			end := written - r.pos
			if int64(len(p)) < end {
				end = int64(len(p))
			}
			n, rerr := r.fetch.readFile.ReadAt(p[:end], r.pos)
			r.pos += int64(n)
			if n > 0 {
				return n, nil
			}
			// A short read with no bytes and no error would spin; treat it as a real
			// failure rather than looping.
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, fmt.Errorf("engine: read staging for %s: %w", r.fetch.Key, rerr)
			}
		}

		if done {
			if err != nil {
				// Bytes already delivered are correct; the stream simply ends short.
				// The client's own integrity check is what catches it, which is why
				// pip and docker verify digests independently of us.
				return 0, err
			}
			return 0, io.EOF
		}

		select {
		case <-notify:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

// Close detaches this reader from the shared fetch. The fetch itself continues.
func (r *follower) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.fetch.release()
	return nil
}

// Registry deduplicates concurrent fetches of the same key.
type Registry struct {
	mu sync.Mutex
	m  map[string]*Fetch
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: make(map[string]*Fetch)} }

// Start returns the in-flight fetch for key, creating one if there is none.
// The second return value reports whether this caller created it and must run it.
func (r *Registry) Start(key, eco string) (*Fetch, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.m[key]; ok {
		return f, false
	}
	f := newFetch(key, eco)
	f.onFinish = func() {
		r.mu.Lock()
		// Only remove ourselves: a later fetch of the same key may already have
		// replaced this entry.
		if cur, ok := r.m[key]; ok && cur == f {
			delete(r.m, key)
		}
		r.mu.Unlock()
	}
	r.m[key] = f
	return f, true
}

// Len reports how many fetches are in flight. Tests and metrics.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// Wait blocks until every fetch that is currently registered has finished. Once
// listeners stop accepting requests no new fetches can appear, making this the
// process-shutdown drain barrier.
func (r *Registry) Wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		fetches := make([]*Fetch, 0, len(r.m))
		for _, f := range r.m {
			fetches = append(fetches, f)
		}
		r.mu.Unlock()
		if len(fetches) == 0 {
			return nil
		}
		for _, f := range fetches {
			if err := f.Wait(ctx); err != nil {
				return err
			}
		}
	}
}

// runFetch streams an upstream response into the blob store, publishing progress as
// it goes. It runs on its own goroutine with a DETACHED context.
//
// Detachment is deliberate and load-bearing: the goroutine outlives the request that
// triggered it, so a client pressing Ctrl-C part-way through a 2.5 GB download does
// not abort the transfer that nine other clients are reading and that the cache is
// about to keep. The only bounds are the upstream request timeout and process
// shutdown.
func (e *Engine) runFetch(
	f *Fetch,
	req upstream.Request,
	want Expect,
	publish func(blob.Digest, int64),
	projectValue ...string,
) {
	project := ""
	if len(projectValue) > 0 {
		project = projectValue[0]
	}
	ctx := context.WithoutCancel(e.baseCtx)
	e.metrics.InflightFetches.WithLabelValues(f.Eco).Inc()
	started := time.Now()

	var (
		digest blob.Digest
		size   int64
		err    error
	)
	defer func() {
		e.metrics.InflightFetches.WithLabelValues(f.Eco).Dec()
		e.metrics.FetchDuration.WithLabelValues(f.Eco).Observe(time.Since(started).Seconds())
		if err == nil && publish != nil {
			// This is deliberately before finish: finish wakes waiters and removes
			// the fetch from its registry. Once either is observable, the catalog
			// entry must already be observable too.
			publish(digest, size)
		}
		f.finish(digest, size, err)
		if err != nil {
			e.events.Publish(obs.Event{
				Kind: obs.EventFetchError, Project: project,
				Eco: f.Eco, ID: f.Key, Detail: err.Error(),
			})
		} else {
			e.events.Publish(obs.Event{
				Kind: obs.EventFetchDone, Project: project,
				Eco: f.Eco, ID: f.Key, Size: size,
			})
		}
	}()

	digest, size, err = e.stream(ctx, f, req, want, project)
}

// stream is runFetch's body, split out so every exit path runs the deferred
// bookkeeping above.
func (e *Engine) stream(
	ctx context.Context,
	f *Fetch,
	req upstream.Request,
	want Expect,
	project string,
) (blob.Digest, int64, error) {
	resp, cancel, err := e.pool.Open(ctx, req)
	if err != nil {
		return "", 0, err
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", 0, &UpstreamHTTPError{URL: req.URL, Status: resp.StatusCode}
	}

	total := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil && n >= 0 {
			total = n
		}
	}
	mediaType := resp.Header.Get("Content-Type")

	// Refuse a transfer the project has no room for as soon as the size is known,
	// which for an artifact origin is here. Discovering it only at commit time meant
	// downloading the whole thing to throw it away — and doing that again on the next
	// request, because a rejected commit leaves no entry to find.
	if want.Room > 0 && total > want.Room {
		return "", 0, e.quotaExceeded(project, total)
	}

	w, err := e.blobs.Create()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = w.Abort() }() // no-op once Commit has run

	readFile, err := os.Open(w.StagingPath())
	if err != nil {
		return "", 0, fmt.Errorf("engine: open staging for reading: %w", err)
	}

	// Readers may attach from here on: the staging file exists and the size is known.
	f.publishHeaders(total, mediaType, w.StagingPath(), readFile)
	e.events.Publish(obs.Event{
		Kind: obs.EventFetchStart, Project: project,
		Eco: f.Eco, ID: f.Key, Total: total,
	})

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp

	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", 0, werr
			}
			// A chunked response declares no length, so the quota can only be applied
			// as the bytes arrive. Stopping here bounds the waste to one buffer.
			if want.Room > 0 && w.Written() > want.Room {
				return "", 0, e.quotaExceeded(project, w.Written())
			}
			// Only advertise bytes that are actually in the file, so a reader can
			// never be told to read past what has been written.
			f.advance(int64(n))
			e.events.Publish(obs.Event{
				Kind: obs.EventFetchProgress, Project: project,
				Eco: f.Eco, ID: f.Key, Size: w.Written(), Total: total,
			})
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", 0, fmt.Errorf("engine: reading %s: %w", req.URL, rerr)
		}
	}

	written := w.Written()
	got := w.Digest()

	// Verify BEFORE publishing. A digest or length mismatch means the staging file is
	// discarded and no catalog entry is written, so the next request retries cleanly
	// rather than serving poisoned content forever.
	if total >= 0 && written != total {
		return "", 0, fmt.Errorf("%w: %s declared %d bytes, delivered %d",
			ErrSizeMismatch, req.URL, total, written)
	}
	if want.Size > 0 && written != want.Size {
		return "", 0, fmt.Errorf("%w: expected %d bytes, got %d", ErrSizeMismatch, want.Size, written)
	}
	if want.Digest != "" && got != want.Digest {
		return "", 0, fmt.Errorf("%w: %s expected %s, got %s",
			ErrDigestMismatch, req.URL, want.Digest, got)
	}

	digest, size, err := w.Commit()
	if err != nil {
		return "", 0, err
	}
	e.pool.CountBytes(f.Eco, req.URL, size)
	return digest, size, nil
}
