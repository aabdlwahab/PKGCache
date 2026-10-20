package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
)

// Uploads that arrive over more than one request.
//
// The OCI distribution API puts a layer up in pieces: a POST opens an upload, PATCHes
// carry its bytes, and a PUT names the digest and closes it. Nothing ties those requests
// together but an id this side hands out, so the staged bytes live in a blob writer kept
// between them — hashed as they arrive, exactly as a one-request Put stages them — and the
// commit is Put's own. An upload nobody has touched for uploadIdle is abandoned, and its
// staging file removed, when the next one starts: a push that died halfway would otherwise
// keep its partial layer on disk until the process exits.

// ErrUploadUnknown means no open upload has this id in this project.
var ErrUploadUnknown = errors.New("engine: no such upload")

// uploadIdle is how long an upload may go untouched before it is abandoned. Every client in
// use sends a layer in one PATCH, so the gap between an upload's requests is seconds; an
// hour is a push that is not coming back.
const uploadIdle = time.Hour

type uploadTable struct {
	mu   sync.Mutex
	open map[string]*upload
}

type upload struct {
	mu           sync.Mutex
	project, eco string
	w            *blob.Writer
	touched      time.Time
	// done is set once the upload has left the table, so a request that found it just
	// before then does not write into a writer that is already committed or aborted.
	done bool
}

// StartUpload opens an upload in a project and returns its id.
func (e *Engine) StartUpload(project, eco string) (string, error) {
	if e.offline(project) {
		return "", ErrReadOnly
	}
	now := time.Now()
	e.uploads.sweep(now)
	w, err := e.blobs.Create()
	if err != nil {
		return "", err
	}
	id, err := uploadID()
	if err != nil {
		_ = w.Abort()
		return "", err
	}
	e.uploads.add(id, &upload{project: project, eco: eco, w: w, touched: now})
	return id, nil
}

// AppendUpload adds a body to an open upload and reports how much the upload now holds.
// maxBytes bounds the whole upload rather than this piece; zero means no bound.
//
// A body that fails partway abandons the upload. Its writer holds however much of the
// piece arrived, and appending the next piece after it would stage bytes that are neither
// the old upload nor the new one; the client starts that layer again instead.
func (e *Engine) AppendUpload(project, eco, id string, body io.Reader, maxBytes int64) (int64, error) {
	u, err := e.uploads.find(project, eco, id, false)
	if err != nil {
		return 0, err
	}
	defer u.mu.Unlock()
	size, err := u.append(body, maxBytes)
	if err != nil {
		e.uploads.drop(id, u)
	}
	return size, err
}

// UploadSize reports how much an open upload holds.
func (e *Engine) UploadSize(project, eco, id string) (int64, bool) {
	u, err := e.uploads.find(project, eco, id, false)
	if err != nil {
		return 0, false
	}
	defer u.mu.Unlock()
	return u.w.Written(), true
}

// CancelUpload abandons an open upload and removes what it staged.
func (e *Engine) CancelUpload(project, eco, id string) {
	if u, err := e.uploads.find(project, eco, id, false); err == nil {
		e.uploads.drop(id, u)
		u.mu.Unlock()
	}
}

// CommitUpload adds a last body to an open upload and stores the whole of it under key,
// as Put stores a body that arrived in one request: verified against o.ExpectDigest before
// any entry is written, and deduplicated against everything the store already holds. The
// upload is closed whatever the outcome; a client whose digest did not match starts again.
func (e *Engine) CommitUpload(
	project, eco, id, key string, body io.Reader, o PutOptions,
) (PutResult, error) {
	u, err := e.uploads.find(project, eco, id, true)
	if err != nil {
		return PutResult{}, err
	}
	defer u.mu.Unlock()
	defer func() { _ = u.w.Abort() }() // no-op after Commit
	if body != nil {
		if _, err := u.append(body, o.MaxBytes); err != nil {
			return PutResult{}, err
		}
	}
	if e.offline(project) {
		return PutResult{}, ErrReadOnly
	}
	entryKey := catalog.EntryKey{Project: project, Eco: eco, Key: key}
	existing, existErr := e.cat.GetEntry(entryKey)
	exists := existErr == nil
	if exists && !o.Overwrite {
		return PutResult{}, fmt.Errorf("%w: %s", ErrExists, key)
	}
	return e.finishPut(u.w, entryKey, exists, existing, o)
}

