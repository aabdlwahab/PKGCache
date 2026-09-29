package git

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

const (
	assetPath  = "/owner/repo/releases/download/v1.0.0/tool-1.0.0.whl"
	assetRoute = "/github.com" + assetPath
)

// releaseHarness points every repository at the fake forge, the way the real resolver
// points github.com/owner/repo at https://github.com/owner/repo.git.
func releaseHarness(t *testing.T, forge func() string, setup func(*testupstream.Server)) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		if setup != nil {
			setup(origin)
		}
		base := origin.URL
		if forge != nil {
			base = forge()
		}
		return NewWithOptions(Options{
			ResolveUpstream: func(repo string) (string, error) {
				_, path, _ := strings.Cut(repo, "/")
				return base + "/" + path + ".git", nil
			},
		})
	})
}

func TestReleaseAssetIsCachedOnceAndInventoried(t *testing.T) {
	body := []byte(strings.Repeat("wheel bytes ", 4096))
	h := releaseHarness(t, nil, func(origin *testupstream.Server) {
		origin.Serve(assetPath, body)
	})

	for range 2 {
		resp := h.Get(assetRoute)
		if resp.Status != http.StatusOK || resp.Text() != string(body) {
			t.Fatalf("asset = %d, %d bytes", resp.Status, len(resp.Text()))
		}
	}
	if hits := h.Origin.Hits(assetPath); hits != 1 {
		t.Fatalf("forge hits = %d, want 1", hits)
	}
	head, err := http.Head(h.URL(assetRoute))
	if err != nil {
		t.Fatal(err)
	}
	_ = head.Body.Close()
	if head.StatusCode != http.StatusOK || head.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("HEAD = %d, Accept-Ranges %q", head.StatusCode, head.Header.Get("Accept-Ranges"))
	}

	h.Flush()
	artifacts, total, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: config.GlobalProject, Eco: ID,
	})
	if err != nil || total != 1 {
		t.Fatalf("inventory total=%d err=%v", total, err)
	}
	if got := artifacts[0]; got.Name != "github.com/owner/repo" || got.Version != "v1.0.0" {
		t.Fatalf("artifact = %+v", got)
	}

	d := h.Eco.Descriptor()
	key := "release/github.com/owner/repo/v1.0.0/tool-1.0.0.whl"
	if !d.FreshnessFor(key).Immutable || d.FreshnessFor("refs/github.com/owner/repo").Immutable {
		t.Fatal("release assets must be immutable and mirrors must not")
	}
	if name, version, _, ok := d.Artifact(key); !ok || name != "github.com/owner/repo" ||
		version != "v1.0.0" {
		t.Fatalf("ParseArtifact(%s) = %s %s %v", key, name, version, ok)
	}
}

func TestReleaseAssetPathsCannotEscape(t *testing.T) {
	h := releaseHarness(t, nil, nil)
	for _, path := range []string{
		"/github.com/owner/repo/releases/download/%2e%2e/tool.whl",
		"/github.com/owner/repo/releases/download/v1/%2e%2e",
		"/not-a-host/repo/releases/download/v1/tool.whl",
	} {
		if resp := h.Get(path); resp.Status != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, resp.Status)
		}
	}
}

// A machine pointed at a team cache fetches a release asset through it, and with
// -no-direct only through it — the forge itself is unreachable from here.
func TestReleaseAssetComesThroughTheTeamCache(t *testing.T) {
	body := []byte("the patched wheel")
	team := releaseHarness(t, nil, func(origin *testupstream.Server) {
		origin.Serve(assetPath, body)
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL
	closed.Close()
	local := releaseHarness(t, func() string { return unreachable }, nil)

	relay := func(direct bool) {
		if err := local.Config.Apply(func(s *config.Snapshot) error {
			s.Local.Relays = map[string]config.Relay{
				config.GlobalProject: {
					Hops: []config.Hop{{Base: team.Server.URL + "/global"}}, Direct: direct,
				},
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	relay(false)
	if resp := local.Get(assetRoute); resp.Status != http.StatusOK || resp.Text() != string(body) {
		t.Fatalf("through the team = %d %q", resp.Status, resp.Text())
	}
	if hits := team.Origin.Hits(assetPath); hits != 1 {
		t.Fatalf("the team cache fetched it %d times, want 1", hits)
	}
}
