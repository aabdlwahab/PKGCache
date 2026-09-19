package lockwarm

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestCacheRegistryRecognisesOnlyOurOwnURLs(t *testing.T) {
	served := map[string]string{
		"root/pypi":  "https://pypi.org/simple",
		"root/cu124": "https://download.pytorch.org/whl/cu124",
	}
	cases := []struct {
		registry string
		base     string
		index    string
		ok       bool
	}{
		// What a pkgcache writes: the daemon's root, then the project, then the eco.
		{"http://127.0.0.1:41780/global/pypi/root/pypi/+simple",
			"http://127.0.0.1:41780/global/pypi", "root/pypi", true},
		{"https://pkgreg.corp:8443/team-a/pypi/root/cu124/+simple",
			"https://pkgreg.corp:8443/team-a/pypi", "root/cu124", true},
		// A deployment that mounts the indexes at the root writes no project segment,
		// and locks rewritten against one are still in circulation.
		{"https://172.17.21.107:3141/root/pypi/+simple",
			"https://172.17.21.107:3141", "root/pypi", true},
		// A trailing slash is the same URL.
		{"http://127.0.0.1:41780/global/pypi/root/pypi/+simple/",
			"http://127.0.0.1:41780/global/pypi", "root/pypi", true},

		// Real indexes, which must never be mistaken for one of ours.
		{"https://pypi.org/simple", "", "", false},
		{"https://download.pytorch.org/whl/cu124", "", "", false},
		{"https://pypi.org/pypi/foo/simple", "", "", false},
		// Our shape, but an index this cache has no upstream for: not adoptable, and
		// reported as the unknown registry it is.
		{"http://127.0.0.1:41780/global/pypi/root/cu999/+simple", "", "", false},
		// The sentinel and a known index, but nothing in front to be a base.
		{"/root/pypi/+simple", "", "", false},
	}
	for _, c := range cases {
		base, index, ok := CacheRegistry(c.registry, served)
		if ok != c.ok || base != c.base || index != c.index {
			t.Errorf("CacheRegistry(%q) = %q, %q, %v; want %q, %q, %v",
				c.registry, base, index, ok, c.base, c.index, c.ok)
		}
	}
}

// An index whose name is a suffix of another must not eat a segment of the base.
func TestCacheRegistryPrefersTheLongestIndexName(t *testing.T) {
	served := map[string]string{"pypi": "https://pypi.org/simple", "root/pypi": "x"}
	base, index, ok := CacheRegistry("http://127.0.0.1:41780/global/pypi/root/pypi/+simple",
		served)
	if !ok || index != "root/pypi" || base != "http://127.0.0.1:41780/global/pypi" {
		t.Errorf("= %q, %q, %v", base, index, ok)
	}
}

func TestLocalCacheSeparatesThisMachineFromEveryOther(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:41780/global/pypi",
		"http://localhost:41780/global/pypi",
		"http://[::1]:41780/global/pypi",
		"http://127.0.0.2:41780/global/pypi",
	} {
		if !LocalCache(base) {
			t.Errorf("LocalCache(%q) = false", base)
		}
	}
	for _, base := range []string{
		"https://pkgreg.corp:8443/team-a/pypi",
		"http://10.0.0.4:41780/global/pypi",
		"::not a url",
	} {
		if LocalCache(base) {
			t.Errorf("LocalCache(%q) = true", base)
		}
	}
}

