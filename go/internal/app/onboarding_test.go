package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/onboarding"
	"github.com/aabdlwahab/PKGCache/internal/pki"
)

func onboardingApp(t *testing.T, authEnabled bool) *App {
	t.Helper()
	snapshot := config.Defaults()
	snapshot.DataDir = t.TempDir()
	snapshot.Log.Level = "error"
	snapshot.Projects = map[string]config.Project{"team-a": {Name: "team-a"}}
	if authEnabled {
		snapshot.Auth.RootUser = "root"
		snapshot.Auth.RootPassword = "rootpass12"
	}
	ca, _, err := pki.LoadOrCreateCA(snapshot.CertsDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueServer([]string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	snapshot.Server.TLS.CAFile = filepath.Join(snapshot.CertsDir(), pki.CACertFile)
	app, err := Open(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func TestGeneratedSetupScriptDownloadRoutes(t *testing.T) {
	app := onboardingApp(t, false)
	server := httptest.NewServer(app.AdminHandler())
	defer server.Close()

	tests := []struct {
		path        string
		contentType string
		contains    string
	}{
		{
			path: "/api/v1/projects/team-a/setup.sh", contentType: "text/x-shellscript",
			contains: "PROJECT='team-a'",
		},
		{
			path: "/api/v1/projects/team-a/setup.ps1", contentType: "text/plain",
			contains: "$Project = 'team-a'",
		},
		{
			path: "/api/setup.sh?project=team-a", contentType: "text/x-shellscript",
			contains: "PROJECT='team-a'",
		},
	}
	for _, test := range tests {
		response, err := server.Client().Get(server.URL + test.path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d %s", test.path, response.StatusCode, body)
			continue
		}
		if !strings.HasPrefix(response.Header.Get("Content-Type"), test.contentType) {
			t.Errorf("%s content type = %q", test.path, response.Header.Get("Content-Type"))
		}
		if response.Header.Get("Cache-Control") != "no-store" ||
			!strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s download headers = %v", test.path, response.Header)
		}
		if !strings.Contains(string(body), test.contains) ||
			strings.Contains(string(body), "Authorization: Bearer") {
			t.Errorf("%s unexpected body", test.path)
		}
	}
}

func TestSetupScriptRequiresProjectVisibility(t *testing.T) {
	app := onboardingApp(t, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/team-a/setup.sh", nil)
	request.Host = "pkgcache.internal:8088"
	app.AdminHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous setup download = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestCAEndpointPublishesOutOfBandFingerprint(t *testing.T) {
	app := onboardingApp(t, false)
	recorder := httptest.NewRecorder()
	app.AdminHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/api/ca.crt", nil))
	fingerprint := recorder.Header().Get("X-Pkgreg-CA-SHA256")
	if recorder.Code != http.StatusOK || len(fingerprint) != 95 ||
		strings.Count(fingerprint, ":") != 31 {
		t.Fatalf("CA response = %d fingerprint=%q", recorder.Code, fingerprint)
	}
}

// The tutorial is a public page whose whole job is to hand a reader a start command,
// and that command is useless without the fingerprint. It used to read the fingerprint
// from the project endpoints route, which requires a login — so on every instance with
// authentication on, an anonymous reader was served a command containing the literal
// string PASTE_FINGERPRINT and no indication anything was missing. The fingerprint is a
// digest of the certificate this same server hands to anyone at /api/ca.crt, so there
// was nothing to protect; what it cost was the one step a newcomer cannot work around.
func TestCoordinatesServeTheFingerprintWithoutALogin(t *testing.T) {
	app := onboardingApp(t, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/coordinates", nil)
	request.Host = "pkgcache.internal:8443"
	app.AdminHandler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("anonymous coordinates = %d %s", recorder.Code, body)
	}
	var payload struct {
		Unified  string `json:"unified"`
		CASHA256 string `json:"ca_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode coordinates: %v: %s", err, body)
	}
	if len(payload.CASHA256) != 95 || strings.Count(payload.CASHA256, ":") != 31 {
		t.Fatalf("ca_sha256 = %q, want a colon-separated SHA-256", payload.CASHA256)
	}
	if payload.Unified != "pkgcache.internal:8443" {
		t.Errorf("unified = %q, want the address the request arrived on", payload.Unified)
	}
	// The same value the authenticated route reports, since one CA serves every
	// project — if these ever diverge, one of the two pages is telling people to pin
	// the wrong certificate.
	authenticated, err := onboardingFingerprint(t, app)
	if err != nil {
		t.Fatalf("read the authenticated fingerprint: %v", err)
	}
	if authenticated != payload.CASHA256 {
		t.Errorf("coordinates report %q but the project route reports %q",
			payload.CASHA256, authenticated)
	}
}

func onboardingFingerprint(t *testing.T, app *App) (string, error) {
	t.Helper()
	caPEM, err := os.ReadFile(app.Config.Current().Server.TLS.CAFile)
	if err != nil {
		return "", err
	}
	return onboarding.FingerprintSHA256(caPEM)
}

func TestSetupScriptRejectsUntrustedHostHeader(t *testing.T) {
	app := onboardingApp(t, false)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/team-a/setup.sh", nil)
	request.Host = "cache.example.test;touch-pwned"
	app.AdminHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_host"`) {
		t.Fatalf("unsafe host = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEndpointInstructionsUseTheClientAndRealHost(t *testing.T) {
	app := onboardingApp(t, false)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/team-a/endpoints", nil)
	request.Host = "pkgcache.internal:8443"
	app.AdminHandler().ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("endpoints = %d %s", recorder.Code, body)
	}
	for _, required := range []string{
		"pkgcache.internal:8443",
		"python -m pip install",
		"PKGREG_FILES_URL",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("endpoint instructions are missing %q: %s", required, body)
		}
	}
	for _, stale := range []string{"<host>", "trusted-host", "strict-ssl=false"} {
		if strings.Contains(body, stale) {
			t.Errorf("endpoint instructions still contain %q: %s", stale, body)
		}
	}
}
