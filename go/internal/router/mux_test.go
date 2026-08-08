package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// handle registers a route that records which pattern matched and what it captured.
func handle(m *Mux, methods []string, pattern string, matched *string, captured *Params) {
	m.Handle(methods, pattern, func(_ http.ResponseWriter, _ *http.Request, p Params) {
		*matched = pattern
		*captured = p
	})
}

// ---- Spike S3 ------------------------------------------------------------
//
// The three route shapes that http.ServeMux cannot serve. Each is a real client
// behaviour, not a hypothetical.

// npm sends a scoped package as one segment with an encoded slash. The name must
// reach the handler intact so the adapter can decode it itself.
func TestS3NpmEncodedSlashInScopedPackage(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/{name}/-/{filename}", &matched, &p)
	handle(m, []string{"GET"}, "/{scope}/{pkg}", &matched, &p)
	handle(m, []string{"GET"}, "/{name}", &matched, &p)

	cases := []struct {
		target      string
		wantPattern string
		wantRaw     string
		wantDecoded string
	}{
		// The encoded form stays ONE segment and keeps its %2F.
		{"/@babel%2Fcore", "/{name}", "@babel%2Fcore", "@babel/core"},
		{"/@types%2Fnode", "/{name}", "@types%2Fnode", "@types/node"},
		// The unencoded form is two segments, which is a different route.
		{"/@babel/core", "/{scope}/{pkg}", "@babel", "@babel"},
		// A plain name is unaffected.
		{"/chalk", "/{name}", "chalk", "chalk"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			matched, p = "", Params{}
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			m.ServeHTTP(httptest.NewRecorder(), req)

			if matched != c.wantPattern {
				t.Fatalf("matched %q, want %q", matched, c.wantPattern)
			}
			first := p.Get(firstName(p))
			if first != c.wantRaw {
				t.Fatalf("raw capture = %q, want %q", first, c.wantRaw)
			}
			if got := p.Unescape(firstName(p)); got != c.wantDecoded {
				t.Fatalf("decoded capture = %q, want %q", got, c.wantDecoded)
			}
		})
	}
}

// The two ServeMux limitations that force a custom router, asserted against the
// live standard library rather than asserted in a comment. If a future Go release
// removes either, this test fails and the decision gets revisited.
//
// A third limitation was expected and does NOT hold: modern ServeMux handles npm's
// "/@babel%2Fcore" correctly. That is recorded here too, so the record stays honest.
func TestServeMuxLimitations(t *testing.T) {
	t.Run("cannot express a non-terminal greedy wildcard", func(t *testing.T) {
		// OCI's /v2/{name...}/manifests/{ref} has no ServeMux equivalent.
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("ServeMux accepted a non-terminal {...} wildcard — " +
					"re-evaluate whether the custom router is still needed")
			}
			t.Logf("ServeMux rejects it as expected: %v", r)
		}()
		http.NewServeMux().HandleFunc("GET /v2/{name...}/manifests/{ref}",
			func(http.ResponseWriter, *http.Request) {})
	})

	t.Run("cleans paths and redirects", func(t *testing.T) {
		// A forward proxy must relay what the client asked for, byte for byte.
		std := http.NewServeMux()
		std.HandleFunc("GET /{path...}", func(http.ResponseWriter, *http.Request) {})
		for _, target := range []string{"/a/../b", "/a//b", "/x/./y"} {
			rec := httptest.NewRecorder()
			std.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			if rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently {
				t.Errorf("ServeMux served %s directly (code %d) — "+
					"re-evaluate the no-cleaning requirement", target, rec.Code)
			}
		}
	})

	t.Run("encoded slashes are handled correctly", func(t *testing.T) {
		// Recorded because it contradicts a claim made early in the design: modern
		// ServeMux keeps %2F inside one segment and decodes it in PathValue.
		std := http.NewServeMux()
		var got string
		std.HandleFunc("GET /{name}", func(_ http.ResponseWriter, r *http.Request) {
			got = "one:" + r.PathValue("name")
		})
		std.HandleFunc("GET /{scope}/{pkg}", func(_ http.ResponseWriter, r *http.Request) {
			got = "two:" + r.PathValue("scope") + "/" + r.PathValue("pkg")
		})
		rec := httptest.NewRecorder()
		std.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/@babel%2Fcore", nil))
		if rec.Code != http.StatusOK || got != "one:@babel/core" {
			t.Fatalf("ServeMux %%2F behaviour changed: code=%d matched=%q", rec.Code, got)
		}
	})

	t.Run("PathValue cannot distinguish an encoded from a literal character", func(t *testing.T) {
		// This is why our Params hands back the raw segment: an apt proxy has to
		// reconstruct the upstream URL exactly as the client wrote it.
		std := http.NewServeMux()
		var seen []string
		std.HandleFunc("GET /f/{file}", func(_ http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.PathValue("file"))
		})
		for _, target := range []string{"/f/a%2Bb", "/f/a+b"} {
			std.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		}
		if len(seen) != 2 || seen[0] != seen[1] {
			t.Fatalf("expected both forms to collapse to one value, got %q", seen)
		}
		t.Logf("both %%2B and + arrive as %q — indistinguishable", seen[0])
	})
}

