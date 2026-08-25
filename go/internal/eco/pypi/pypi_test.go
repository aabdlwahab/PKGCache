package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func pypiHarness(t *testing.T, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return NewWithIndexes(map[string]string{"root/pypi": origin.URL})
	})
}

func digestString(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func requestJSON(h *ecotest.Harness, path string) *ecotest.Response {
	h.T.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.URL(path), nil)
	req.Header.Set("Accept", jsonMediaType)
	return h.Do(req)
}

func TestPEP691JSONParseRewriteAndNormalize(t *testing.T) {
	const wheel = "Demo_Pkg-1.0-py3-none-any.whl"
	h := pypiHarness(t, func(origin *testupstream.Server) {
		doc := map[string]any{
			"meta": map[string]any{"api-version": "1.1"},
			"name": "demo-pkg",
			"files": []any{map[string]any{
				"filename":           wheel,
				"url":                origin.URLFor("/packages/"+wheel) + "#sha256=abc123",
				"hashes":             map[string]string{},
				"requires-python":    ">=3.9",
				"yanked":             "bad build",
				"dist-info-metadata": "sha256=metadata123",
			}},
		}
		body, _ := json.Marshal(doc)
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: body, ContentType: jsonMediaType,
		})
	})

	resp := requestJSON(h, "/root/pypi/+simple/Demo_Pkg/")
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Status, resp.Text())
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, jsonMediaType) {
		t.Fatalf("content type = %q", ct)
	}
	var got struct {
		Name  string `json:"name"`
		Files []struct {
			URL          string            `json:"url"`
			Hashes       map[string]string `json:"hashes"`
			CoreMetadata any               `json:"core-metadata"`
			Yanked       any               `json:"yanked"`
		} `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "demo-pkg" || len(got.Files) != 1 {
		t.Fatalf("document = %+v", got)
	}
	if !strings.Contains(got.Files[0].URL,
		"/global/pypi/root/pypi/+f/demo-pkg/"+wheel) {
		t.Fatalf("file URL bypasses the cache: %q", got.Files[0].URL)
	}
	if got.Files[0].Hashes["sha256"] != "abc123" {
		t.Fatalf("fragment hash was not normalized: %+v", got.Files[0].Hashes)
	}
	core, ok := got.Files[0].CoreMetadata.(map[string]any)
	if !ok || core["sha256"] != "metadata123" {
		t.Fatalf("core-metadata was not normalized: %#v", got.Files[0].CoreMetadata)
	}
	if got.Files[0].Yanked != "bad build" {
		t.Fatalf("yanked reason = %#v", got.Files[0].Yanked)
	}
}

func TestPEP503HTMLParseAndJSONRender(t *testing.T) {
	const wheel = "demo_pkg-2.0-py3-none-any.whl"
	h := pypiHarness(t, func(origin *testupstream.Server) {
		body := `<html><body>
<a href="../packages/` + wheel + `#sha256=deadbeef"
 data-requires-python="&gt;=3.10" data-yanked="broken"
 data-core-metadata="sha256=feedface"><strong>` + wheel + `</strong></a>
</body></html>`
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: []byte(body), ContentType: "text/html; charset=UTF-8",
		})
	})

	resp := requestJSON(h, "/root/pypi/+simple/demo-pkg/")
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Status, resp.Text())
	}
	var got struct {
		Files []struct {
			Filename       string         `json:"filename"`
			RequiresPython string         `json:"requires-python"`
			Yanked         any            `json:"yanked"`
			CoreMetadata   map[string]any `json:"core-metadata"`
		} `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Filename != wheel {
		t.Fatalf("files = %+v", got.Files)
	}
	if got.Files[0].RequiresPython != ">=3.10" ||
		got.Files[0].Yanked != "broken" ||
		got.Files[0].CoreMetadata["sha256"] != "feedface" {
		t.Fatalf("PEP attributes were not preserved: %+v", got.Files[0])
	}
}

func TestHTMLRenderPreservesInstallerAttributes(t *testing.T) {
	const wheel = "demo_pkg-2.0-py3-none-any.whl"
	h := pypiHarness(t, func(origin *testupstream.Server) {
		body := `{"meta":{"api-version":"1.1"},"name":"demo-pkg","files":[{` +
			`"filename":"` + wheel + `","url":"/packages/` + wheel + `",` +
			`"hashes":{"sha256":"abc"},"requires-python":">=3.11",` +
			`"yanked":"historical reason","core-metadata":true}]}`
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: []byte(body), ContentType: jsonMediaType,
		})
	})

	resp := h.Get("/root/pypi/+simple/demo-pkg/")
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Status, resp.Text())
	}
	for _, want := range []string{
		`#sha256=abc`,
		`data-requires-python="&gt;=3.11"`,
		`data-yanked=""`,
		`data-core-metadata="true"`,
		`>demo_pkg-2.0-py3-none-any.whl</a>`,
	} {
		if !strings.Contains(resp.Text(), want) {
			t.Fatalf("HTML missing %q:\n%s", want, resp.Text())
		}
	}
}

