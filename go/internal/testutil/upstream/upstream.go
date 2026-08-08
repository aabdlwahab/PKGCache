// Package upstream provides a synthetic origin server for tests.
//
// The engine's hard cases are all failure cases: an origin that truncates mid-body,
// one that stalls, one that returns the wrong bytes for a declared digest, one that
// demands a bearer token first. Those are impractical to provoke against a real
// registry and trivial here, so every one of them is a permanent test rather than a
// hope.
package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Behaviour describes how the origin should answer one path.
type Behaviour struct {
	// Body is the content served. Ignored when Status is an error code.
	Body []byte
	// Status defaults to 200.
	Status int
	// ContentType is sent as-is; empty means the server omits the header.
	ContentType string
	// ETag and LastModified drive conditional revalidation. When set, the server
	// answers a matching If-None-Match / If-Modified-Since with 304.
	ETag         string
	LastModified string

	// TruncateAfter cuts the body short after this many bytes while still
	// advertising the full Content-Length — the classic partial-transfer failure.
	TruncateAfter int
	// Corrupt serves a body of the right length but the wrong bytes, which is what
	// digest verification exists to catch.
	Corrupt bool
	// OmitContentLength forces a chunked response, so the fetch path cannot rely on
	// knowing the size in advance.
	OmitContentLength bool
	// DelayPerChunk slows the body so a test can observe progressive delivery, or
	// disconnect part-way through.
	DelayPerChunk time.Duration
	// ChunkSize controls how the body is written out. Zero means all at once.
	ChunkSize int
	// RequireBearer answers the first unauthenticated request with a 401 challenge
	// pointing at this server's own token endpoint.
	RequireBearer bool
	// FailTimes returns 500 for the first N requests, then succeeds. For retry and
	// single-flight tests.
	FailTimes int
}

// Server is a controllable origin.
type Server struct {
	*httptest.Server

	mu     sync.Mutex
	routes map[string]*Behaviour
	fails  map[string]int

	// Requests counts every request the origin received. The single-flight tests
	// assert on this: N concurrent clients must produce exactly one upstream fetch.
	Requests atomic.Int64
	// TokenRequests counts hits on the bearer token endpoint.
	TokenRequests atomic.Int64

	perPath sync.Map // path -> *atomic.Int64
}

// New starts a synthetic origin. The caller closes it.
func New() *Server {
	s := &Server{routes: map[string]*Behaviour{}, fails: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Handle registers a behaviour for a path.
func (s *Server) Handle(path string, b Behaviour) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = &b
	s.fails[path] = b.FailTimes
}

// Serve is the common case: a 200 with this body.
func (s *Server) Serve(path string, body []byte) {
	s.Handle(path, Behaviour{Body: body})
}

// URLFor returns the absolute URL for a path on this origin.
func (s *Server) URLFor(path string) string {
	return s.URL + path
}

// Hits reports how many requests a specific path received.
func (s *Server) Hits(path string) int64 {
	if v, ok := s.perPath.Load(path); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func (s *Server) countPath(path string) {
	v, _ := s.perPath.LoadOrStore(path, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

const tokenPath = "/__token"

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.Requests.Add(1)
	s.countPath(r.URL.Path)

	if r.URL.Path == tokenPath {
		s.TokenRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"token":"synthetic-token","expires_in":300}`)
		return
	}

	s.mu.Lock()
	b, ok := s.routes[r.URL.Path]
	remainingFails := s.fails[r.URL.Path]
	if ok && remainingFails > 0 {
		s.fails[r.URL.Path] = remainingFails - 1
	}
	s.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}
	if remainingFails > 0 {
		http.Error(w, "synthetic failure", http.StatusInternalServerError)
		return
	}

	if b.RequireBearer && r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s%s",service="synthetic",scope="repository:test:pull"`,
				s.URL, tokenPath))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if b.Status >= 400 {
		http.Error(w, http.StatusText(b.Status), b.Status)
		return
	}

	// Conditional revalidation: the whole point of a Ref's ETag.
	if b.ETag != "" {
		w.Header().Set("ETag", b.ETag)
		if match := r.Header.Get("If-None-Match"); match != "" && match == b.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if b.LastModified != "" {
		w.Header().Set("Last-Modified", b.LastModified)
		if since := r.Header.Get("If-Modified-Since"); since == b.LastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	body := b.Body
	if b.Corrupt {
		body = corrupt(body)
	}
	if b.ContentType != "" {
		w.Header().Set("Content-Type", b.ContentType)
	}
	if !b.OmitContentLength {
		// Deliberately the FULL length even when truncating, so the fetch path has to
		// notice the short body itself rather than being told.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	status := b.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}

	limit := len(body)
	if b.TruncateAfter > 0 && b.TruncateAfter < limit {
		limit = b.TruncateAfter
	}
	chunk := b.ChunkSize
	if chunk <= 0 {
		chunk = limit
	}
	flusher, _ := w.(http.Flusher)
	for off := 0; off < limit; off += chunk {
		end := min(off+chunk, limit)
		if _, err := w.Write(body[off:end]); err != nil {
			return // client hung up; nothing more to do
		}
		if flusher != nil {
			flusher.Flush()
		}
		if b.DelayPerChunk > 0 {
			time.Sleep(b.DelayPerChunk)
		}
	}
}

func corrupt(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	for i := range out {
		out[i] ^= 0xFF
	}
	return out
}

// Repeat builds a body of roughly n bytes from a repeating pattern, which keeps
// large-artifact tests cheap while still producing a distinctive digest.
func Repeat(pattern string, n int) []byte {
	if pattern == "" {
		pattern = "x"
	}
	return []byte(strings.Repeat(pattern, n/len(pattern)+1))[:n]
}