// OCI image names contain slashes and the discriminator is a literal SUFFIX, so the
// greedy wildcard cannot be terminal.
func TestS3OCIGreedyNonTerminalWildcard(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/v2/", &matched, &p)
	handle(m, []string{"GET"}, "/v2/{name...}/manifests/{ref}", &matched, &p)
	handle(m, []string{"GET"}, "/v2/{name...}/blobs/{digest}", &matched, &p)
	handle(m, []string{"GET"}, "/v2/{name...}/tags/list", &matched, &p)

	cases := []struct {
		target, wantPattern, wantName, wantLast string
	}{
		{"/v2/alpine/manifests/3.20", "/v2/{name...}/manifests/{ref}", "alpine", "3.20"},
		{"/v2/library/alpine/manifests/3.20", "/v2/{name...}/manifests/{ref}", "library/alpine", "3.20"},
		{"/v2/a/b/c/d/manifests/latest", "/v2/{name...}/manifests/{ref}", "a/b/c/d", "latest"},
		{
			"/v2/library/alpine/blobs/sha256:abc",
			"/v2/{name...}/blobs/{digest}", "library/alpine", "sha256:abc",
		},
		// The literal suffix is anchored at the END, so an image legitimately named
		// ".../manifests" still resolves correctly.
		{"/v2/org/manifests/manifests/v1", "/v2/{name...}/manifests/{ref}", "org/manifests", "v1"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			matched, p = "", Params{}
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.target, nil))
			if matched != c.wantPattern {
				t.Fatalf("matched %q, want %q", matched, c.wantPattern)
			}
			if got := p.Get("name"); got != c.wantName {
				t.Fatalf("name = %q, want %q", got, c.wantName)
			}
			last := p.Get("ref")
			if last == "" {
				last = p.Get("digest")
			}
			if last != c.wantLast {
				t.Fatalf("trailing capture = %q, want %q", last, c.wantLast)
			}
		})
	}

	t.Run("tags/list has no trailing capture", func(t *testing.T) {
		matched, p = "", Params{}
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2/library/alpine/tags/list", nil))
		if matched != "/v2/{name...}/tags/list" {
			t.Fatalf("matched %q", matched)
		}
		if got := p.Get("name"); got != "library/alpine" {
			t.Fatalf("name = %q", got)
		}
	})
}

// The apt forward proxy receives absolute-form request targets. Go parses them into
// r.URL with a Host, and the path must survive untouched.
func TestS3AptAbsoluteFormTarget(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/{path...}", &matched, &p)

	req := httptest.NewRequest(http.MethodGet,
		"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease", nil)
	if req.URL.Host != "archive.ubuntu.com" {
		t.Fatalf("Host = %q; absolute-form parsing changed", req.URL.Host)
	}
	m.ServeHTTP(httptest.NewRecorder(), req)

	if matched != "/{path...}" {
		t.Fatalf("matched %q", matched)
	}
	if got := p.Get("path"); got != "ubuntu/dists/noble/InRelease" {
		t.Fatalf("path = %q", got)
	}
}

