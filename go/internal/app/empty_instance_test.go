package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every other API test creates something first, so none of them ever saw what a fresh
// instance actually serves. It served {"jobs":null} — Go marshals a nil slice as null —
// and the console died on the first `.filter` with "Cannot read properties of null".
//
// A field the client iterates must be an array whenever it is present. This walks the
// read surface of an instance that has never been used and asserts exactly that.
func TestFreshInstanceNeverServesNullWhereAListIsExpected(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	// key -> the JSON field that must be a list. Named explicitly rather than inferred,
	// so a genuinely nullable field (onboarding, which is absent without TLS) is not
	// swept in by accident.
	cases := []struct {
		path   string
		fields []string
	}{
		{"/api/v1/ecosystems", []string{"ecosystems"}},
		{"/api/v1/projects", []string{"projects"}},
		{"/api/v1/jobs", []string{"jobs"}},
		{"/api/v1/users", []string{"users"}},
		{"/api/v1/audit", []string{"audit"}},
		{"/api/v1/tokens?project=global", []string{"tokens"}},
		{"/api/v1/projects/global/upstreams", []string{"upstreams"}},
		{"/api/v1/projects/global/snapshots", []string{"snapshots"}},
		{"/api/v1/projects/global/artifacts", []string{"artifacts"}},
		{"/api/v1/stats?project=global", []string{
			"by_eco", "leaderboard", "top_largest", "recent_added",
		}},
		{"/api/v1/stats/series?project=global&by=outcome", []string{"points"}},
		{"/api/v1/stats/storage", []string{"samples"}},
		{"/api/v1/stats/upstreams?project=global", []string{"points"}},
		{"/api/v1/stats/ages?project=global", []string{"buckets"}},
		{"/api/v1/projects/global/artifacts", []string{"artifacts"}},
	}

	for _, test := range cases {
		response, body := controlRequest(t, server.Client(), http.MethodGet,
			server.URL+test.path, cookie, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", test.path, response.StatusCode, body)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: %v (%s)", test.path, err, body)
		}
		for _, field := range test.fields {
			raw, present := payload[field]
			if !present {
				t.Errorf("%s: field %q is missing entirely", test.path, field)
				continue
			}
			trimmed := strings.TrimSpace(string(raw))
			if trimmed == "null" {
				t.Errorf("%s: %q is null; a list field must be [] when empty", test.path, field)
				continue
			}
			if !strings.HasPrefix(trimmed, "[") {
				t.Errorf("%s: %q is not an array: %s", test.path, field, trimmed)
			}
		}
	}
}

// The legacy shim answers the same way. It is what the retired console used, and
// anything still pointed at it deserves the same guarantee.
func TestFreshInstanceLegacyEndpointsAlsoReturnArrays(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	for path, field := range map[string]string{
		"/api/projects": "projects",
		"/api/jobs":     "jobs",
	} {
		response, body := controlRequest(t, server.Client(), http.MethodGet,
			server.URL+path, cookie, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", path, response.StatusCode, body)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := strings.TrimSpace(string(payload[field])); got == "null" {
			t.Errorf("%s: %q is null", path, field)
		}
	}
}

// A fresh instance must also serve the console itself — the page, not just the API.
func TestFreshInstanceServesTheConsoleShell(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	response := getResponse(t, server.Client(), server.URL+"/console")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/console = %d", response.StatusCode)
	}
	if !strings.Contains(string(body), "/console/boot.js") {
		t.Fatalf("the shell does not load the console: %.200s", body)
	}
}
