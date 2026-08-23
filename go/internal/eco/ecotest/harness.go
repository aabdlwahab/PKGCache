// Package ecotest wires a real engine, blob store and catalog behind an ecosystem
// so adapter tests exercise the whole stack rather than a mock of it.
//
// Adapter bugs live at the seams — a rewritten URL that points at the wrong base, a
// cache key that does not match what the next request asks for, an artifact row that
// never gets written. A mocked engine hides exactly those.
package ecotest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/router"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// Harness is a live single-ecosystem server.
type Harness struct {
	T       *testing.T
	Eco     eco.Ecosystem
	Engine  *engine.Engine
	Blobs   *blob.Store
	Catalog *catalog.DB
	Origin  *testupstream.Server
	Config  *config.Store
	Server  *httptest.Server

	project string
	mux     *router.Mux
	desc    eco.Descriptor
}

// New starts a harness serving one ecosystem for the global project.
func New(t *testing.T, build func(*testupstream.Server) eco.Ecosystem) *Harness {
	t.Helper()
	dir := t.TempDir()

	blobs, err := blob.Open(dir)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	cat, err := catalog.Open(catalog.Options{Path: filepath.Join(dir, "catalog.db")})
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	snap := config.Defaults()
	snap.DataDir = dir
	snap.Upstream.RequestTimeout = 30 * time.Second
	snap.Upstream.ConnectTimeout = 5 * time.Second
	cfg := config.NewStore(&snap)

	origin := testupstream.New()
	t.Cleanup(origin.Close)

	m := obs.NewMetrics()
	pool, err := upstream.New(snap.Upstream, m)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.Options{
		Blobs:   blobs,
		Catalog: cat,
		Pool:    pool,
		Config:  cfg,
		Metrics: m,
		Events:  obs.NewBus(),
	})

	h := &Harness{
		T: t, Engine: e, Blobs: blobs, Catalog: cat,
		Origin: origin, Config: cfg, project: config.GlobalProject,
	}
	h.Eco = build(origin)
	h.desc = h.Eco.Descriptor()
	h.mux = mount(h.Eco)

	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Server.Close)
	return h
}

// mount registers an ecosystem's routes, admin ones first so a greedy protocol
// catch-all cannot shadow them. This mirrors what the real project router does.
func mount(e eco.Ecosystem) *router.Mux {
	m := router.New()
	for _, r := range e.Routes() {
		if r.Admin {
			m.Handle(r.Methods, r.Pattern, r.Handler)
		}
	}
	for _, r := range e.Routes() {
		if !r.Admin {
			m.Handle(r.Methods, r.Pattern, r.Handler)
		}
	}
	return m
}

func (h *Harness) serve(w http.ResponseWriter, r *http.Request) {
	root := "/" + h.project + "/" + h.desc.ID
	// Unit tests may address the mounted adapter directly, while real-client
	// acceptance tests follow rewritten URLs carrying /<project>/<eco>. Accept both
	// and strip the real routing prefix exactly as the production project router
	// does. Without this, a rewritten npm tarball URL is correct in isolation but
	// cannot be followed inside the harness that produced it.
	escaped := r.URL.EscapedPath()
	if escaped == root || strings.HasPrefix(escaped, root+"/") {
		stripped := strings.TrimPrefix(escaped, root)
		if stripped == "" {
			stripped = "/"
		}
		cloned := r.Clone(r.Context())
		clonedURL := *r.URL
		decoded, err := url.PathUnescape(stripped)
		if err == nil {
			clonedURL.Path = decoded
			if decoded != stripped {
				clonedURL.RawPath = stripped
			} else {
				clonedURL.RawPath = ""
			}
			cloned.URL = &clonedURL
			r = cloned
		}
	}
	base := eco.NewCtx(w, r, h.project, root,
		router.Params{}, h.Engine, h.Config.Current(), h.desc)
	h.mux.ServeHTTP(w, eco.Bind(r, base))
}

// SetProject changes the tenant subsequent requests are served as.
func (h *Harness) SetProject(name string) { h.project = name }

// URL builds an absolute URL against the harness server.
func (h *Harness) URL(path string) string { return h.Server.URL + path }

// Offline flips the instance-wide hard mode.
func (h *Harness) Offline(v bool) {
	h.T.Helper()
	if err := h.Config.Apply(func(s *config.Snapshot) error {
		s.Upstream.Offline = v
		return nil
	}); err != nil {
		h.T.Fatalf("set offline: %v", err)
	}
}

// Do issues a request and returns the response with its body already read.
func (h *Harness) Do(req *http.Request) *Response {
	h.T.Helper()
	resp, err := h.Server.Client().Do(req)
	if err != nil {
		h.T.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readAll(h.T, resp)
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}
}

// Get issues a GET.
func (h *Harness) Get(path string) *Response {
	h.T.Helper()
	req, err := http.NewRequestWithContext(h.T.Context(), http.MethodGet, h.URL(path), http.NoBody)
	if err != nil {
		h.T.Fatalf("build request: %v", err)
	}
	return h.Do(req)
}

// Flush persists queued catalog writes so a test can assert on stored state.
func (h *Harness) Flush() {
	h.T.Helper()
	if err := h.Catalog.Flush(); err != nil {
		h.T.Fatalf("catalog flush: %v", err)
	}
}

// Response is a completed HTTP exchange.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Text returns the body as a string.
func (r *Response) Text() string { return string(r.Body) }

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}
