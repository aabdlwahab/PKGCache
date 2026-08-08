package app

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
)

func configuredApp(t *testing.T, mutate func(*config.Snapshot)) *App {
	t.Helper()
	snapshot := config.Defaults()
	snapshot.DataDir = t.TempDir()
	snapshot.Log.Level = "error"
	if mutate != nil {
		mutate(&snapshot)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	a, err := Open(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestUnifiedNamespaceAndLiveProjectResolution(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Upstream.Offline = true
		snapshot.Projects = map[string]config.Project{
			"team-a": {Name: "team-a"},
		}
	})
	for _, project := range []string{"global", "team-a", "ghost"} {
		if _, err := a.Engine.PutBytes(project, "files", "seed.txt",
			[]byte(project), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(a.UnifiedHandler())
	defer server.Close()

	for _, path := range []string{
		"/global/files/seed.txt",
		"/team-a/files/seed.txt",
		"/v2/",
	} {
		response := getResponse(t, server.Client(), server.URL+path)
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("%s = %d %q", path, response.StatusCode, body)
		}
		response.Body.Close()
	}

	response := getResponse(t, server.Client(),
		server.URL+"/v2/team-a/dockerhub/library/alpine/tags/list")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), `"name":"team-a/dockerhub/library/alpine"`) {
		t.Fatalf("named OCI project = %d %q", response.StatusCode, body)
	}

	for path, want := range map[string]string{
		"/ghost/files/": "unknown project \"ghost\"",
		"/v2/ghost/dockerhub/library/alpine/manifests/latest": "unknown project \"ghost\"",
		"/left-pad": "default project is 'global'",
	} {
		response := getResponse(t, server.Client(), server.URL+path)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound || !strings.Contains(string(body), want) {
			t.Errorf("%s = %d %q, want 404 containing %q",
				path, response.StatusCode, body, want)
		}
	}

	// Config publication is atomic and read on every request: a newly registered
	// tenant is routable immediately, without rebuilding the handler.
	if err := a.Config.SetProjects(map[string]config.Project{
		"team-a": {Name: "team-a"},
		"ghost":  {Name: "ghost"},
	}); err != nil {
		t.Fatal(err)
	}
	response = getResponse(t, server.Client(), server.URL+"/ghost/files/")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("new project was not visible on next request: %d", response.StatusCode)
	}
}

func TestUnifiedConsoleHealthAndWrongProxyPort(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.UnifiedHandler())
	defer server.Close()

	response := getResponse(t, server.Client(), server.URL+"/")
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(response.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(string(body), "<!doctype html>") {
		t.Fatalf("unified console = %d %q", response.StatusCode, body)
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("unified console has no CSP")
	}

	response = getResponse(t, server.Client(), server.URL+"/healthz")
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if health["server"] != "unified" {
		t.Fatalf("unified health = %+v", health)
	}

	request := httptest.NewRequest(http.MethodGet, "http://packages.example.test/a.deb", nil)
	recorder := httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), a.Config.Current().Server.ProxyAddr) {
		t.Fatalf("absolute request = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestProxyProjectResolutionAndCatalogScope(t *testing.T) {
	var hits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("deb bytes"))
	}))
	defer origin.Close()

	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Projects = map[string]config.Project{"team-a": {Name: "team-a"}}
	})

	request := httptest.NewRequest(http.MethodGet, origin.URL+"/pool/demo_1.0_amd64.deb", nil)
	request.Header.Set("Proxy-Authorization", basicProxyAuth("team-a"))
	recorder := httptest.NewRecorder()
	a.ProxyHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "deb bytes" {
		t.Fatalf("known proxy project = %d %q", recorder.Code, recorder.Body.String())
	}
	entries, err := a.Catalog.ListEntries(catalog.EntryQuery{Project: "team-a", Eco: "apt"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("team-a apt entries = %+v, err=%v", entries, err)
	}

	request = httptest.NewRequest(http.MethodGet, origin.URL+"/pool/other.deb", nil)
	request.Header.Set("Proxy-Authorization", basicProxyAuth("ghost"))
	recorder = httptest.NewRecorder()
	a.ProxyHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound ||
		!strings.Contains(recorder.Body.String(), "unknown project \"ghost\"") {
		t.Fatalf("unknown proxy project = %d %q", recorder.Code, recorder.Body.String())
	}
	if hits != 1 {
		t.Fatalf("unknown proxy project reached origin; hits=%d", hits)
	}

	request = httptest.NewRequest(http.MethodConnect, "http://example.test:443", nil)
	recorder = httptest.NewRecorder()
	a.ProxyHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("CONNECT status = %d", recorder.Code)
	}
}

func TestProjectPrefixPreservesEscaping(t *testing.T) {
	a := configuredApp(t, nil)
	server := httptest.NewServer(a.UnifiedHandler())
	defer server.Close()

	// The encoded slash must remain one npm package-name capture after the project
	// prefix is removed. Offline mode makes the expected endpoint miss deterministic.
	a.Config.Apply(func(snapshot *config.Snapshot) error {
		snapshot.Upstream.Offline = true
		return nil
	})
	response := getResponse(t, server.Client(), server.URL+"/global/npm/@scope%2Fpkg")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound ||
		!strings.Contains(string(body), "not cached") {
		t.Fatalf("escaped scoped package = %d %q", response.StatusCode, body)
	}
}

func getResponse(t *testing.T, client *http.Client, rawURL string) *http.Response {
	t.Helper()
	response, err := client.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func basicProxyAuth(project string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(project+":ignored"))
}

func proxyURL(t *testing.T, address, project string) *url.URL {
	t.Helper()
	raw := "http://" + address
	if project != "" {
		raw = "http://" + url.User(project).String() + "@" + address
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