// PyPI filenames carry "+" and encoded characters that must not be mangled.
func TestS3PypiFilenameEncoding(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/{index...}/+f/{project}/{filename}", &matched, &p)

	cases := []struct{ target, wantIndex, wantFile, wantDecoded string }{
		{
			"/root/pytorch-cu124/+f/torch/torch-2.6.0%2Bcu124-cp313-cp313-linux_x86_64.whl",
			"root/pytorch-cu124",
			"torch-2.6.0%2Bcu124-cp313-cp313-linux_x86_64.whl",
			"torch-2.6.0+cu124-cp313-cp313-linux_x86_64.whl",
		},
		{
			"/root/pypi/+f/numpy/numpy-2.0.0-cp312-cp312-manylinux_2_17_x86_64.whl",
			"root/pypi",
			"numpy-2.0.0-cp312-cp312-manylinux_2_17_x86_64.whl",
			"numpy-2.0.0-cp312-cp312-manylinux_2_17_x86_64.whl",
		},
	}
	for _, c := range cases {
		t.Run(c.wantFile, func(t *testing.T) {
			matched, p = "", Params{}
			m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.target, nil))
			if matched == "" {
				t.Fatal("no route matched")
			}
			if got := p.Get("index"); got != c.wantIndex {
				t.Fatalf("index = %q, want %q", got, c.wantIndex)
			}
			if got := p.Get("filename"); got != c.wantFile {
				t.Fatalf("raw filename = %q, want %q", got, c.wantFile)
			}
			if got := p.Unescape("filename"); got != c.wantDecoded {
				t.Fatalf("decoded filename = %q, want %q", got, c.wantDecoded)
			}
		})
	}
}

// ---- general behaviour ----------------------------------------------------

// Registration order decides, so an administrative route can win over a catch-all.
func TestRegistrationOrderWins(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/+indexes", &matched, &p)
	handle(m, []string{"GET"}, "/{index...}/+simple/{project}/", &matched, &p)
	handle(m, []string{"GET"}, "/{path...}", &matched, &p)

	for target, want := range map[string]string{
		"/+indexes":                 "/+indexes",
		"/root/pypi/+simple/numpy/": "/{index...}/+simple/{project}/",
		"/anything/else":            "/{path...}",
	} {
		matched, p = "", Params{}
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		if matched != want {
			t.Errorf("%s matched %q, want %q", target, matched, want)
		}
	}
}

// A trailing slash is a different resource; PyPI's simple index depends on it.
func TestTrailingSlashIsSignificant(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/simple/{project}/", &matched, &p)
	handle(m, []string{"GET"}, "/simple/{project}", &matched, &p)

	matched = ""
	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/simple/numpy/", nil))
	if matched != "/simple/{project}/" {
		t.Fatalf("with slash matched %q", matched)
	}
	matched = ""
	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/simple/numpy", nil))
	if matched != "/simple/{project}" {
		t.Fatalf("without slash matched %q", matched)
	}
}

// A terminal greedy must serve both a directory and a file, so it keeps the slash
// rather than requiring the pattern to declare one.
func TestTerminalGreedyKeepsTrailingSlash(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/{path...}", &matched, &p)

	for target, want := range map[string]string{
		"/":                     "",
		"/builds/":              "builds/",
		"/builds/v1/app.tar.gz": "builds/v1/app.tar.gz",
	} {
		matched, p = "", Params{}
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		if matched == "" {
			t.Fatalf("%s did not match", target)
		}
		if got := p.Get("path"); got != want {
			t.Errorf("%s -> path %q, want %q", target, got, want)
		}
	}
}

// No path cleaning: ".." is an ordinary character in a package filename, and the
// safety boundary is the catalog's exact-match keys, not a normalisation pass.
func TestNoPathCleaningOrRedirect(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/{path...}", &matched, &p)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a/../b", nil))
	if rec.Code == http.StatusMovedPermanently {
		t.Fatal("the router redirected; it must never rewrite a path")
	}
	if got := p.Get("path"); got != "a/../b" {
		t.Fatalf("path = %q, want it verbatim", got)
	}
}