// Link files a blob the store already holds under another key, copying nothing.
func (e *Engine) Link(project, eco, key string, digest blob.Digest, mediaType string) error {
	if e.offline(project) {
		return ErrReadOnly
	}
	return e.link(catalog.EntryKey{Project: project, Eco: eco, Key: key},
		Resolution{Project: project, Eco: eco, MediaType: mediaType}, digest, 0, time.Now())
}

// ReadEntry returns the bytes of a small entry — a manifest, an index. maxBytes bounds
// it, so a key that holds a layer is refused rather than read into memory.
func (e *Engine) ReadEntry(project, eco, key string, maxBytes int64) ([]byte, catalog.Entry, error) {
	entry, err := e.cat.GetEntry(catalog.EntryKey{Project: project, Eco: eco, Key: key})
	if err != nil {
		return nil, catalog.Entry{}, err
	}
	if maxBytes > 0 && entry.Size > maxBytes {
		return nil, entry, fmt.Errorf("%w: %s is %d bytes", ErrTooLarge, key, entry.Size)
	}
	file, _, err := e.blobs.Open(entry.Digest)
	if err != nil {
		return nil, entry, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, entry.Size))
	if err != nil {
		return nil, entry, fmt.Errorf("engine: read %s: %w", key, err)
	}
	return body, entry, nil
}

func (u *upload) append(body io.Reader, maxBytes int64) (int64, error) {
	src := body
	if maxBytes > 0 {
		// One byte past what is left, so going over is seen rather than truncated.
		src = io.LimitReader(body, maxBytes-u.w.Written()+1)
	}
	_, err := io.Copy(u.w, src)
	u.touched = time.Now()
	if err != nil {
		return u.w.Written(), fmt.Errorf("engine: reading upload: %w", err)
	}
	if maxBytes > 0 && u.w.Written() > maxBytes {
		return u.w.Written(), fmt.Errorf("%w: over %d bytes", ErrTooLarge, maxBytes)
	}
	return u.w.Written(), nil
}

func (t *uploadTable) add(id string, u *upload) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = map[string]*upload{}
	}
	t.open[id] = u
}

// find returns the upload with its lock held. With claim it also leaves the table, so it
// is this caller's alone to commit.
func (t *uploadTable) find(project, eco, id string, claim bool) (*upload, error) {
	t.mu.Lock()
	u, ok := t.open[id]
	if !ok || u.project != project || u.eco != eco {
		t.mu.Unlock()
		return nil, ErrUploadUnknown
	}
	if claim {
		delete(t.open, id)
	}
	t.mu.Unlock()
	u.mu.Lock()
	if u.done {
		u.mu.Unlock()
		return nil, ErrUploadUnknown
	}
	if claim {
		u.done = true
	}
	return u, nil
}

// drop removes an upload whose lock the caller holds, and its staged bytes.
func (t *uploadTable) drop(id string, u *upload) {
	t.mu.Lock()
	if t.open[id] == u {
		delete(t.open, id)
	}
	t.mu.Unlock()
	u.done = true
	_ = u.w.Abort()
}

// sweep abandons the uploads nobody has touched for uploadIdle. One a request is using
// right now is not idle, whatever its clock says, and is left for the next sweep.
func (t *uploadTable) sweep(now time.Time) {
	t.mu.Lock()
	var stale []*upload
	for id, u := range t.open {
		if !u.mu.TryLock() {
			continue
		}
		if now.Sub(u.touched) < uploadIdle {
			u.mu.Unlock()
			continue
		}
		delete(t.open, id)
		u.done = true
		stale = append(stale, u)
	}
	t.mu.Unlock()
	for _, u := range stale {
		_ = u.w.Abort()
		u.mu.Unlock()
	}
}

func uploadID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("engine: upload id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
