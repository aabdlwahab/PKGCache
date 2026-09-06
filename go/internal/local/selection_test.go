//go:build !windows

// The machine's working project, over the route the window uses. Tested through a real
// instance rather than a fake, because the thing that was broken was the join between the
// two halves: the browser had a project and the file every command reads had another, and
// each half worked perfectly on its own.
package local

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// Home is redirected for every test in this file, not only the ones that re-point:
	// the surface writes into a home directory, and a test that reached the real one
	// would rewrite the .npmrc of whoever ran it.
	instance.API.Selection = &Selection{DataDir: snap.DataDir, Home: t.TempDir()}
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

// The window draws its warning from this: what the machine works in and what the tools
// were pointed at, in one answer, because the interesting case is them disagreeing.
func TestSelectionReportsThePersistedProject(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	home := t.TempDir()
	server, instance, dataDir := newSelection(t)
	instance.API.Selection = &Selection{DataDir: dataDir, Home: home}
	if _, err := instance.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPersist(PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Project: "global", DataDir: dataDir, Home: home,
		Available: AvailabilityAccepted, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if response := selectProject(t, server, "work"); response.StatusCode != http.StatusOK {
		t.Fatalf("selecting work answered %s", response.Status)
	}

	var body struct {
		Project   string `json:"project"`
		Persisted *struct {
			Project string `json:"project"`
			Files   int    `json:"files"`
		} `json:"persisted"`
	}
	response, err := server.Client().Get(server.URL + "/api/v1/local/project")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Project != "work" {
		t.Fatalf("the machine works in %q, want work", body.Project)
	}
	if body.Persisted == nil || body.Persisted.Project != "global" {
		t.Fatalf("the persisted project reads as %+v, want global", body.Persisted)
	}
	if body.Persisted.Files == 0 {
		t.Fatal("the persisted record reports no files")
	}

	// And the button: one request moves the tools onto the machine's project.
	request, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/local/project/repoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	repointed, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repointed.Body.Close() }()
	if repointed.StatusCode != http.StatusOK {
		t.Fatalf("re-pointing answered %s", repointed.Status)
	}
	if after := readPersistedFile(t, filepath.Join(home, ".npmrc")); !strings.Contains(after, "/work/npm/") {
		t.Fatalf("npmrc was not re-pointed: %s", after)
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

// The second half of the bug the window could not show: the persisted settings name one
// project literally, so switching project moves `pkgcache build` and leaves every
// `npm install` where it was. Re-pointing is what the window's button calls.
func TestRepointMovesThePersistedSettings(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	home := t.TempDir()
	dir := t.TempDir()
	if err := ApplyPersist(PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Project: "global", DataDir: dir, Home: home,
		GitHosts: []string{"github.com"}, Available: AvailabilityAccepted, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	npmrc := filepath.Join(home, ".npmrc")
	if before := readPersistedFile(t, npmrc); !strings.Contains(before, "/global/npm/") {
		t.Fatalf("persist did not name the project it was given: %s", before)
	}

	selection := &Selection{DataDir: dir, Home: home}
	settings, found := selection.Persisted()
	if !found || settings.Project != "global" {
		t.Fatalf("persisted reads as %+v found=%v, want global", settings, found)
	}
	if err := selection.Repoint("work"); err != nil {
		t.Fatal(err)
	}
	if after := readPersistedFile(t, npmrc); !strings.Contains(after, "/work/npm/") {
		t.Fatalf("npmrc was not re-pointed: %s", after)
	}
	// The git hosts survive, recovered from the block rather than remembered: an
	// installation made before re-pointing existed has to come through with the same
	// hosts it was installed with.
	if after := readPersistedFile(t, filepath.Join(home, ".gitconfig")); !strings.Contains(after, "github.com") ||
		!strings.Contains(after, "/work/git/") {
		t.Fatalf("gitconfig lost its hosts or its project: %s", after)
	}
	if settings, _ := selection.Persisted(); settings.Project != "work" {
		t.Fatalf("the record still says %q", settings.Project)
	}
}

// Nothing installed is not an error the window should have to special-case, but it is not
// a silent success either: re-pointing files that do not exist would report that npm had
// been moved when nothing had been.
func TestRepointWithoutAnInstallationIsRefused(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	selection := &Selection{DataDir: t.TempDir()}
	if _, found := selection.Persisted(); found {
		t.Fatal("a cache with no persisted settings reports some")
	}
	if err := selection.Repoint("work"); err == nil {
		t.Fatal("re-pointing an installation that does not exist succeeded")
	}
}

func readPersistedFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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
