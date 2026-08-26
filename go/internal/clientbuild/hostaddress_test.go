package clientbuild

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/dockerfile"
)

func TestHostAddressRewritesEveryLoopbackURL(t *testing.T) {
	options := FromEnvironment(Options{
		HostAddress: true,
		Bridge:      "http://127.0.0.1:41780",
		AptProxy:    "http://127.0.0.1:41780",
		Project:     "global",
	})
	rewrite, err := options.rewriteOptions()
	if err != nil {
		t.Fatal(err)
	}
	if rewrite.Mode != dockerfile.HostGateway {
		t.Fatalf("mode = %v, want HostGateway", rewrite.Mode)
	}
	for name, got := range map[string]string{
		"base":      rewrite.Base,
		"registry":  rewrite.Registry,
		"apt proxy": rewrite.AptProxy,
	} {
		if strings.Contains(got, "127.0.0.1") {
			t.Errorf("%s still names loopback: %q", name, got)
		}
		if !strings.Contains(got, DefaultHostGateway) {
			t.Errorf("%s does not name the host gateway: %q", name, got)
		}
		if !strings.Contains(got, "41780") {
			t.Errorf("%s lost the port: %q", name, got)
		}
	}
}

// The build has to ask for host.docker.internal on native Linux, where Docker does not
// provide it. Harmless on Docker Desktop, which does.
func TestHostAddressBuildAddsTheHostGateway(t *testing.T) {
	var got []string
	options := FromEnvironment(Options{
		HostAddress: true,
		Bridge:      "http://127.0.0.1:41780",
		Project:     "global",
		Stdout:      discard{},
		Stderr:      discard{},
		Runner: func(_ context.Context, _ string, args []string, _ io.Reader) error {
			got = args
			return nil
		},
	})
	dir := project(t, "FROM alpine\nRUN true\n")
	if err := Build(context.Background(), options, []string{dir}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--add-host="+DefaultHostGateway+":host-gateway") {
		t.Fatalf("build did not request the host gateway: %v", got)
	}
	if strings.Contains(joined, "--network=host") {
		t.Errorf("a host-gateway build asked for the host network as well: %v", got)
	}
	if strings.Contains(joined, "--secret") {
		t.Errorf("a plain-HTTP cache was given a CA secret: %v", got)
	}
}

// Bridge mode is unchanged: the loopback address only exists inside the build if the
// container shares this machine's network namespace.
func TestBridgeBuildStillUsesTheHostNetwork(t *testing.T) {
	var got []string
	options := FromEnvironment(Options{
		Bridge:  "http://127.0.0.1:41780",
		Project: "global",
		Stdout:  discard{},
		Stderr:  discard{},
		Runner: func(_ context.Context, _ string, args []string, _ io.Reader) error {
			got = args
			return nil
		},
	})
	dir := project(t, "FROM alpine\nRUN true\n")
	if err := Build(context.Background(), options, []string{dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(got, " "), "--network=host") {
		t.Fatalf("bridge build lost --network=host: %v", got)
	}
}

// pkgcache's namespace wins, so that running `pkgcache build` inside a pkgreg-client
// shell builds against the cache the command names rather than against a server whose
// variables happen to still be set.
func TestEnvironmentPrefersPkgcache(t *testing.T) {
	t.Setenv("PKGREG_BRIDGE_URL", "http://127.0.0.1:9999")
	t.Setenv("PKGREG_PROJECT", "team-a")
	t.Setenv("PKGCACHE_BRIDGE_URL", "http://127.0.0.1:41780")
	t.Setenv("PKGCACHE_PROJECT", "global")

	options := FromEnvironment(Options{})
	if options.Bridge != "http://127.0.0.1:41780" {
		t.Errorf("bridge = %q, want pkgcache's", options.Bridge)
	}
	if options.Project != "global" {
		t.Errorf("project = %q, want pkgcache's", options.Project)
	}

	// With only the server's set, they are still honoured: pkgreg-client is unchanged.
	t.Setenv("PKGCACHE_BRIDGE_URL", "")
	t.Setenv("PKGCACHE_PROJECT", "")
	options = FromEnvironment(Options{})
	if options.Bridge != "http://127.0.0.1:9999" || options.Project != "team-a" {
		t.Errorf("pkgreg-client's environment was ignored: %+v", options)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
