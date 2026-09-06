package clientbuild

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCache serves the two endpoints index discovery reads: the ecosystems it has
// compiled in, and one project's own upstream rows.
func fakeCache(t *testing.T, projectRows string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/ecosystems":
			_, _ = w.Write([]byte(`{"ecosystems":[
				{"id":"npm","default_upstreams":{"registry":"https://registry.npmjs.org"}},
				{"id":"pypi","default_upstreams":{
					"root/pypi":"https://pypi.org/simple",
					"root/pytorch-cpu":"https://download.pytorch.org/whl/cpu"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/upstreams"):
			_, _ = w.Write([]byte(projectRows))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// The bug: a project with no upstream rows of its own rewrote nothing, so an
// --extra-index-url naming a pytorch channel went to the internet — past a cache that
// serves that exact index, in every project, with nothing configured.
func TestBuiltInIndexesAreDiscoveredWithoutAnyRows(t *testing.T) {
	base := fakeCache(t, `{"upstreams":[]}`)

	indexes := DiscoverIndexes(context.Background(), base, "embed")
	if got := indexes["https://download.pytorch.org/whl/cpu"]; got != "root/pytorch-cpu" {
		t.Fatalf("pytorch index resolved to %q, want root/pytorch-cpu", got)
	}
	if got := indexes["https://pypi.org/simple"]; got != "root/pypi" {
		t.Fatalf("pypi index resolved to %q", got)
	}
	// npm has defaults too, and they are a different rewrite with a different path shape.
	if _, found := indexes["https://registry.npmjs.org"]; found {
		t.Fatal("an npm registry was offered as a pypi index")
	}
}

// A row wins over the default for the same URL: an operator who points that URL at an
// index of their own has said something more specific than the adapter's built-in.
func TestProjectRowsOverrideTheBuiltIns(t *testing.T) {
	base := fakeCache(t, `{"upstreams":[
		{"eco":"pypi","name":"pytorch-mirror","url":"https://download.pytorch.org/whl/cpu","enabled":true}]}`)

	indexes := DiscoverIndexes(context.Background(), base, "global")
	if got := indexes["https://download.pytorch.org/whl/cpu"]; got != "pytorch-mirror" {
		t.Fatalf("the project's own row lost to the built-in: %q", got)
	}
}

// A disabled row is not a source, and must not shadow the built-in that still works.
func TestDisabledRowDoesNotShadowTheBuiltIn(t *testing.T) {
	base := fakeCache(t, `{"upstreams":[
		{"eco":"pypi","name":"pytorch-mirror","url":"https://download.pytorch.org/whl/cpu","enabled":false}]}`)

	indexes := DiscoverIndexes(context.Background(), base, "global")
	if got := indexes["https://download.pytorch.org/whl/cpu"]; got != "root/pytorch-cpu" {
		t.Fatalf("a disabled row left the index as %q", got)
	}
}

// A cache that cannot be reached is not a reason to fail a build; the directly named
// indexes go upstream as they always did.
func TestUnreachableCacheYieldsNoIndexes(t *testing.T) {
	if got := DiscoverIndexes(context.Background(), "http://127.0.0.1:1", "global"); len(got) != 0 {
		t.Fatalf("an unreachable cache produced %v", got)
	}
}
