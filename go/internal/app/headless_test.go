package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Headless removes the browser surface and nothing else. The failure this guards
// against is disabling the console by unmounting its routes, which in single-port
// mode would drop "/" through to the data plane and have it read as a package
// request rather than answered as "there is no console here".
func TestHeadlessKeepsEveryMachineFacingSurface(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Server.Headless = true
	})
	if a.Console.Enabled() {
		t.Fatal("console enabled under headless")
	}
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	for path, want := range map[string]int{
		"/":                      http.StatusNotFound,
		"/console":               http.StatusNotFound,
		"/tutorial":              http.StatusNotFound,
		"/healthz":               http.StatusOK,
		"/readyz":                http.StatusOK,
		"/version":               http.StatusOK,
		"/metrics":               http.StatusOK,
		"/api/v1/me":             http.StatusOK,
		"/api/v1/ecosystems":     http.StatusOK,
		"/api/v1/does-not-exist": http.StatusNotFound,
	} {
		response := getResponse(t, server.Client(), server.URL+path)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s = %d, want %d (%s)", path, response.StatusCode, want, body)
		}
	}
}

// The same build serves the console when headless is off, so the flag is the only
// thing that decides — not what the binary happens to contain.
func TestConsoleServesWhenHeadlessIsOff(t *testing.T) {
	a := configuredApp(t, nil)
	if !a.Console.Enabled() {
		t.Fatal("console disabled by default")
	}
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	response := getResponse(t, server.Client(), server.URL+"/console")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/console = %d", response.StatusCode)
	}
	if !strings.Contains(string(body), "<html") {
		t.Fatalf("/console did not return a document: %.120s", body)
	}
}

// In single-port mode the console shares an address with the data plane, so headless
// has to stay a console-layer decision: "/" must still be recognised as a console
// path and refused, never handed to the package router.
func TestHeadlessSinglePortDoesNotLeakIntoTheDataPlane(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Server.Headless = true
		snapshot.Server.SinglePort = true
	})
	server := httptest.NewServer(a.UnifiedHandler())
	defer server.Close()

	response := getResponse(t, server.Client(), server.URL+"/")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("/ = %d, want 404", response.StatusCode)
	}
	if !strings.Contains(string(body), "/api/v1") {
		t.Fatalf("/ was answered by something other than the console handler: %.160s", body)
	}
	// The data plane is unaffected.
	if response := getResponse(t, server.Client(), server.URL+"/healthz"); response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("/healthz = %d", response.StatusCode)
	} else {
		response.Body.Close()
	}
}
