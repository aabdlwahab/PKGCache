//go:build !windows

// The machine's working project, over the route the window uses. Tested through a real
// instance rather than a fake, because the thing that was broken was the join between the
// two halves: the browser had a project and the file every command reads had another, and
// each half worked perfectly on its own.
package local

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
)

// newSelection builds an instance with the working-project surface wired the way the
// daemon wires it, and serves its control plane.
func newSelection(t *testing.T) (*httptest.Server, *app.App, string) {
	t.Helper()
	snap := testSnapshot(t, 0)
	instance, err := app.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	instance.API.Selection = &Selection{DataDir: snap.DataDir}
	server := httptest.NewServer(instance.API)
	t.Cleanup(server.Close)
	return server, instance, snap.DataDir
}

func selectProject(t *testing.T, server *httptest.Server, name string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"project":` + quote(name) + `}`)
	request, err := http.NewRequest(http.MethodPut, server.URL+"/api/v1/local/project", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// The headline, and the bug: switching project in the window has to move the project
// every client command works in. It used to move a value in the browser and nothing
// else, so `pkgcache build` and pkgcache-docker went on filling global while the window
// said otherwise.
func TestSelectingAProjectMovesTheMachine(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	server, instance, dataDir := newSelection(t)
	if _, err := instance.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}

	if response := selectProject(t, server, "work"); response.StatusCode != http.StatusOK {
		t.Fatalf("selecting work answered %s", response.Status)
	}
	// CurrentProject and not the file: this is the exact call pkgcache-docker makes to
	// decide which project a build's artifacts land in.
	if got := CurrentProject(dataDir); got != "work" {
		t.Fatalf("the machine is working in %q, want work", got)
	}
	if !HasCurrentProject(dataDir) {
		t.Fatal("the machine reports no chosen project after one was chosen")
	}
}

// Reading it back is how the window opens on the project a terminal may have selected
// since it was last open.
func TestSelectionIsReadBack(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	server, instance, dataDir := newSelection(t)
	if _, err := instance.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrentProject(dataDir, "work"); err != nil {
		t.Fatal(err)
	}

	response, err := server.Client().Get(server.URL + "/api/v1/local/project")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Project string `json:"project"`
		Chosen  bool   `json:"chosen"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Project != "work" || !body.Chosen {
		t.Fatalf("read back %+v, want work and chosen", body)
	}
}

// A default that names nothing is the failure this replaces: it would surface as a 404
// from the router at the next `npm ci`, a long way from the click that caused it.
func TestSelectingAProjectThatIsNotHereIsRefused(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	server, _, dataDir := newSelection(t)

	response := selectProject(t, server, "nosuch")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("selecting a project that does not exist answered %s", response.Status)
	}
	if got := CurrentProject(dataDir); got != config.GlobalProject {
		t.Fatalf("a refused selection was stored anyway: %q", got)
	}
}

// Deleting the project the machine is working in has to move it, or every later command
// asks for a project that is gone. `pkgcache project remove` has always done this; the
// console's delete button reaches the same store by another route.
func TestDeletingTheSelectedProjectReleasesIt(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	server, instance, dataDir := newSelection(t)
	if _, err := instance.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}
	if response := selectProject(t, server, "work"); response.StatusCode != http.StatusOK {
		t.Fatalf("selecting work answered %s", response.Status)
	}

	request, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/projects/work", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("deleting work answered %s", response.Status)
	}
	if got := CurrentProject(dataDir); got != config.GlobalProject {
		t.Fatalf("the machine still works in %q after it was deleted", got)
	}
}

// The environment override belongs to one command's environment, and the daemon's
// environment is not the shell's. The surface reports and writes the stored choice.
func TestStoredProjectIgnoresTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := SetCurrentProject(dir, "work"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProjectEnvVar, "ci")
	if got := CurrentProject(dir); got != "ci" {
		t.Fatalf("the environment no longer overrides one command's project: %q", got)
	}
	if got := StoredProject(dir); got != "work" {
		t.Fatalf("the stored choice reads as %q, want work", got)
	}
	selection := &Selection{DataDir: dir}
	if got, chosen := selection.Selected(); got != "work" || !chosen {
		t.Fatalf("the surface reports %q chosen=%v, want work and true", got, chosen)
	}
}
