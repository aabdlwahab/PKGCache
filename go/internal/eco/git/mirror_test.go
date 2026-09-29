package git

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

func TestParseGitVersion(t *testing.T) {
	for out, want := range map[string]gitVersion{
		"git version 2.27.0\n":                 {2, 27},
		"git version 2.39.3 (Apple Git-146)\n": {2, 39},
		"git version 2.45.2.windows.1\n":       {2, 45},
		"git version 3.0.0":                    {3, 0},
	} {
		got, ok := parseGitVersion(out)
		if !ok || got != want {
			t.Errorf("parseGitVersion(%q) = %v, %v; want %v", out, got, ok, want)
		}
	}
	for _, out := range []string{"", "hub version 2.14.2", "git version two"} {
		if _, ok := parseGitVersion(out); ok {
			t.Errorf("parseGitVersion(%q) succeeded", out)
		}
	}
}

// A git older than the options the mirror would like still mirrors, refreshes and
// maintains. git 2.27 is RHEL 8's, and it answered the options it does not know with a
// usage error, which made every repository a 502.
func TestAnOlderGitStillMirrors(t *testing.T) {
	fixture := newGitFixture(t, false)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Behaves as git 2.27 does: the same version, and exit 129 for each option it lacks.
	old := filepath.Join(t.TempDir(), "git-2.27")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ] || [ "$1" = "--version" ]; then
  echo "git version 2.27.0"
  exit 0
fi
for arg in "$@"; do
  case "$arg" in
    --no-write-fetch-head|--no-auto-maintenance|--atomic|--geometric*|--write-midx)
      echo "error: unknown option $arg" >&2
      exit 129 ;;
  esac
done
exec %s "$@"
`, shellQuote(realGit))
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return NewWithOptions(Options{
			GitPath: old,
			RefsTTL: -1, // every request refreshes, so the fetch into an existing mirror runs too
			ResolveUpstream: func(string) (string, error) {
				return fixture.upstream, nil
			},
		})
	})

	clone := filepath.Join(t.TempDir(), "clone")
	runGitCommand(t, "", nil, "clone", h.URL("/global/git/example.test/org/demo.git"), clone)
	assertFile(t, filepath.Join(clone, "README.md"), "second\n")

	request, _ := http.NewRequest(http.MethodPost, h.URL("/global/git/+maintain"), nil)
	maintained := h.Do(request)
	var result struct {
		Maintained int      `json:"maintained"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal(maintained.Body, &result); err != nil {
		t.Fatalf("maintain = %d %q: %v", maintained.Status, maintained.Text(), err)
	}
	if maintained.Status != http.StatusOK || result.Maintained != 1 || len(result.Errors) != 0 {
		t.Fatalf("maintain = %d %+v", maintained.Status, result)
	}
}
