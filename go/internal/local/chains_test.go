//go:build !windows

// Chain configuration opens the store, and the harness that starts a real daemon lives
// on the Unix side of this package. See local_test.go.
package local

import (
	"context"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
)

// A team cache configured for a project that does not exist here is inert: upstream rows
// are per project, and there is no project to write them for. Legitimate — the two
// commands can be run in either order — but also exactly what a typo looks like, so the
// caller is told rather than left with a machine that quietly bypasses its cache.
func TestConfigureChainsReportsProjectsThatDoNotExist(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	snap := testSnapshot(t, 0)

	var set TeamSet
	set.Set(config.GlobalProject, Team{Server: "https://team", Project: "global", Direct: true})
	set.Set("nosuch", Team{Server: "https://elsewhere", Project: "global"})

	unknown, err := ConfigureChains(context.Background(), snap, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "nosuch" {
		t.Fatalf("unknown projects = %v, want [nosuch]", unknown)
	}
}

// The chain written for the one project that does exist has to be the chain, and running
// it twice must leave one of them rather than two.
func TestConfigureChainsIsIdempotent(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	snap := testSnapshot(t, 0)

	var set TeamSet
	set.Set(config.GlobalProject, Team{Server: "https://team", Project: "global", Direct: true})
	for range 2 {
		if _, err := ConfigureChains(context.Background(), snap, set); err != nil {
			t.Fatal(err)
		}
	}
	// Derived rather than written out: the count is a consequence of how many indexes
	// can be chained, and a test that hardcodes it fails the day one is added for a
	// reason that has nothing to do with idempotency.
	want := 2 * len(chainedEcosystems)
	if got := len(managedUpstreams(t, snap)); got != want {
		t.Fatalf("two runs left %d managed rows, want the chain's %d", got, want)
	}

	// And removing the configuration removes them, rather than leaving a chain nothing
	// refers to any more.
	if _, err := ConfigureChains(context.Background(), snap, TeamSet{}); err != nil {
		t.Fatal(err)
	}
	if got := len(managedUpstreams(t, snap)); got != 0 {
		t.Fatalf("%d managed rows survived an empty configuration", got)
	}
}

func managedUpstreams(t *testing.T, snap *config.Snapshot) []control.Upstream {
	t.Helper()
	return managedUpstreamsFor(t, snap, config.GlobalProject)
}

// managedUpstreamsFor is the same for a named project, which is where an inherited chain
// either exists or silently does not.
func managedUpstreamsFor(
	t *testing.T, snap *config.Snapshot, project string,
) []control.Upstream {
	t.Helper()
	instance, err := app.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()
	rows, err := instance.Projects.Upstreams(project)
	if err != nil {
		t.Fatal(err)
	}
	var managed []control.Upstream
	for _, row := range rows {
		if managedRow(row) {
			managed = append(managed, row)
		}
	}
	return managed
}