func TestJSONRenderPreservesNullRequiresPython(t *testing.T) {
	body, err := renderJSON("demo", []simpleFile{{
		Filename: "demo-1.tar.gz", Yanked: false, CoreMetadata: false,
	}}, "https://cache.invalid/global/pypi/+f/demo")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	value, present := document.Files[0]["requires-python"]
	if !present || value != nil {
		t.Fatalf("requires-python = %#v, present=%v; want explicit null", value, present)
	}
}

func TestParseLegacyPythonSimpleArray(t *testing.T) {
	const body = `[{"filename":"demo-1.whl","url":"https://files.invalid/demo-1.whl",` +
		`"hashes":{"sha256":"abc"},"requires_python":">=3.10",` +
		`"yanked":"bad build","core_metadata":{"sha256":"def"}}]`
	files, err := parseJSON([]byte(body), "https://pypi.invalid/simple/demo/")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Filename != "demo-1.whl" ||
		files[0].RequiresPython != ">=3.10" || files[0].Yanked != "bad build" {
		t.Fatalf("legacy files = %#v", files)
	}
	core, ok := files[0].CoreMetadata.(map[string]string)
	if !ok || core["sha256"] != "def" {
		t.Fatalf("legacy core metadata = %#v", files[0].CoreMetadata)
	}
}

func TestWheelAndMetadataSidecar(t *testing.T) {
	const (
		wheelName = "demo_pkg-1.2.3-py3-none-any.whl"
		wheelBody = "valid wheel bytes"
		metadata  = "Metadata-Version: 2.3\nName: demo-pkg\nVersion: 1.2.3\n"
	)
	h := pypiHarness(t, func(origin *testupstream.Server) {
		body := `{"files":[{"filename":"` + wheelName + `",` +
			`"url":"` + origin.URLFor("/packages/"+wheelName) + `",` +
			`"hashes":{"sha256":"` + digestString(wheelBody) + `"},` +
			`"core-metadata":true}]}`
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: []byte(body), ContentType: jsonMediaType,
		})
		origin.Serve("/packages/"+wheelName, []byte(wheelBody))
		origin.Serve("/packages/"+wheelName+".metadata", []byte(metadata))
	})

	// The simple request is what real pip/uv performs first and seeds the document.
	if resp := requestJSON(h, "/root/pypi/+simple/demo-pkg/"); resp.Status != http.StatusOK {
		t.Fatalf("simple status = %d", resp.Status)
	}
	for i := 0; i < 2; i++ {
		resp := h.Get("/root/pypi/+f/demo-pkg/" + wheelName)
		if resp.Status != http.StatusOK || resp.Text() != wheelBody {
			t.Fatalf("wheel request %d = %d %q", i, resp.Status, resp.Text())
		}
	}
	resp := h.Get("/root/pypi/+f/demo-pkg/" + wheelName + ".metadata")
	if resp.Status != http.StatusOK || resp.Text() != metadata {
		t.Fatalf("metadata = %d %q", resp.Status, resp.Text())
	}
	if hits := h.Origin.Hits("/packages/" + wheelName); hits != 1 {
		t.Fatalf("wheel upstream hits = %d, want 1", hits)
	}

	h.Flush()
	arts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v rows=%+v", total, err, arts)
	}
	if arts[0].Name != "demo-pkg" || arts[0].Version != "1.2.3" ||
		arts[0].Arch != "py3-none-any" {
		t.Fatalf("artifact = %+v", arts[0])
	}
}

func TestDeclaredSHA256RejectsBadWheel(t *testing.T) {
	const wheel = "demo_pkg-1.0-py3-none-any.whl"
	h := pypiHarness(t, func(origin *testupstream.Server) {
		body := `{"files":[{"filename":"` + wheel + `",` +
			`"url":"` + origin.URLFor("/packages/"+wheel) + `",` +
			`"hashes":{"sha256":"` + digestString("expected") + `"}}]}`
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: []byte(body), ContentType: jsonMediaType,
		})
		origin.Serve("/packages/"+wheel, []byte("wrong"))
	})

	// HEAD waits for verification before writing response headers. A progressive
	// GET necessarily starts with 200 before the final digest is known; installers
	// independently verify the hash, while the cache guarantees bad bytes are never
	// committed.
	req, _ := http.NewRequest(http.MethodHead,
		h.URL("/root/pypi/+f/demo-pkg/"+wheel), nil)
	resp := h.Do(req)
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.Status, resp.Text())
	}
	if _, err := h.Catalog.GetEntry(catalog.EntryKey{
		Project: "global", Eco: ID, Key: "root/pypi/+f/demo-pkg/" + wheel,
	}); err == nil {
		t.Fatal("digest-mismatched wheel was cached")
	}
}