// A lock rewritten against a daemon that has since restarted on another port is warmed
// and re-pointed, rather than failing as a registry nothing serves. Every URL the rewrite
// produces names the new port, and nothing but the URLs changes.
func TestRewrittenLockSurvivesTheDaemonChangingPort(t *testing.T) {
	const stale = `version = 1
revision = 3

[[package]]
name = "idna"
version = "3.10"
source = { registry = "http://127.0.0.1:41780/global/pypi/root/pypi/+simple" }
sdist = { url = "http://127.0.0.1:41780/global/pypi/root/pypi/+f/idna/idna-3.10.tar.gz", hash = "sha256:aaa", size = 10 }
wheels = [
    { url = "http://127.0.0.1:41780/global/pypi/root/pypi/+f/idna/idna-3.10-py3-none-any.whl", hash = "sha256:bbb", size = 20 },
]
`
	packages, err := Parse(stale)
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]string{"root/pypi": "https://pypi.org/simple"}
	indexes := NewIndexMap(served)
	if _, ok := indexes.Index(packages[0].Registry); ok {
		t.Fatal("a stale cache URL resolved before it was adopted")
	}

	base, index, ok := CacheRegistry(packages[0].Registry, served)
	if !ok || !LocalCache(base) {
		t.Fatalf("CacheRegistry(%q) = %q, %q, %v", packages[0].Registry, base, index, ok)
	}
	indexes.Adopt(packages[0].Registry, index)

	got := Rewrite(stale, packages, indexes, "http://127.0.0.1:52001/global/pypi")
	if strings.Contains(got, "41780") {
		t.Errorf("the old port survived the rewrite:\n%s", got)
	}
	for _, want := range []string{
		`registry = "http://127.0.0.1:52001/global/pypi/root/pypi/+simple"`,
		"http://127.0.0.1:52001/global/pypi/root/pypi/+f/idna/idna-3.10.tar.gz",
		"http://127.0.0.1:52001/global/pypi/root/pypi/+f/idna/idna-3.10-py3-none-any.whl",
		`hash = "sha256:bbb"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten lock missing %q", want)
		}
	}

	// And it is warmable again, which is the point: the cache is empty on the machine
	// the lock arrived at.
	paths := recordWarm(t, func(handler http.Handler) error {
		return Warm(context.Background(), handler, "global", packages, indexes, 2, nil)
	})
	if len(paths) != 2 {
		t.Fatalf("warmed %v", paths)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/global/pypi/root/pypi/+f/idna/") {
			t.Errorf("wrong warm path %q", path)
		}
	}
}

// One index page per package, whatever the lock pins for it, and the trailing slash the
// simple route is registered with.
func TestWarmIndexAsksOncePerPackage(t *testing.T) {
	packages := []Package{
		{Name: "idna", Registry: "https://pypi.org/simple",
			Files: []File{{Filename: "idna-3.10-py3-none-any.whl"}}},
		// The same project, forked across resolution markers onto two versions.
		{Name: "Torch_Thing", Registry: "https://download.pytorch.org/whl/cu124",
			Files: []File{{Filename: "torch_thing-2.6.0.whl"}}},
		{Name: "torch-thing", Registry: "https://download.pytorch.org/whl/cu124",
			Files: []File{{Filename: "torch_thing-2.5.0.whl"}}},
	}
	indexes := NewIndexMap(map[string]string{
		"root/pypi":  "https://pypi.org/simple",
		"root/cu124": "https://download.pytorch.org/whl/cu124",
	})
	paths := recordWarm(t, func(handler http.Handler) error {
		return WarmIndex(context.Background(), handler, "team-a", packages, indexes, 3, nil)
	})
	want := []string{
		"/team-a/pypi/root/cu124/+simple/torch-thing/",
		"/team-a/pypi/root/pypi/+simple/idna/",
	}
	if len(paths) != len(want) {
		t.Fatalf("warmed %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("warmed %q, want %q", paths[i], want[i])
		}
	}

	// An index the cache does not serve is refused before a single request is made.
	if err := WarmIndex(context.Background(), http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}), "team-a",
		[]Package{{Name: "x", Registry: "https://elsewhere.invalid/simple"}},
		indexes, 1, nil); err == nil {
		t.Error("WarmIndex accepted an unserved registry")
	}
}

// recordWarm runs one warm against a handler that answers everything, and returns the
// paths it was asked for, sorted so the assertions do not depend on the worker pool.
func recordWarm(t *testing.T, warm func(http.Handler) error) []string {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	if err := warm(handler); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}
