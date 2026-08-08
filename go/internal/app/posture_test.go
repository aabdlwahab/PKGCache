package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
)

// The P0 invariants, as tests, so that the default deployment posture cannot regress
// quietly. Each one reproduces something an audit found on a running instance.

// TestSinglePortPlainBranchDoesNotServeTheAdminNamespace pins the fix for the worst of
// them: a single port that speaks TLS also answered origin-form plain HTTP with the
// console, the metrics endpoint and the entire control API, on an address the product's
// own pages advertise as an https origin.
func TestSinglePortPlainBranchDoesNotServeTheAdminNamespace(t *testing.T) {
	a, _ := appWithTLS(t, true)
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})
	address := runtime.Addresses()["single"]

	// Never follow the redirect: the assertion is that one is issued, and following it
	// would put the client back on the TLS side where everything is allowed.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, path := range []string{
		"/", "/console", "/tutorial", "/metrics", "/version", "/readyz",
		"/api/v1/projects", "/api/v1/login", "/api/v1/me",
	} {
		response := getResponse(t, client, "http://"+address+path)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusPermanentRedirect {
			t.Errorf("plain http %s = %d %q; want 308 to https", path,
				response.StatusCode, body)
			continue
		}
		want := "https://" + address + path
		if got := response.Header.Get("Location"); got != want {
			t.Errorf("plain http %s redirected to %q, want %q", path, got, want)
		}
	}

	// Liveness is the deliberate exception, and it must keep answering.
	response := getResponse(t, client, "http://"+address+"/healthz")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("plain http /healthz = %d; probes must not be redirected",
			response.StatusCode)
	}
}

// TestSinglePortPlainBranchStillProxiesForApt guards the reason the cleartext branch
// exists at all. apt and apk cannot speak to a TLS proxy, so hardening the plain side
// must not take the forward proxy with it.
func TestSinglePortPlainBranchStillProxiesForApt(t *testing.T) {
	a, _ := appWithTLS(t, true)
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})
	address := runtime.Addresses()["single"]

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("still proxying"))
	}))
	defer origin.Close()

	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL(t, address, "team-a"))}
	defer transport.CloseIdleConnections()
	response := getResponse(t, &http.Client{Transport: transport},
		origin.URL+"/pool/pkg_1.0_amd64.deb")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "still proxying" {
		t.Fatalf("apt proxy through the plain branch = %d %q", response.StatusCode, body)
	}
}

// TestPlainOnlyDeploymentKeepsItsWholeNamespace is the other side of the same change.
// A process with no certificate pair is plaintext on purpose — behind a TLS-terminating
// proxy, or on a trusted network — and has no https to redirect to. Redirecting there
// would be an outage, not a hardening.
func TestPlainOnlyDeploymentKeepsItsWholeNamespace(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Server.SinglePort = true
		snapshot.Server.UnifiedAddr = "127.0.0.1:0"
		// No TLS cert or key: config.Defaults carries none, and this deployment
		// deliberately does not add any.
	})
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})
	address := runtime.Addresses()["single"]

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, path := range []string{"/", "/healthz", "/readyz", "/metrics", "/version"} {
		response := getResponse(t, client, "http://"+address+path)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("plain-only deployment http %s = %d; want 200 (nothing to redirect to)",
				path, response.StatusCode)
		}
	}
}

// TestCleartextLoginRefusedWhenTLSIsAvailable covers the defence in depth behind the
// listener redirect. The listener stops this on the single port; the API stops it
// wherever a future listener layout, an embedding or a misconfiguration routes a
// password exchange over cleartext on a host that terminates TLS.
//
// The observed failure was worse than an unencrypted password: the response also
// carried a twelve-hour session cookie, necessarily without Secure, so a passive
// observer got a replayable session as well as the credential.
func TestCleartextLoginRefusedWhenTLSIsAvailable(t *testing.T) {
	a, _ := appWithTLS(t, true)
	if err := a.Config.Apply(func(next *config.Snapshot) error {
		next.Auth.RootUser = "root"
		next.Auth.RootPassword = "rootpass12"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// httptest serves plain HTTP, which is exactly the condition under test: a request
	// with no TLS state reaching a process whose configuration terminates TLS.
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	response, body := postJSON(t, server.Client(), server.URL+"/api/v1/login",
		`{"username":"root","password":"rootpass12"}`)
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("cleartext login = %d %q; want 426", response.StatusCode, body)
	}
	for _, cookie := range response.Cookies() {
		t.Fatalf("a refused cleartext login still issued a cookie: %s", cookie.Name)
	}
	if !strings.Contains(string(body), "https") {
		t.Errorf("refusal does not tell the caller what to do instead: %q", body)
	}
}

// TestPlainDeploymentStillAcceptsLogin pins the exemption. Refusing here would lock
// every TLS-terminated-in-front deployment out of its own console.
func TestPlainDeploymentStillAcceptsLogin(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Auth.RootUser = "root"
		snapshot.Auth.RootPassword = "rootpass12"
	})
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()

	response, body := postJSON(t, server.Client(), server.URL+"/api/v1/login",
		`{"username":"root","password":"rootpass12"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login on a deliberately plain deployment = %d %q", response.StatusCode, body)
	}
}

func postJSON(
	t *testing.T, client *http.Client, target, payload string,
) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response, body
}
