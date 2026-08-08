package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control/api"
	"github.com/brightskies/pkgreg/internal/control/auth"
	"github.com/brightskies/pkgreg/internal/pki"
)

// Guest sessions are a deliberate hole in a control plane that was just closed, so the
// tests are written as confinement rather than as feature coverage: the interesting
// assertions are all about what a guest cannot reach.

func guestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Auth.RootUser = "root"
		snapshot.Auth.RootPassword = "rootpass12"
		snapshot.Auth.GuestRead = true
		// Connect offers the generated setup script, which embeds the public CA, so
		// the fixture needs one or that route 404s for reasons unrelated to guests.
		ca, _, err := pki.LoadOrCreateCA(snapshot.CertsDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ca.IssueServer([]string{"localhost"}); err != nil {
			t.Fatal(err)
		}
		snapshot.Server.TLS.CAFile = filepath.Join(snapshot.CertsDir(), pki.CACertFile)
	})
	server := httptest.NewServer(a.AdminHandler())
	t.Cleanup(server.Close)
	return a, server
}

// guestSession signs in as a guest and returns the cookie.
func guestSession(t *testing.T, server *httptest.Server) *http.Cookie {
	t.Helper()
	response, body := postJSON(t, server.Client(), server.URL+"/api/v1/login/guest", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("guest login = %d %q", response.StatusCode, body)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("guest login set %d cookies, want 1", len(cookies))
	}
	return cookies[0]
}

func as(t *testing.T, server *httptest.Server, cookie *http.Cookie, method, path, body string) int {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode
}

// TestGuestCanReadTheGlobalProject covers the point of the feature: a visitor with no
// account sees what is cached and how to connect to it.
func TestGuestCanReadTheGlobalProject(t *testing.T) {
	_, server := guestApp(t)
	cookie := guestSession(t, server)

	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/ecosystems",
		"/api/v1/projects",
		"/api/v1/projects/global",
		"/api/v1/projects/global/artifacts",
		"/api/v1/projects/global/endpoints",
		"/api/v1/projects/global/setup.sh",
		"/api/v1/stats",
		"/api/v1/stats/series",
		"/api/v1/stats/storage",
		"/api/v1/stats/ages",
		"/api/v1/downloads",
		"/api/v1/coordinates",
	} {
		if got := as(t, server, cookie, http.MethodGet, path, ""); got != http.StatusOK {
			t.Errorf("guest GET %s = %d, want 200", path, got)
		}
	}
}

