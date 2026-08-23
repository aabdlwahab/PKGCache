//go:build !windows

// Configuring a source touches the store, the trust bundle and the outbound pool, so it
// is tested against a real instance rather than a fake. See local_test.go for the harness.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
)

// fakeTeam is a TLS server standing in for a pkgreg: it publishes its own certificate at
// the path setup fetches, which is the whole of the trust bootstrap.
func fakeTeam(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var caPEM []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ca.crt" {
			_, _ = w.Write(caPEM)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	caPEM = pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	})
	return server, sha256Sum(server.Certificate().Raw)
}

func newSources(t *testing.T) (*Sources, *app.App) {
	t.Helper()
	snap := testSnapshot(t, 0)
	instance, err := app.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	return &Sources{
		DataDir: snap.DataDir, Store: instance.Projects, Ecos: instance.Ecos,
		Pool: instance.Pool, Snapshot: snap,
	}, instance
}

// The headline: a project is pointed at a cache, the CA is pinned, the chain is written,
// and the running process trusts the cache it has just been told about.
func TestConfigureSourcePinsTrustAndWritesTheChain(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)
	server, fingerprint := fakeTeam(t)

	state, err := sources.Configure(context.Background(), config.GlobalProject,
		controlapi.SourceSpec{Server: server.URL, Fingerprint: fingerprint, Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	if state.Server != server.URL || state.Fingerprint == "" {
		t.Fatalf("state = %+v", state)
	}

	// The chain: the team first, the public registry behind it.
	rows, err := instance.Projects.Upstreams(config.GlobalProject)
	if err != nil {
		t.Fatal(err)
	}
	var team, public int
	for _, row := range rows {
		switch row.Priority {
		case teamPriority:
			team++
		case publicPriority:
			public++
		}
	}
	if team == 0 || public == 0 {
		t.Fatalf("chain is %+v, want both tiers", rows)
	}

	// The bundle, and the pool that reads it. Without the reload the configuration was
	// right and every fetch to the new tier failed on an unknown authority.
	bundle, err := os.ReadFile(TeamCAPath(sources.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bundle), "BEGIN CERTIFICATE") {
		t.Fatal("the trust bundle holds no certificate")
	}
	client := instance.Pool.Client()
	response, err := client.Get(server.URL + "/anything")
	if err != nil {
		t.Fatalf("the running pool does not trust the cache it just pinned: %v", err)
	}
	_ = response.Body.Close()
}

// The fingerprint is the whole trust decision, so both ways of omitting or getting it
// wrong are refused — and nothing is written.
func TestConfigureSourceRefusesWithoutAMatchingFingerprint(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, _ := newSources(t)
	server, fingerprint := fakeTeam(t)

	for _, c := range []struct{ name, server, fingerprint string }{
		{"no fingerprint", server.URL, ""},
		{"wrong fingerprint", server.URL, strings.Repeat("ab", 32)},
		{"no server", "", fingerprint},
	} {
		if _, err := sources.Configure(context.Background(), config.GlobalProject,
			controlapi.SourceSpec{Server: c.server, Fingerprint: c.fingerprint}); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if _, err := os.Stat(TeamCAPath(sources.DataDir)); !os.IsNotExist(err) {
		t.Fatal("a refused configuration still wrote a trust bundle")
	}
}

// Forget removes a project's own configuration and says so when there is none, because
// "forgotten" would otherwise be a false statement about where packages come from.
func TestForgetSource(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, _ := newSources(t)
	server, fingerprint := fakeTeam(t)

	if err := sources.Forget(context.Background(), config.GlobalProject); err == nil {
		t.Error("forgetting a source that does not exist reported success")
	}
	if _, err := sources.Configure(context.Background(), config.GlobalProject,
		controlapi.SourceSpec{Server: server.URL, Fingerprint: fingerprint}); err != nil {
		t.Fatal(err)
	}
	if err := sources.Forget(context.Background(), config.GlobalProject); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(TeamCAPath(sources.DataDir)); !os.IsNotExist(err) {
		t.Error("the last source went and its CA stayed in the bundle")
	}
	states, err := sources.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Server != "" {
			t.Errorf("%s still resolves through %s", state.Project, state.Server)
		}
		if !state.Direct {
			t.Errorf("%s reports neither a team cache nor a direct chain", state.Project)
		}
	}
}

// A listing says which projects chose their configuration and which inherited it, since
// that is the difference between removing an override and excusing a project entirely.
func TestSourcesReportInheritance(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)
	if _, err := instance.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}
	server, fingerprint := fakeTeam(t)
	if _, err := sources.Configure(context.Background(), config.GlobalProject,
		controlapi.SourceSpec{Server: server.URL, Fingerprint: fingerprint}); err != nil {
		t.Fatal(err)
	}
	states, err := sources.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]controlapi.SourceState{}
	for _, state := range states {
		byName[state.Project] = state
	}
	if byName[config.GlobalProject].Inherited {
		t.Error("the project that was configured is reported as inheriting")
	}
	if !byName["work"].Inherited || byName["work"].Server != server.URL {
		t.Errorf("work = %+v, want it following global", byName["work"])
	}
}

// sha256Sum is the fingerprint form trust.Fetch expects: 64 lowercase hex characters.
func sha256Sum(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