func TestMethodMatching(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"PUT", "DELETE"}, "/{path...}", &matched, &p)
	handle(m, []string{"GET"}, "/{path...}", &matched, &p)

	for _, c := range []struct{ method, want string }{
		{http.MethodGet, "/{path...}"},
		{http.MethodHead, "/{path...}"}, // HEAD falls through to the GET handler
		{http.MethodPut, "/{path...}"},
		{http.MethodDelete, "/{path...}"},
	} {
		matched = ""
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, "/x", nil))
		if matched != c.want {
			t.Errorf("%s matched %q", c.method, matched)
		}
	}

	if _, _, ok := m.Lookup(http.MethodPatch, "/x"); ok {
		t.Fatal("PATCH should not match")
	}
}

func TestNotFound(t *testing.T) {
	m := New()
	m.Handle([]string{"GET"}, "/known", func(http.ResponseWriter, *http.Request, Params) {})

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}

	called := false
	m.NotFound = func(w http.ResponseWriter, _ *http.Request, _ Params) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if !called || rec.Code != http.StatusTeapot {
		t.Fatalf("custom NotFound not used: called=%v code=%d", called, rec.Code)
	}
}

func TestEmptySegmentDoesNotMatchCapture(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/a/{x}/b", &matched, &p)

	matched = ""
	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a//b", nil))
	if matched != "" {
		t.Fatal("an empty segment must not satisfy a capture")
	}
}

// A malformed pattern is a programming error found at startup, not a runtime 404.
func TestBadPatternsPanic(t *testing.T) {
	for _, pattern := range []string{
		"no-leading-slash",
		"/{a...}/x/{b...}", // two greedy wildcards need backtracking
		"/{}",
		"/{unclosed",
	} {
		t.Run(pattern, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("pattern %q should have panicked", pattern)
				}
			}()
			New().Handle([]string{"GET"}, pattern, func(http.ResponseWriter, *http.Request, Params) {})
		})
	}
}

func TestParamsHelpers(t *testing.T) {
	p := Params{names: []string{"a", "bad"}, values: []string{"x%2Fy", "%zz"}}
	if !p.Has("a") || p.Has("missing") {
		t.Fatal("Has is wrong")
	}
	if p.Get("a") != "x%2Fy" {
		t.Fatalf("Get = %q", p.Get("a"))
	}
	if p.Unescape("a") != "x/y" {
		t.Fatalf("Unescape = %q", p.Unescape("a"))
	}
	// A malformed escape falls back to raw rather than erroring: the adapter's 404 is
	// a better answer to a broken client than a 500.
	if p.Unescape("bad") != "%zz" {
		t.Fatalf("malformed Unescape = %q", p.Unescape("bad"))
	}
	if p.Get("missing") != "" {
		t.Fatal("missing capture should be empty")
	}
}

func TestRootPath(t *testing.T) {
	var matched string
	var p Params
	m := New()
	handle(m, []string{"GET"}, "/", &matched, &p)

	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if matched != "/" {
		t.Fatalf("root did not match: %q", matched)
	}
}

func firstName(p Params) string {
	if len(p.names) == 0 {
		return ""
	}
	return p.names[0]
}

func BenchmarkLookup(b *testing.B) {
	m := New()
	noop := func(http.ResponseWriter, *http.Request, Params) {}
	m.Handle([]string{"GET"}, "/v2/", noop)
	m.Handle([]string{"GET"}, "/v2/{name...}/manifests/{ref}", noop)
	m.Handle([]string{"GET"}, "/v2/{name...}/blobs/{digest}", noop)
	m.Handle([]string{"GET"}, "/{index...}/+f/{project}/{filename}", noop)
	m.Handle([]string{"GET"}, "/{path...}", noop)

	b.ResetTimer()
	for range b.N {
		if _, _, ok := m.Lookup("GET", "/v2/library/alpine/blobs/sha256:abcdef"); !ok {
			b.Fatal("no match")
		}
	}
}

