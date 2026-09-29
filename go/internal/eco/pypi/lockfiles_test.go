package pypi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

const (
	lockedWheel = "/packages/5f/97/2aab507d3d00ca626e8e57c1eac6a79e4e5fbcc63eb99733ff55d1717f65/" +
		"pydantic_core-2.46.4-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
	torchWheel = "/whl/cu126/torch-2.8.0%2Bcu126-cp312-cp312-manylinux_2_28_x86_64.whl"
)

// filesHarness serves the lock-file hosts' paths from the test origin.
func filesHarness(t *testing.T, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		return NewWithIndexes(map[string]string{"root/pypi": origin.URL}).
			WithFileHost("files.pythonhosted.org", origin.URL).
			WithFileHost("download-r2.pytorch.org", origin.URL)
	})
}

// A file uv.lock names by its address is served from that address's path, cached, and
// inventoried like any other wheel — PyPI's and PyTorch's alike.
func TestLockedFileIsServedByItsAddress(t *testing.T) {
	h := filesHarness(t, func(origin *testupstream.Server) {
		origin.Serve(lockedWheel, []byte("pydantic bytes"))
		origin.Serve("/whl/cu126/torch-2.8.0+cu126-cp312-cp312-manylinux_2_28_x86_64.whl",
			[]byte("torch bytes"))
	})
	for path, want := range map[string]string{
		"/+files/files.pythonhosted.org" + lockedWheel:             "pydantic bytes",
		"/global/pypi/+files/files.pythonhosted.org" + lockedWheel: "pydantic bytes",
		"/+files/download-r2.pytorch.org" + torchWheel:             "torch bytes",
	} {
		if got := h.Get(path); got.Status != http.StatusOK || got.Text() != want {
			t.Fatalf("GET %s = %d %q", path, got.Status, got.Text())
		}
	}
	if n := h.Origin.Hits(lockedWheel); n != 1 {
		t.Fatalf("PyPI was asked %d times, want once", n)
	}
	h.Flush()
	arts, _, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: "global", Eco: ID})
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, a := range arts {
		versions[a.Name] = a.Version
	}
	if versions["pydantic-core"] != "2.46.4" || versions["torch"] != "2.8.0+cu126" {
		t.Fatalf("inventory = %v", versions)
	}
}

// The route fetches from a fixed set of hosts, so it answers their package files and
// nothing else.
func TestLockedFileRefusesAnythingButAPackageFile(t *testing.T) {
	h := filesHarness(t, nil)
	for _, path := range []string{
		"/+files/evil.example/packages/x-1.0-py3-none-any.whl",
		"/+files/files.pythonhosted.org/simple/six/",
		"/+files/files.pythonhosted.org/packages/aa/bb/readme.html",
		"/+files/files.pythonhosted.org/packages/5f/%2e%2e/%2e%2e/secret.whl",
		"/+files/files.pythonhosted.org/packages/5f%2F97/x-1.0-py3-none-any.whl",
		"/+files/files.pythonhosted.org/packages/5f//x-1.0-py3-none-any.whl",
		"/+files/files.pythonhosted.org/.whl",
	} {
		if got := h.Get(path); got.Status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, got.Status)
		}
	}
	if n := h.Origin.Requests.Load(); n != 0 {
		t.Fatalf("the origin was asked %d times for paths that are not package files", n)
	}
}

// Behind a team cache, a locked file is asked of the team at the same path first.
func TestLockedFileAsksTheTeamFirst(t *testing.T) {
	var teamHits atomic.Int64
	team := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/pypi/+files/files.pythonhosted.org"+lockedWheel ||
			r.URL.Query().Get("sha256") != digestString("from the team") {
			http.NotFound(w, r)
			return
		}
		teamHits.Add(1)
		_, _ = w.Write([]byte("from the team"))
	}))
	t.Cleanup(team.Close)

	h := filesHarness(t, func(origin *testupstream.Server) {
		origin.Serve(lockedWheel, []byte("from pypi"))
	})
	if err := h.Config.Apply(func(s *config.Snapshot) error {
		s.Local.Relays = map[string]config.Relay{config.GlobalProject: {
			Direct: true, Hops: []config.Hop{{Base: team.URL + "/global"}},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The hash travels up the chain with the path, so the team can answer by it too.
	got := h.Get("/+files/files.pythonhosted.org" + lockedWheel + "?sha256=" + digestString("from the team"))
	if got.Status != http.StatusOK || got.Text() != "from the team" {
		t.Fatalf("GET = %d %q, want the team's copy", got.Status, got.Text())
	}
	if teamHits.Load() != 1 || h.Origin.Hits(lockedWheel) != 0 {
		t.Fatalf("team hits %d, PyPI hits %d: want the team alone",
			teamHits.Load(), h.Origin.Hits(lockedWheel))
	}
}

// The lock's hash, passed on by the build, lets a file held under any other name be served
// without fetching it again: here, the wheel an index install already brought in.
func TestLockedFileWithItsHashIsServedFromWhatIsAlreadyHeld(t *testing.T) {
	const wheel = "demo_pkg-1.0-py3-none-any.whl"
	const path = "/packages/aa/bb/cccc/" + wheel
	h := filesHarness(t, func(origin *testupstream.Server) {
		body := `{"files":[{"filename":"` + wheel + `","url":"` + origin.URLFor("/index-files/"+wheel) +
			`","hashes":{"sha256":"` + digestString("the wheel") + `"}}]}`
		origin.Handle("/demo-pkg/", testupstream.Behaviour{Body: []byte(body), ContentType: jsonMediaType})
		origin.Serve("/index-files/"+wheel, []byte("the wheel"))
		origin.Serve(path, []byte("the wheel"))
	})
	if got := h.Get("/root/pypi/+f/demo-pkg/" + wheel); got.Status != http.StatusOK {
		t.Fatalf("index install = %d %q", got.Status, got.Text())
	}
	got := h.Get("/+files/files.pythonhosted.org" + path + "?sha256=" + digestString("the wheel"))
	if got.Status != http.StatusOK || got.Text() != "the wheel" {
		t.Fatalf("locked file = %d %q", got.Status, got.Text())
	}
	if n := h.Origin.Hits(path); n != 0 {
		t.Fatalf("PyPI was asked %d times for a file already held", n)
	}
}

// A hash that does not match is the cache's to refuse, and nothing is kept.
func TestLockedFileWithTheWrongHashIsRefused(t *testing.T) {
	const path = "/packages/aa/bb/cccc/demo_pkg-1.0-py3-none-any.whl"
	h := filesHarness(t, func(origin *testupstream.Server) {
		origin.Serve(path, []byte("not what the lock says"))
	})
	// HEAD waits for verification, which is where a mismatch surfaces as a status.
	req, _ := http.NewRequest(http.MethodHead,
		h.URL("/+files/files.pythonhosted.org"+path+"?sha256="+digestString("the wheel")), nil)
	if got := h.Do(req); got.Status != http.StatusBadGateway {
		t.Fatalf("HEAD = %d, want 502 for a hash mismatch", got.Status)
	}
	if _, err := h.Catalog.GetEntry(catalog.EntryKey{
		Project: "global", Eco: ID, Key: "files/files.pythonhosted.org" + path,
	}); err == nil {
		t.Fatal("a file that failed its hash was cached")
	}
	if got := h.Get("/+files/files.pythonhosted.org" + path + "?sha256=nothex"); got.Status != http.StatusNotFound {
		t.Fatalf("a malformed hash = %d, want 404", got.Status)
	}
}