// TestGuestReceivesTheLiveStream. The stream is not in the loop above because it never
// ends: draining it would hang the test. It is reachable only because eventFilter
// scopes a guest to the global project and withholds audit and job frames — without
// that filter this route would publish every tenant's activity to anyone who clicked
// "browse as guest", which is why it was originally denied outright.
func TestGuestReceivesTheLiveStream(t *testing.T) {
	_, server := guestApp(t)
	cookie := guestSession(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	// Headers only: the body stays open until the context expires, which is the
	// point of a stream.
	defer func() {
		cancel()
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("guest GET /api/v1/events = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}

// TestGuestCannotReachOperationalSurfaces is the confinement that matters most: these
// carry credentials, account names, internal mirror hostnames and air-gap cadence.
func TestGuestCannotReachOperationalSurfaces(t *testing.T) {
	_, server := guestApp(t)
	cookie := guestSession(t, server)

	for _, path := range []string{
		"/api/v1/users",
		"/api/v1/audit",
		"/api/v1/tokens",
		"/api/v1/jobs",
		"/api/v1/projects/global/upstreams",
		"/api/v1/projects/global/snapshots",
		"/api/v1/stats/upstreams",
	} {
		if got := as(t, server, cookie, http.MethodGet, path, ""); got != http.StatusForbidden {
			t.Errorf("guest GET %s = %d, want 403", path, got)
		}
	}
}

func TestGuestCannotMutateAnything(t *testing.T) {
	_, server := guestApp(t)
	cookie := guestSession(t, server)

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/projects", `{"name":"guest-made-this"}`},
		{http.MethodPatch, "/api/v1/projects/global", `{"offline":true}`},
		{http.MethodDelete, "/api/v1/projects/global", ""},
		{http.MethodPost, "/api/v1/projects/global/upstreams", `{"eco":"npm","name":"x","url":"https://x"}`},
		{http.MethodPost, "/api/v1/projects/global/snapshots", ""},
		{http.MethodPost, "/api/v1/projects/global/export", ""},
		{http.MethodPost, "/api/v1/projects/global/import", ""},
		{http.MethodPost, "/api/v1/projects/global/lockwarm", ""},
		{http.MethodPost, "/api/v1/projects/global/maintenance/evict", ""},
		{http.MethodPost, "/api/v1/maintenance/gc", ""},
		{http.MethodPost, "/api/v1/tokens", `{"project":"global","scope":"read"}`},
		{http.MethodPost, "/api/v1/users", `{"username":"x","password":"abcdefgh","role":"admin"}`},
		{http.MethodPatch, "/api/v1/users/root", `{"role":"user"}`},
		{http.MethodDelete, "/api/v1/users/root", ""},
	}
	for _, tc := range cases {
		got := as(t, server, cookie, tc.method, tc.path, tc.body)
		if got != http.StatusForbidden {
			t.Errorf("guest %s %s = %d, want 403", tc.method, tc.path, got)
		}
	}

	// Logout is the one mutation a guest may perform: it ends their own session and
	// nothing else. Without it a guest cannot leave.
	if got := as(t, server, cookie, http.MethodPost, "/api/v1/logout", ""); got != http.StatusOK {
		t.Errorf("guest logout = %d, want 200 — a guest must be able to leave", got)
	}
}

// TestGuestCannotLeaveTheGlobalProject checks both shapes: a project named in the path
// is refused, and a project named in a query is rewritten rather than honoured. The
// rewrite is the subtle one — without it, an absent project on /stats means "the whole
// instance" and a guest would receive every tenant's totals.
func TestGuestCannotLeaveTheGlobalProject(t *testing.T) {
	a, server := guestApp(t)
	if _, err := a.Projects.Create("secret-team", "root"); err != nil {
		t.Fatal(err)
	}
	cookie := guestSession(t, server)

	for _, path := range []string{
		"/api/v1/projects/secret-team",
		"/api/v1/projects/secret-team/artifacts",
		"/api/v1/projects/secret-team/endpoints",
		"/api/v1/projects/secret-team/setup.sh",
	} {
		if got := as(t, server, cookie, http.MethodGet, path, ""); got != http.StatusForbidden {
			t.Errorf("guest GET %s = %d, want 403", path, got)
		}
	}

	// The project list must not even name the other tenant: a switcher full of doors
	// that all answer 403 is worse than one door.
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects", nil)
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(payload.Projects))
	for _, project := range payload.Projects {
		names = append(names, project.Name)
	}
	if !slices.Equal(names, []string{config.GlobalProject}) {
		t.Errorf("guest project list = %v, want only [global]", names)
	}
}

// TestGuestStatsQueryIsRewrittenNotHonoured pins the rewrite specifically, because a
// refusal here would have been the safe-looking wrong answer: the console asks for
// stats without a project on first paint, and refusing would break the Overview for
// every guest while a rewrite quietly gives them their own slice.
func TestGuestStatsQueryIsRewrittenNotHonoured(t *testing.T) {
	a, server := guestApp(t)
	if _, err := a.Projects.Create("secret-team", "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.PutBytes("secret-team", "files", "hidden.txt",
		[]byte("classified"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.PutBytes(config.GlobalProject, "files", "public.txt",
		[]byte("fine"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	cookie := guestSession(t, server)

	for _, target := range []string{
		"/api/v1/stats",
		"/api/v1/stats?project=secret-team",
		"/api/v1/stats?project=",
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+target, nil)
		request.AddCookie(cookie)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("guest GET %s = %d %q, want 200", target, response.StatusCode, body)
			continue
		}
		if strings.Contains(string(body), "secret-team") {
			t.Errorf("guest GET %s leaked another project: %s", target, body)
		}
	}
}

// TestGuestNameIsReserved: Guard.Actor resolves this name without consulting the
// database, so an account holding it would either be unreachable or would inherit the
// guest path's permissions.
func TestGuestNameIsReserved(t *testing.T) {
	_, server := guestApp(t)
	admin := signIn(t, server, "root", "rootpass12")

	got := as(t, server, admin, http.MethodPost, "/api/v1/users",
		`{"username":"guest","password":"abcdefgh","role":"user","reports_to":"root"}`)
	if got != http.StatusBadRequest {
		t.Errorf("creating an account named guest = %d, want 400", got)
	}
	// And the name must not be a password login of its own.
	response, _ := postJSON(t, server.Client(), server.URL+"/api/v1/login",
		`{"username":"guest","password":"guest"}`)
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("password login as guest = %d, want 401", response.StatusCode)
	}
}

// TestGuestDisabledByConfiguration covers the toggle, including the case that matters
// operationally: turning it off must invalidate sessions already minted, not merely
// stop new ones.
func TestGuestDisabledByConfiguration(t *testing.T) {
	a, server := guestApp(t)
	cookie := guestSession(t, server)
	if got := as(t, server, cookie, http.MethodGet, "/api/v1/me", ""); got != http.StatusOK {
		t.Fatalf("guest /me before disabling = %d", got)
	}

	if err := a.Config.Apply(func(next *config.Snapshot) error {
		next.Auth.GuestRead = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := as(t, server, nil, http.MethodPost, "/api/v1/login/guest", ""); got != http.StatusForbidden {
		t.Errorf("guest login while disabled = %d, want 403", got)
	}
	if got := as(t, server, cookie, http.MethodGet, "/api/v1/me", ""); got != http.StatusUnauthorized {
		t.Errorf("an existing guest session survived disabling: /me = %d, want 401", got)
	}
}

// TestUnauthenticatedMeAdvertisesGuestAvailability: the sign-in screen has no other way
// to learn whether to draw the button, because every endpoint that could tell it is
// behind the check that just refused it.
func TestUnauthenticatedMeAdvertisesGuestAvailability(t *testing.T) {
	a, server := guestApp(t)

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/me", nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /me = %d, want 401", response.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["guest_available"] != true {
		t.Errorf("401 body did not advertise guest availability: %s", body)
	}
	// The refusal must still describe itself as a refusal.
	if payload["code"] != "authentication_required" {
		t.Errorf("detail overwrote the error code: %s", body)
	}

	if err := a.Config.Apply(func(next *config.Snapshot) error {
		next.Auth.GuestRead = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response, err = server.Client().Do(request.Clone(request.Context()))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["guest_available"] != false {
		t.Errorf("guest availability still advertised after disabling: %s", body)
	}
}

// TestAccountSessionsAreUnaffectedByGuestSupport is the regression guard in the other
// direction. Confining guests must not narrow anybody else.
func TestAccountSessionsAreUnaffectedByGuestSupport(t *testing.T) {
	_, server := guestApp(t)
	admin := signIn(t, server, "root", "rootpass12")

	for _, path := range []string{
		"/api/v1/users", "/api/v1/audit", "/api/v1/tokens", "/api/v1/jobs",
		"/api/v1/projects/global/upstreams", "/api/v1/projects/global/snapshots",
		"/api/v1/stats/upstreams",
	} {
		if got := as(t, server, admin, http.MethodGet, path, ""); got != http.StatusOK {
			t.Errorf("root GET %s = %d, want 200", path, got)
		}
	}
	if got := as(t, server, admin, http.MethodPost, "/api/v1/projects",
		`{"name":"team-b"}`); got != http.StatusCreated {
		t.Errorf("root creating a project = %d, want 201", got)
	}
}

// TestEveryGuestRouteExists keeps the allowlist honest: a renamed route would silently
// drop out of it and quietly remove a guest's access, which is a failure nobody would
// notice until someone clicked.
func TestEveryGuestRouteExists(t *testing.T) {
	registered := api.RegisteredRoutes()
	for pattern := range api.GuestRoutes() {
		if !slices.Contains(registered, pattern) {
			t.Errorf("guest allowlist names %q, which is not a registered route", pattern)
		}
	}
}

func signIn(t *testing.T, server *httptest.Server, username, password string) *http.Cookie {
	t.Helper()
	response, body := postJSON(t, server.Client(), server.URL+"/api/v1/login",
		`{"username":"`+username+`","password":"`+password+`"}`)
	if response.StatusCode != http.StatusOK || len(response.Cookies()) != 1 {
		t.Fatalf("login = %d %q", response.StatusCode, body)
	}
	if response.Cookies()[0].Name != auth.SessionCookie {
		t.Fatalf("unexpected cookie %q", response.Cookies()[0].Name)
	}
	return response.Cookies()[0]
}
