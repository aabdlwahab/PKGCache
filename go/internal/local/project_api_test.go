//go:build !windows

// The registry half of projects goes through a real daemon: the point of doing this over
// HTTP rather than by opening the store is that a project can be created while the cache
// is serving, and only a running daemon proves that.
package local

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

func TestProjectLifecycleThroughTheDaemon(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	snap := testSnapshot(t, 0)
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}

	// A fresh cache has exactly one project, and it is the implicit one. Anything else
	// here would mean local mode had invented a tenant nobody asked for.
	projects, err := ListProjects(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Name != config.GlobalProject {
		t.Fatalf("a fresh cache holds %+v, want just %s", projects, config.GlobalProject)
	}

	created, err := CreateProject(ctx, state, "myteam")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "myteam" {
		t.Fatalf("created %+v", created)
	}
	if names := projectNames(t, ctx, state); !contains(names, "myteam") {
		t.Fatalf("the new project is not listed: %v", names)
	}

	// The conflict carries the API's code, because that is the half a script branches on.
	_, err = CreateProject(ctx, state, "myteam")
	if err == nil {
		t.Fatal("creating the same project twice succeeded")
	}
	if !strings.Contains(err.Error(), "project_exists") {
		t.Fatalf("conflict lost its code: %v", err)
	}

	// Refused in the client, before a request is made: the global project is the one
	// every path falls back to, and a cache without it has nowhere to serve from.
	if err := DeleteProject(ctx, state, config.GlobalProject); err == nil {
		t.Fatal("the global project was deleted")
	}

	if err := DeleteProject(ctx, state, "myteam"); err != nil {
		t.Fatal(err)
	}
	if names := projectNames(t, ctx, state); contains(names, "myteam") {
		t.Fatalf("the deleted project is still listed: %v", names)
	}
	if err := DeleteProject(ctx, state, "myteam"); err == nil {
		t.Fatal("deleting a project twice succeeded")
	}
}

// A name the server reserves must fail here with the server's own message rather than
// with something this package invented: there is one set of rules for project names and
// it lives in the control plane.
func TestCreateProjectRefusesAReservedName(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	snap := testSnapshot(t, 0)
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"npm", "api", "v2", "Work", ""} {
		if _, err := CreateProject(ctx, state, name); err == nil {
			t.Fatalf("%q was accepted as a project name", name)
		}
	}
}

// Nothing is reachable, so the error has to name the address rather than surface a bare
// "connection refused" from somewhere in net/http.
func TestProjectAPIReportsAnUnreachableCache(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Port 1 on loopback: privileged, unbound, and refused immediately.
	_, err := ListProjects(ctx, State{Addr: "127.0.0.1:1"})
	if err == nil {
		t.Fatal("listing projects against nothing succeeded")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("the error does not say where it tried: %v", err)
	}
}

func projectNames(t *testing.T, ctx context.Context, state State) []string {
	t.Helper()
	projects, err := ListProjects(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(projects))
	for _, project := range projects {
		names = append(names, project.Name)
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A project created over the API inherits the team chain, exactly as one created from
// the CLI does.
//
// This is the regression that matters most in this file. The chain used to be written by
// `pkgcache project create` itself, so it existed for that one command and for nothing
// else: a project made from the widget, or by anything speaking to the API, was created
// with no upstream rows at all. That does not fail — it resolves straight to the public
// internet, succeeding every time, while the team cache it was supposed to use sits
// configured and idle. Nothing in the output of a build would ever say so.
func TestAProjectCreatedOverTheAPIInheritsTheChain(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	snap := testSnapshot(t, 0)
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })

	// A team configured for the global project, which every other project follows.
	var set TeamSet
	set.Set(config.GlobalProject, Team{
		Server: "https://team.internal:8443", Project: config.GlobalProject, Direct: true,
	})
	if err := WriteTeams(snap.DataDir, set); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CreateProject(ctx, state, "fresh"); err != nil {
		t.Fatal(err)
	}
	stopDaemon(t, snap.DataDir)

	rows := managedUpstreamsFor(t, snap, "fresh")
	if len(rows) == 0 {
		t.Fatal("a project created over the API has no chain: it will go straight to the " +
			"public internet and never touch the team cache")
	}
	var teamRows int
	for _, row := range rows {
		if row.Priority == teamPriority {
			teamRows++
			if !strings.Contains(row.URL, "team.internal") {
				t.Fatalf("row at team priority does not point at the team: %+v", row)
			}
		}
	}
	if teamRows != len(chainedEcosystems) {
		t.Fatalf("%d rows point at the team, want one per chained index (%d)",
			teamRows, len(chainedEcosystems))
	}
}
