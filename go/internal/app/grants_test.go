package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Project grants over HTTP. The unit tests in internal/control/auth cover the policy;
// these cover that every route actually consults it — an authorization rule the
// handlers do not reach is a rule that only exists in the test suite.

func grantsApp(t *testing.T) *httptest.Server {
	t.Helper()
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Auth.RootUser = "root"
		snapshot.Auth.RootPassword = "rootpass12"
	})
	server := httptest.NewServer(a.AdminHandler())
	t.Cleanup(server.Close)
	return server
}

// account creates one and returns a signed-in cookie for it.
func account(t *testing.T, server *httptest.Server, root *http.Cookie, name, role string) *http.Cookie {
	t.Helper()
	body := `{"username":"` + name + `","password":"` + name + `pass1234","role":"` + role + `"}`
	if got := as(t, server, root, http.MethodPost, "/api/v1/users", body); got != http.StatusCreated {
		t.Fatalf("creating %s = %d", name, got)
	}
	return signIn(t, server, name, name+"pass1234")
}

func TestGrantedAdminOperatesAProjectTheyDoNotOwn(t *testing.T) {
	server := grantsApp(t)
	root := signIn(t, server, "root", "rootpass12")
	alice := account(t, server, root, "alice", "admin")
	bob := account(t, server, root, "bob", "admin")

	if got := as(t, server, alice, http.MethodPost, "/api/v1/projects",
		`{"name":"team-a"}`); got != http.StatusCreated {
		t.Fatalf("alice creating her project = %d", got)
	}

	// Before the grant bob cannot even see it, and the project list must not name it:
	// a tenant nobody may reach is a tenant whose existence is not theirs to learn.
	if got := as(t, server, bob, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusForbidden {
		t.Errorf("ungranted admin GET project = %d, want 403", got)
	}
	if names := projectNames(t, server, bob); slices.Contains(names, "team-a") {
		t.Errorf("ungranted admin sees projects %v, which names team-a", names)
	}

	if got := as(t, server, alice, http.MethodPut, "/api/v1/projects/team-a/grants/bob",
		`{"level":"operate"}`); got != http.StatusOK {
		t.Fatalf("alice granting bob = %d", got)
	}

	for _, step := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/projects/team-a", "", http.StatusOK},
		{http.MethodGet, "/api/v1/projects/team-a/upstreams", "", http.StatusOK},
		{http.MethodPatch, "/api/v1/projects/team-a", `{"quota_bytes":1024}`, http.StatusOK},
		// Reassigning ownership stays a superuser act even with operate.
		{http.MethodPatch, "/api/v1/projects/team-a", `{"owner":"bob"}`, http.StatusForbidden},
		// So does removing the tenant, and so does changing who else may reach it.
		{http.MethodDelete, "/api/v1/projects/team-a", "", http.StatusForbidden},
		{http.MethodPut, "/api/v1/projects/team-a/grants/root", `{"level":"view"}`, http.StatusForbidden},
		{http.MethodDelete, "/api/v1/projects/team-a/grants/bob", "", http.StatusForbidden},
	} {
		if got := as(t, server, bob, step.method, step.path, step.body); got != step.want {
			t.Errorf("granted admin %s %s = %d, want %d", step.method, step.path, got, step.want)
		}
	}
	if names := projectNames(t, server, bob); !slices.Contains(names, "team-a") {
		t.Errorf("granted admin sees projects %v, which omits team-a", names)
	}
}

