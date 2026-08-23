package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Projects on one machine, without accounts.
//
// A server's project is a tenant: an owner, a quota, a grant list, someone else to be
// kept out of. None of that means anything here, and pretending otherwise would invite
// the wrong reading of what this is for. On a laptop a project is exactly two things:
//
//   - a separate upstream chain, so work goes through the company's cache and a side
//     project goes straight to the public registry, and
//   - a separate accounting boundary, so "what is this costing per project" is a
//     question the single catalog can answer.
//
// It is deliberately NOT an isolation boundary. The blob store is content-addressed and
// shared, which is the point: two projects needing the same wheel store it once.
//
// The registry itself is the server's, unchanged — the control database is opened in
// local mode and the auth guard allows everything when no accounts exist, so project
// records here are the same records, created through the same API the console uses. What
// this file adds is the two things a command line needs and a browser does not: talking
// to that API without a session, and remembering which project the user is working in.

// projectPath is where the current project is kept. Its own file, and not a line in the
// daemon state, because a choice about how somebody works outlives every daemon.
func projectPath(dataDir string) string { return filepath.Join(dataDir, "project.json") }

// currentProject is the on-disk shape. A record rather than a bare string, so a later
// per-project setting has somewhere to go without a migration.
type currentProject struct {
	Current string `json:"current"`
}

// ProjectEnvVar names the project for one command, without changing the stored choice.
//
// CI wants this: a pipeline that must not depend on what a previous step selected, and
// cannot answer an interactive question, sets one variable. It is read here rather than
// in config because the project is not a server setting — it never reaches a Snapshot.
const ProjectEnvVar = config.LocalEnvPrefix + "PROJECT"

// CurrentProject returns the project commands work in when none is named.
//
// Never an error and never empty: an unreadable or malformed file means the global
// project, because failing a `pkgcache run` over a corrupt preference file would be a
// worse answer than quietly using the default the user started with.
func CurrentProject(dataDir string) string {
	if name := strings.TrimSpace(os.Getenv(ProjectEnvVar)); name != "" {
		return name
	}
	data, err := os.ReadFile(projectPath(dataDir))
	if err != nil {
		return config.GlobalProject
	}
	var stored currentProject
	if err := json.Unmarshal(data, &stored); err != nil {
		return config.GlobalProject
	}
	if name := strings.TrimSpace(stored.Current); name != "" {
		return name
	}
	return config.GlobalProject
}

// SetCurrentProject records the project later commands default to.
func SetCurrentProject(dataDir, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("local: a project name is required")
	}
	// The default is the absence of a choice, not a stored value: a cache whose
	// preference file says "global" and one that has never been asked should behave
	// identically, and only one of them needs a file.
	if name == config.GlobalProject {
		ClearCurrentProject(dataDir)
		return nil
	}
	data, err := json.MarshalIndent(currentProject{Current: name}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(projectPath(dataDir), append(data, '\n'), 0o600)
}

// ClearCurrentProject forgets the choice, returning to the global project.
func ClearCurrentProject(dataDir string) { _ = os.Remove(projectPath(dataDir)) }

// HasCurrentProject reports whether a choice was made, which `status` shows differently
// from the default nobody chose.
func HasCurrentProject(dataDir string) bool {
	if strings.TrimSpace(os.Getenv(ProjectEnvVar)) != "" {
		return true
	}
	_, err := os.Stat(projectPath(dataDir))
	return err == nil
}

// Project is what the control API reports about one project.
//
// A subset: this is a laptop, so an owner, a grant list and a quota are fields with
// nothing to say. Offline is here because it is per-project and genuinely useful — one
// project pinned to what it already has while another keeps fetching.
type Project struct {
	Name      string    `json:"name"`
	Offline   bool      `json:"offline"`
	CreatedAt time.Time `json:"created_at"`
}

// projectAPI is the client for the control API on the loopback socket.
//
// Over HTTP rather than by opening the store, for one reason that decides it: the store
// has a single writer, so a command that opened it would have to stop the daemon first,
// and stopping a running cache to create a project would interrupt whatever else is
// downloading through it. The daemon is already the writer; ask it.
type projectAPI struct {
	base   string
	client *http.Client
}

// newProjectAPI talks to the daemon described by a State.
func newProjectAPI(state State) projectAPI {
	// Longer than probeClient's two seconds and far shorter than a package fetch:
	// these are local database round trips, and deleting a project also drops its
	// catalog roots, which is the slow one.
	return projectAPI{base: state.BaseURL(), client: &http.Client{Timeout: 30 * time.Second}}
}

// ListProjects returns every project this cache knows, global first and the rest by
// name, so the output of two runs can be compared.
func ListProjects(ctx context.Context, state State) ([]Project, error) {
	var body struct {
		Projects []Project `json:"projects"`
	}
	if err := newProjectAPI(state).do(ctx, http.MethodGet, "/api/v1/projects", nil, &body); err != nil {
		return nil, err
	}
	return body.Projects, nil
}

// CreateProject registers a project on this cache.
func CreateProject(ctx context.Context, state State, name string) (Project, error) {
	var created Project
	request := map[string]string{"name": name}
	err := newProjectAPI(state).do(ctx, http.MethodPost, "/api/v1/projects", request, &created)
	return created, err
}

// DeleteProject unregisters a project and drops its catalog roots.
//
// The bytes stay: they are content-addressed and may be another project's too, so they
// are reclaimed by `pkgcache prune` when nothing references them. Saying so is the point —
// somebody deleting a project to free space needs to know that is not what happened.
func DeleteProject(ctx context.Context, state State, name string) error {
	if name == config.GlobalProject {
		return errors.New("local: the global project cannot be deleted")
	}
	return newProjectAPI(state).do(
		ctx, http.MethodDelete, "/api/v1/projects/"+name, nil, nil)
}

// do performs one control-plane request and decodes the error shape the API uses.
func (a projectAPI) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("local: reach the cache at %s: %w", a.base, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 400 {
		return apiError(response)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("local: the cache answered %s with something unreadable: %w",
			response.Status, err)
	}
	return nil
}

// apiError turns the control plane's {"code","error"} into a Go error.
//
// The code is carried in the message rather than dropped, because it is the stable half:
// "project_exists" is what a script would branch on and what a bug report should quote.
func apiError(response *http.Response) error {
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if err := json.Unmarshal(data, &failure); err != nil || failure.Message == "" {
		return fmt.Errorf("local: the cache answered %s", response.Status)
	}
	if failure.Code == "" {
		return errors.New(failure.Message)
	}
	return fmt.Errorf("%s (%s)", failure.Message, failure.Code)
}