// §8 fuzz target: path resolution. The matcher runs on the escaped path, before any
// decoding, so it is the first thing a hostile URL reaches. It must never panic, and a
// capture must never claim text the path did not contain.
func FuzzPathResolution(f *testing.F) {
	for _, seed := range []string{
		"/",
		"/v2/library/alpine/manifests/3.20",
		"/global/npm/@babel%2Fcore",
		"/global/pypi/root/pypi/+simple/numpy/",
		"/a//b",
		"/../../etc/passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/global/files/dir/",
		"/global/files/name+with+plus",
		"/global/files/name%2Bwith%2Bescape",
		"//////",
		"/global/git/example.test/org/repo.git/info/refs",
	} {
		f.Add(seed)
	}

	m := New()
	noop := func(http.ResponseWriter, *http.Request, Params) {}
	m.Handle([]string{"GET"}, "/v2/", noop)
	m.Handle([]string{"GET"}, "/v2/{name...}/manifests/{ref}", noop)
	m.Handle([]string{"GET"}, "/v2/{name...}/blobs/{digest}", noop)
	m.Handle([]string{"GET"}, "/{index...}/+simple/{project}/", noop)
	m.Handle([]string{"GET"}, "/{path...}", noop)

	f.Fuzz(func(t *testing.T, path string) {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		_, params, ok := m.Lookup("GET", path)
		if !ok {
			return
		}
		// Every capture has to be a substring of the path it came from. A matcher that
		// synthesised or dropped bytes is how a traversal slips past a later check.
		for _, name := range params.names {
			value := params.Get(name)
			if value == "" {
				continue
			}
			if !strings.Contains(path, strings.TrimSuffix(value, "/")) {
				t.Fatalf("capture %q = %q is not present in %q", name, value, path)
			}
		}
		// Unescaping must not panic and must not lengthen the value.
		for _, name := range params.names {
			if decoded := params.Unescape(name); len(decoded) > len(params.Get(name)) {
				t.Fatalf("unescaping %q grew the value: %q -> %q",
					name, params.Get(name), decoded)
			}
		}
	})
}

// Percent-encoding is not meaningful inside a path segment, so a literal route segment
// must match either spelling. pip derives a PEP 658 ".metadata" URL from the index link
// the cache emitted with a literal "+", re-encodes it to "%2B", and used to get a 404
// that failed the whole install.
func TestLiteralSegmentMatchesPercentEncodedSpelling(t *testing.T) {
	m := New()
	var captured Params
	var matched string
	handle(m, []string{"GET"}, "/{index...}/+f/{project}/{filename}", &matched, &captured)
	handle(m, []string{"GET"}, "/v2/{name...}/manifests/{ref}", &matched, &captured)

	for _, c := range []struct{ name, path, wantFile string }{
		{"literal plus", "/root/pypi/+f/six/six-1.17.0.whl.metadata", "six-1.17.0.whl.metadata"},
		{"encoded plus", "/root/pypi/%2Bf/six/six-1.17.0.whl.metadata", "six-1.17.0.whl.metadata"},
		{"upper-case escape", "/root/pypi/%2bf/six/six-1.17.0.whl.metadata", "six-1.17.0.whl.metadata"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, params, ok := m.Lookup("GET", c.path)
			if !ok {
				t.Fatalf("no route matched %q", c.path)
			}
			if got := params.Get("filename"); got != c.wantFile {
				t.Fatalf("filename = %q, want %q", got, c.wantFile)
			}
		})
	}

	// The decoding must not turn an escaped separator into a real one: "%2F" stays
	// inside its segment, so a scoped npm name is still one segment and cannot be
	// smuggled into matching a multi-segment route.
	if _, _, ok := m.Lookup("GET", "/v2/library%2Falpine/manifests/3.20"); !ok {
		t.Fatal("an escaped slash inside a capture stopped matching")
	}
	if _, params, ok := m.Lookup("GET", "/v2/library%2Falpine/manifests/3.20"); ok {
		if got := params.Get("name"); got != "library%2Falpine" {
			t.Fatalf("capture = %q, want the escaped form preserved", got)
		}
	}
}
