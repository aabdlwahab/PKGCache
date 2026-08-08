package lockwarm

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

const sampleLock = `version = 1
revision = 3
requires-python = ">=3.11"

[[package]]
name = "idna"
version = "3.10"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/idna-3.10.tar.gz", hash = "sha256:aaa", size = 10 }
wheels = [
    { url = "https://files.pythonhosted.org/packages/idna-3.10-py3-none-any.whl", hash = "sha256:bbb", size = 20 },
]

[[package]]
name = "Torch_Thing"
version = "2.6.0"
source = { registry = "https://download.pytorch.org/whl/cu124" }
wheels = [
    { url = "https://download.pytorch.org/whl/cu124/torch_thing.whl", hash = "sha256:ccc", size = 30 },
]

[[package]]
name = "workspace"
version = "0.1.0"
source = { virtual = "." }
`

func TestParseAndRewritePreservesNonURLs(t *testing.T) {
	packages, err := Parse(sampleLock)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages[1].Project() != "torch-thing" {
		t.Fatalf("packages = %+v", packages)
	}
	indexes := NewIndexMap(map[string]string{
		"root/pypi":  "https://pypi.org/simple/",
		"root/cu124": "https://download.pytorch.org/whl/cu124",
	})
	got := Rewrite(sampleLock, packages, indexes,
		"https://cache.local:3141/team-a/pypi")
	for _, want := range []string{
		`registry = "https://cache.local:3141/team-a/pypi/root/pypi/+simple"`,
		"https://cache.local:3141/team-a/pypi/root/pypi/+f/idna/idna-3.10-py3-none-any.whl",
		"https://cache.local:3141/team-a/pypi/root/cu124/+f/torch-thing/torch_thing.whl",
		`hash = "sha256:bbb"`,
		`source = { virtual = "." }`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten lock missing %q", want)
		}
	}
	if strings.Contains(got, "files.pythonhosted.org") {
		t.Fatal("upstream file URL survived rewrite")
	}
}

func TestParseRejectsUnsupportedOrMalformedLocks(t *testing.T) {
	for _, text := range []string{"garbage", "version = 99\n"} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("Parse(%q) succeeded", text)
		}
	}
}

func TestWarmFansOutEveryLockedFile(t *testing.T) {
	packages, err := Parse(sampleLock)
	if err != nil {
		t.Fatal(err)
	}
	indexes := NewIndexMap(map[string]string{
		"root/pypi":  "https://pypi.org/simple",
		"root/cu124": "https://download.pytorch.org/whl/cu124",
	})
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
	if err := Warm(context.Background(), handler, "team-a", packages, indexes, 3, nil); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) != 3 {
		t.Fatalf("warmed %d paths: %v", len(paths), paths)
	}
	for _, value := range paths {
		if !strings.HasPrefix(value, "/team-a/pypi/") {
			t.Errorf("wrong project route %q", value)
		}
	}
}
