package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// An instance with no accounts is not a degraded server, it is pkgcache: one developer,
// one machine, a loopback socket and no adversary the control plane could keep out. The
// guards in internal/control/auth all say so for themselves, and the API's requireOwner
// has to say it too — it funnels through RequireUser, which is deliberately the one
// guard with no such branch because it is what refuses a guest.
//
// The bug this pins: a cache with no accounts could create a project through this API
// and never delete one, from the console or the command line.
func TestProjectLifecycleWithNoAccounts(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	if a.Accounts.Enabled() {
		t.Fatal("this instance has accounts; the test proves nothing")
	}

	response, body := controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/projects", nil, `{"name":"myteam"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %q", response.StatusCode, body)
	}

	response, body = controlRequest(t, server.Client(), http.MethodDelete,
		server.URL+"/api/v1/projects/myteam", nil, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d %q", response.StatusCode, body)
	}

	response, body = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/projects/myteam", nil, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("after deleting, get = %d %q", response.StatusCode, body)
	}
}

// The global project is what every fallback resolves to, so it survives even here.
func TestGlobalProjectSurvivesWithNoAccounts(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	response, body := controlRequest(t, server.Client(), http.MethodDelete,
		server.URL+"/api/v1/projects/global", nil, "")
	if response.StatusCode < 400 {
		t.Fatalf("the global project was deleted: %d %q", response.StatusCode, body)
	}
}

// Opening project deletion must not open the permission system with it. Grants are
// checked a second time against the actor, and with no accounts there is no actor and
// nobody to grant to — so this stays refused, and a grant list on a laptop stays the
// empty and meaningless thing it should be.
func TestGrantsStayRefusedWithNoAccounts(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/api/v1/projects/global/grants/someone", `{"level":"view"}`},
		{http.MethodDelete, "/api/v1/projects/global/grants/someone", ""},
	} {
		response, body := controlRequest(
			t, server.Client(), c.method, server.URL+c.path, nil, c.body)
		if response.StatusCode < 400 {
			t.Fatalf("%s %s = %d %q, want a refusal",
				c.method, c.path, response.StatusCode, body)
		}
	}
}

// 401 on an instance with no accounts describes a door that does not exist, and sends
// anyone reading it looking for a credential they cannot have. The console already hides
// the panel off me.auth_enabled; this is for everything that does not.
func TestAccountEndpointsSayThereAreNoAccounts(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodPost, "/api/v1/users", `{"username":"someone","password":"whatever12"}`},
		{http.MethodPatch, "/api/v1/users/someone", `{"role":"admin"}`},
		{http.MethodDelete, "/api/v1/users/someone", ""},
	} {
		response, body := controlRequest(
			t, server.Client(), c.method, server.URL+c.path, nil, c.body)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d %q, want 404", c.method, c.path, response.StatusCode, body)
			continue
		}
		if !strings.Contains(string(body), "auth_disabled") {
			t.Errorf("%s %s said %q, which does not name the reason", c.method, c.path, body)
		}
	}

	// And /me still says which world this is, because that is what the console reads.
	response, body := controlRequest(
		t, server.Client(), http.MethodGet, server.URL+"/api/v1/me", nil, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"auth_enabled":false`) {
		t.Fatalf("/me = %d %q", response.StatusCode, body)
	}
}

// An ordered chain is two origins for one index, and the schema forbade it for every
// project but one.
//
// UNIQUE (project, eco, name) looked right and was not: CreateUpstream stores the global
// project as NULL, SQLite treats NULLs as distinct, so global could hold a full chain
// while a named project failed on its second row with a constraint error. Nothing caught
// it because every two-tier chain anyone had written was global's, and the per-project
// tests used -no-direct, which writes a single row. Fixed by schema v5.
func TestNamedProjectHoldsAnOrderedChain(t *testing.T) {
	a := configuredApp(t, nil)
	if _, err := a.Projects.Create("work", ""); err != nil {
		t.Fatal(err)
	}
	rows := []control.Upstream{
		{Eco: "npm", Name: "registry", URL: "https://cache.internal/global/npm",
			Kind: "origin", Priority: 10, Enabled: true},
		{Eco: "npm", Name: "registry", URL: "https://registry.npmjs.org",
			Kind: "origin", Priority: 20, Enabled: true},
	}
	for _, row := range rows {
		if _, err := a.Projects.AddUpstream("work", row); err != nil {
			t.Fatalf("adding %s at priority %d: %v", row.Name, row.Priority, err)
		}
	}
	chain, err := a.Projects.Upstreams("work")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 {
		t.Fatalf("work holds %d rows, want the chain's 2", len(chain))
	}

	// Two origins at the same position are still a contradiction, and still refused.
	duplicate := rows[0]
	duplicate.URL = "https://somewhere.else"
	if _, err := a.Projects.AddUpstream("work", duplicate); err == nil {
		t.Error("a second origin at the same priority for the same index was accepted")
	}
}

// An event stream never ends by itself, and http.Server.Shutdown waits for active
// handlers — so one widget window left open made every `pkgcache stop`, every `setup` and
// the idle exit itself wait out the whole shutdown grace period. Measured at 30 seconds
// before the fix and 67 milliseconds after it.
func TestEventStreamEndsWhenTheProcessIsClosing(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", response.StatusCode)
	}

	// Reading blocks until the handler returns, which is the thing being asserted: before
	// this the only way out was the client hanging up or the grace period expiring.
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		ended <- err
	}()
	a.API.Close()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream outlived the process closing")
	}
}