func TestIndexesAndUnknownIndex(t *testing.T) {
	h := pypiHarness(t, nil)
	resp := h.Get("/+indexes")
	if resp.Status != http.StatusOK ||
		!strings.Contains(resp.Text(), `"root/pypi"`) {
		t.Fatalf("+indexes = %d %s", resp.Status, resp.Text())
	}
	if resp := h.Get("/root/missing/+simple/demo/"); resp.Status != http.StatusNotFound {
		t.Fatalf("unknown index status = %d", resp.Status)
	}
}

func TestOfflineUsesCachedSimpleDocumentAndWheel(t *testing.T) {
	const wheel = "demo-1.0-py3-none-any.whl"
	h := pypiHarness(t, func(origin *testupstream.Server) {
		body := `{"files":[{"filename":"` + wheel + `",` +
			`"url":"` + origin.URLFor("/packages/"+wheel) + `","hashes":{}}]}`
		origin.Handle("/demo/", testupstream.Behaviour{
			Body: []byte(body), ContentType: jsonMediaType,
		})
		origin.Serve("/packages/"+wheel, []byte("wheel"))
	})
	if resp := h.Get("/root/pypi/+f/demo/" + wheel); resp.Status != http.StatusOK {
		t.Fatalf("seed status = %d: %s", resp.Status, resp.Text())
	}
	h.Offline(true)
	if resp := requestJSON(h, "/root/pypi/+simple/demo/"); resp.Status != http.StatusOK {
		t.Fatalf("offline simple = %d: %s", resp.Status, resp.Text())
	}
	if resp := h.Get("/root/pypi/+f/demo/" + wheel); resp.Text() != "wheel" {
		t.Fatalf("offline wheel = %d %q", resp.Status, resp.Text())
	}
}

func TestParseDistributionFilename(t *testing.T) {
	tests := []struct {
		filename, name, version, arch string
		ok                            bool
	}{
		{"numpy-2.1.0-cp312-cp312-manylinux_x86_64.whl", "numpy", "2.1.0", "cp312-cp312-manylinux_x86_64", true},
		{"demo_pkg-1.0-py3-none-any.whl", "demo-pkg", "1.0", "py3-none-any", true},
		{"typing_extensions-4.12.2.tar.gz", "typing-extensions", "4.12.2", "", true},
		{"source-1.0.tar.xz", "source", "1.0", "", true},
		{"README.txt", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			name, version, arch, ok := ParseDistributionFilename(tt.filename)
			if name != tt.name || version != tt.version || arch != tt.arch || ok != tt.ok {
				t.Fatalf("got (%q,%q,%q,%v), want (%q,%q,%q,%v)",
					name, version, arch, ok, tt.name, tt.version, tt.arch, tt.ok)
			}
		})
	}
}

func TestDescriptor(t *testing.T) {
	reg := eco.NewRegistry()
	reg.Register(New())
	d := reg.Descriptors()[0]
	if d.ID != ID || d.Upstreams != eco.UpstreamNamedSet ||
		!d.FreshnessFor("root/pypi/+f/demo/a.whl").Immutable ||
		d.FreshnessFor("simple/root/pypi/demo").Immutable {
		t.Fatalf("descriptor = %+v", d)
	}
}

