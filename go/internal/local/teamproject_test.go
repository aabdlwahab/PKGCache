//go:build !windows

package local

import (
	"context"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
)

// The far side's project need not be this side's, and usually is not: a laptop working in
// "global" points at the team's "research". The whole point of the setting is that the two
// names are independent, and nothing tested that they were.
func TestTeamProjectNamesTheFarSide(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)
	server, fingerprint := fakeTeam(t)

	state, err := sources.Configure(context.Background(), config.GlobalProject,
		controlapi.SourceSpec{
			Server: server.URL, Fingerprint: fingerprint,
			TeamProject: "research", Direct: true,
		})
	if err != nil {
		t.Fatal(err)
	}
	if state.TeamProject != "research" {
		t.Fatalf("state reports %q as the far-side project", state.TeamProject)
	}

	// The setting only means anything if it reaches the URL misses are sent to.
	rows, err := instance.Projects.Upstreams(config.GlobalProject)
	if err != nil {
		t.Fatal(err)
	}
	// Every tier-one row has to name it. The trailing slash is not required: the
	// wildcard OCI row is the discovered-registry root, "/v2/research" with no alias
	// after it, and asserting a slash there would fail a URL that is correct.
	scoped, byName := 0, map[string]string{}
	for _, row := range rows {
		if row.Priority != teamPriority {
			continue
		}
		byName[row.Eco+" "+row.Name] = row.URL
		if !strings.Contains(row.URL, "/research") {
			t.Errorf("team upstream %s is pointed at %s, which is not the research project",
				row.Name, row.URL)
		}
		scoped++
	}
	if scoped == 0 {
		t.Fatal("no team-tier upstream was written")
	}
	// Spelled out for the three shapes, because each composes the project differently and
	// a regression in one would hide behind the others in a Contains check.
	for name, want := range map[string]string{
		"pypi root/pypi":             "/research/pypi/root/pypi/+simple",
		"npm registry":               "/research/npm",
		"oci dockerhub":              "/v2/research/dockerhub",
		"oci " + ociname.AnyRegistry: "/v2/research",
	} {
		if got, ok := byName[name]; !ok {
			t.Errorf("no team upstream for %s", name)
		} else if !strings.HasSuffix(got, want) {
			t.Errorf("%s is %s, want it to end in %s", name, got, want)
		}
	}

	// And it is remembered, because `status` and the window both read it back rather
	// than re-deriving it from a chain URL.
	team, has, err := ReadTeam(sources.DataDir, config.GlobalProject)
	if err != nil || !has {
		t.Fatalf("team record: %v %v", has, err)
	}
	if team.Project != "research" {
		t.Fatalf("the record says %q", team.Project)
	}
}

// Left empty it is the far side's global project, which is what somebody who has not
// thought about it means — and it must be that rather than the local project's name,
// which would point a laptop working in "work" at a team project that may not exist.
func TestTeamProjectDefaultsToTheirGlobal(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, _ := newSources(t)
	server, fingerprint := fakeTeam(t)

	if _, err := instanceProject(t, sources, server.URL, fingerprint); err != nil {
		t.Fatal(err)
	}
	team, has, err := ReadTeam(sources.DataDir, config.GlobalProject)
	if err != nil || !has {
		t.Fatalf("team record: %v %v", has, err)
	}
	if team.Project != config.GlobalProject {
		t.Fatalf("an unset team project became %q", team.Project)
	}
}

func instanceProject(
	t *testing.T, sources *Sources, server, fingerprint string,
) (controlapi.SourceState, error) {
	t.Helper()
	return sources.Configure(context.Background(), config.GlobalProject,
		controlapi.SourceSpec{Server: server, Fingerprint: fingerprint})
}