func TestViewGrantIsReadOnlyOverHTTP(t *testing.T) {
	server := grantsApp(t)
	root := signIn(t, server, "root", "rootpass12")
	alice := account(t, server, root, "alice", "admin")
	bob := account(t, server, root, "bob", "admin")

	if got := as(t, server, alice, http.MethodPost, "/api/v1/projects",
		`{"name":"team-a"}`); got != http.StatusCreated {
		t.Fatalf("alice creating her project = %d", got)
	}
	if got := as(t, server, alice, http.MethodPut, "/api/v1/projects/team-a/grants/bob",
		`{"level":"view"}`); got != http.StatusOK {
		t.Fatalf("alice granting view = %d", got)
	}
	if got := as(t, server, bob, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusOK {
		t.Errorf("view grantee GET project = %d, want 200", got)
	}
	if got := as(t, server, bob, http.MethodPatch, "/api/v1/projects/team-a",
		`{"quota_bytes":1}`); got != http.StatusForbidden {
		t.Errorf("view grantee PATCH project = %d, want 403", got)
	}
}

func TestRevokingAGrantClosesAccessImmediately(t *testing.T) {
	server := grantsApp(t)
	root := signIn(t, server, "root", "rootpass12")
	alice := account(t, server, root, "alice", "admin")
	bob := account(t, server, root, "bob", "admin")

	if got := as(t, server, alice, http.MethodPost, "/api/v1/projects",
		`{"name":"team-a"}`); got != http.StatusCreated {
		t.Fatalf("alice creating her project = %d", got)
	}
	if got := as(t, server, alice, http.MethodPut, "/api/v1/projects/team-a/grants/bob",
		`{"level":"operate"}`); got != http.StatusOK {
		t.Fatalf("grant = %d", got)
	}
	// The session bob is holding was minted before the grant and outlives the revoke,
	// which is the case worth asserting: authorization is read per request, not pinned
	// into the session at sign-in.
	if got := as(t, server, bob, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusOK {
		t.Fatalf("granted GET = %d", got)
	}
	if got := as(t, server, alice, http.MethodDelete,
		"/api/v1/projects/team-a/grants/bob", ""); got != http.StatusOK {
		t.Fatalf("revoke = %d", got)
	}
	if got := as(t, server, bob, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusForbidden {
		t.Errorf("revoked GET = %d, want 403", got)
	}
}

// The console draws a project's controls from /me before it has touched that project,
// so the grants have to arrive with the identity.
func TestMeReportsTheActorsGrants(t *testing.T) {
	server := grantsApp(t)
	root := signIn(t, server, "root", "rootpass12")
	alice := account(t, server, root, "alice", "admin")
	bob := account(t, server, root, "bob", "admin")

	if got := as(t, server, alice, http.MethodPost, "/api/v1/projects",
		`{"name":"team-a"}`); got != http.StatusCreated {
		t.Fatalf("alice creating her project = %d", got)
	}
	if got := as(t, server, alice, http.MethodPut, "/api/v1/projects/team-a/grants/bob",
		`{"level":"operate"}`); got != http.StatusOK {
		t.Fatalf("grant = %d", got)
	}
	var me struct {
		Grants map[string]string `json:"grants"`
	}
	decodeAs(t, server, bob, "/api/v1/me", &me)
	if me.Grants["team-a"] != "operate" {
		t.Errorf("/me grants = %v, want team-a: operate", me.Grants)
	}
}

// A user assigned to an admin reads that admin's projects and changes nothing —
// the relationship this feature's other half configures.
func TestUserAssignedToAnAdminSeesTheirProjectsReadOnly(t *testing.T) {
	server := grantsApp(t)
	root := signIn(t, server, "root", "rootpass12")
	alice := account(t, server, root, "alice", "admin")

	if got := as(t, server, root, http.MethodPost, "/api/v1/users",
		`{"username":"carol","password":"carolpass12","role":"user","reports_to":"alice"}`,
	); got != http.StatusCreated {
		t.Fatalf("creating carol under alice = %d", got)
	}
	carol := signIn(t, server, "carol", "carolpass12")
	if got := as(t, server, alice, http.MethodPost, "/api/v1/projects",
		`{"name":"team-a"}`); got != http.StatusCreated {
		t.Fatalf("alice creating her project = %d", got)
	}
	if got := as(t, server, carol, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusOK {
		t.Errorf("report GET project = %d, want 200", got)
	}
	if got := as(t, server, carol, http.MethodPatch, "/api/v1/projects/team-a",
		`{"quota_bytes":1}`); got != http.StatusForbidden {
		t.Errorf("report PATCH project = %d, want 403", got)
	}
	// Reassigning that user to a different manager is a superuser decision.
	if got := as(t, server, alice, http.MethodPatch, "/api/v1/users/carol",
		`{"reports_to":"root"}`); got != http.StatusForbidden {
		t.Errorf("admin reassigned a report = %d, want 403", got)
	}
	if got := as(t, server, root, http.MethodPatch, "/api/v1/users/carol",
		`{"reports_to":"root"}`); got != http.StatusOK {
		t.Errorf("superuser reassigning a report = %d, want 200", got)
	}
	if got := as(t, server, carol, http.MethodGet, "/api/v1/projects/team-a", ""); got != http.StatusForbidden {
		t.Errorf("reassigned report still reads the old manager's project = %d, want 403", got)
	}
}

func projectNames(t *testing.T, server *httptest.Server, cookie *http.Cookie) []string {
	t.Helper()
	var answer struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	decodeAs(t, server, cookie, "/api/v1/projects", &answer)
	names := make([]string, 0, len(answer.Projects))
	for _, project := range answer.Projects {
		names = append(names, project.Name)
	}
	return names
}

func decodeAs(t *testing.T, server *httptest.Server, cookie *http.Cookie, path string, into any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}