// §8 fuzz target: PEP 503 HTML. The parser reads whatever an upstream index served,
// including a private mirror nobody here controls, and the URLs it extracts become
// upstream requests. It must not panic, and it must not emit a file the cache would
// then fetch from somewhere the page did not name.
func FuzzParseSimpleHTML(f *testing.F) {
	const page = "https://pypi.org/simple/demo/"
	for _, seed := range []string{
		`<a href="demo-1.0-py3-none-any.whl#sha256=` + strings.Repeat("a", 64) + `">demo</a>`,
		`<a href="demo-1.0.tar.gz" data-requires-python="&gt;=3.10" data-yanked="broken">d</a>`,
		`<a href="../../packages/de/mo/demo-2.0.whl">demo</a>`,
		`<a href="https://files.pythonhosted.org/x/demo-3.0.whl">demo</a>`,
		`<a href="demo+local-1.0.whl">demo</a>`,
		`<a href=demo-unquoted.whl>demo</a>`,
		`<a href="demo.whl" data-core-metadata="sha256=` + strings.Repeat("b", 64) + `">d</a>`,
		`<a href="//evil.example/demo.whl">demo</a>`,
		`<a href="javascript:alert(1)">demo</a>`,
		"<a href=",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		files, err := parseSimple([]byte(body), "text/html", page)
		if err != nil {
			return // a page we refuse to read cannot mislead us
		}
		for _, file := range files {
			if file.Filename == "" {
				t.Fatalf("emitted a file with no name from %q", body)
			}
			if strings.ContainsAny(file.Filename, "/\\") {
				t.Fatalf("filename %q contains a path separator", file.Filename)
			}
			// Every URL has to be absolute and http(s): a relative or scheme-less result
			// would be resolved later against something other than this page.
			if !strings.HasPrefix(file.URL, "http://") && !strings.HasPrefix(file.URL, "https://") {
				t.Fatalf("file %q resolved to a non-HTTP URL %q", file.Filename, file.URL)
			}
			// A declared hash is reported as the page wrote it. What matters is that
			// anything the adapter will actually verify against parses as a digest —
			// a malformed one is dropped there rather than becoming a bogus path.
			if raw := file.Hashes["sha256"]; raw != "" {
				if digest, err := blob.ParseDigest(raw); err == nil && !digest.Valid() {
					t.Fatalf("accepted sha256 %q as a usable digest", raw)
				}
			}
		}
	})
}

// A stray non-UTF-8 byte in an index used to panic the parser: the scanner folded case
// with strings.ToLower, which rewrites invalid bytes into U+FFFD and so changes the byte
// length, and then sliced the original string with offsets from the folded copy.
func TestParseHTMLSurvivesInvalidUTF8AndOddAnchors(t *testing.T) {
	const page = "https://pypi.org/simple/demo/"
	for _, body := range []string{
		"<A href=\"\xe3\xf7\"></A",
		"<A href=\"demo-1.0.whl\">\xff\xfe</A>",
		"<a href=\"0\">/</a>",
		"<a href=\"demo.whl\">..</a>",
		"<a href=\"javascript:alert(1)\">demo</a>",
		"<a href=\"file:///etc/passwd\">demo</a>",
		"<a href=\"//host-relative/demo.whl\">demo</a>",
	} {
		files, err := parseSimple([]byte(body), "text/html", page)
		if err != nil {
			continue
		}
		for _, file := range files {
			if !validDistributionFilename(file.Filename) {
				t.Errorf("body %q yielded unusable filename %q", body, file.Filename)
			}
			if !strings.HasPrefix(file.URL, "http://") &&
				!strings.HasPrefix(file.URL, "https://") {
				t.Errorf("body %q yielded non-HTTP URL %q", body, file.URL)
			}
		}
	}
}

// A scheme-relative href resolves against the page, so it must keep the page's scheme
// rather than becoming an unfetchable URL.
func TestSchemeRelativeHrefResolvesAgainstThePage(t *testing.T) {
	files, err := parseSimple(
		[]byte(`<a href="//files.example/demo-1.0.whl">demo-1.0.whl</a>`),
		"text/html", "https://pypi.org/simple/demo/")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].URL != "https://files.example/demo-1.0.whl" {
		t.Fatalf("URL = %q", files[0].URL)
	}
}

// A wheel whose filename carries a '+' — which is every PyTorch CUDA wheel, since the
// local version is part of the name. Rebuilding the URL from the decoded path emitted a
// literal '+', download.pytorch.org did not recognise it, and the cache answered 502
// while the wheel beside it served fine.
func TestMetadataSuffixKeepsTheOriginalEncoding(t *testing.T) {
	const wheel = "https://download.pytorch.org/whl/cu130/" +
		"torchcodec-0.16.0%2Bcu130-cp312-cp312-manylinux_2_28_x86_64.whl"
	got := addMetadataSuffix(wheel)
	if !strings.Contains(got, "%2B") {
		t.Errorf("the escape was lost, so upstream sees a different filename:\n%s", got)
	}
	if !strings.HasSuffix(got, ".whl.metadata") {
		t.Errorf("suffix not appended: %s", got)
	}
	if strings.Contains(got, "+cu130") {
		t.Errorf("a literal '+' reached the URL: %s", got)
	}
}

func TestMetadataSuffixLeavesAPlainURLAlone(t *testing.T) {
	// Nothing to preserve, and the result must not gain an encoding it never had.
	got := addMetadataSuffix("https://files.pythonhosted.org/x/idna-3.10-py3-none-any.whl")
	if got != "https://files.pythonhosted.org/x/idna-3.10-py3-none-any.whl.metadata" {
		t.Errorf("addMetadataSuffix = %s", got)
	}
}
