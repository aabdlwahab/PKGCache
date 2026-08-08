package clientinstaller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/onboarding"
)

func testCA(t *testing.T, serial int64) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "pkgreg client test CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testTLSMaterial(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(100),
		Subject:      pkix.Name{CommonName: "pkgreg bootstrap test CA"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		IsCA:         true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(
		rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}

	leafPublic, leafPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(101),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(
		rand.Reader, leafTemplate, caTemplate, leafPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafPrivate,
		}
}

func testServer(t *testing.T, endpointCA, scriptCA []byte, requireCookie bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireCookie && r.Header.Get("Cookie") != "pkgreg_session=test-value" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/ca.crt":
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write(endpointCA)
		case r.URL.Path == "/api/v1/coordinates":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"proxy":"pkgcache.internal:3142"}`)
		case strings.HasSuffix(r.URL.Path, "/setup.sh"):
			script, err := onboarding.Shell(onboarding.Config{
				Project: "team-a", Host: "pkgcache.internal",
				UnifiedPort: 8443, ProxyPort: 3142, CAPEM: scriptCA,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/x-shellscript")
			_, _ = w.Write(script)
		case strings.HasSuffix(r.URL.Path, "/setup.ps1"):
			script, err := onboarding.PowerShell(onboarding.Config{
				Project: "team-a", Host: "pkgcache.internal",
				UnifiedPort: 8443, ProxyPort: 3142, CAPEM: scriptCA,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(script)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestFetchPinsCAAndSelectsPlatformScript(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, err := onboarding.FingerprintSHA256(ca)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		goos, extension, marker string
	}{
		{"linux", "sh", "#!/usr/bin/env bash"},
		{"darwin", "sh", "#!/usr/bin/env bash"},
		{"windows", "ps1", "[CmdletBinding()]"},
	} {
		t.Run(test.goos, func(t *testing.T) {
			bundle, err := Fetch(context.Background(), Options{
				Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
				OperatingSystem: test.goos, Client: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Extension != test.extension ||
				!strings.Contains(string(bundle.Script), test.marker) ||
				bundle.Fingerprint != fingerprint {
				t.Fatalf("bundle = extension %q fingerprint %q", bundle.Extension, bundle.Fingerprint)
			}
		})
	}
}

func TestFetchBootstrapsOnlyCAThenVerifiesTLSScript(t *testing.T) {
	ca, certificate := testTLSMaterial(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/ca.crt":
				w.Header().Set("Content-Type", "application/x-pem-file")
				_, _ = w.Write(ca)
			case strings.HasSuffix(r.URL.Path, "/setup.sh"):
				script, err := onboarding.Shell(onboarding.Config{
					Project: "team-a", Host: "127.0.0.1",
					UnifiedPort: 8443, ProxyPort: 3142, CAPEM: ca,
				})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/x-shellscript")
				_, _ = w.Write(script)
			default:
				http.NotFound(w, r)
			}
		}))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}
	server.StartTLS()
	defer server.Close()

	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	bundle, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(bundle.Script), "#!/usr/bin/env bash") {
		t.Fatal("did not download the setup script over pinned TLS")
	}
}

func TestRunDefaultsToTemporaryShellWithoutDownloadingSetupScript(t *testing.T) {
	ca, certificate := testTLSMaterial(t)
	var setupRequested atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/ca.crt":
				w.Header().Set("Content-Type", "application/x-pem-file")
				_, _ = w.Write(ca)
			case "/api/v1/coordinates":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"proxy":"cache.example:3142"}`)
			case "/readyz":
				_, _ = io.WriteString(w, "ready")
			default:
				if strings.Contains(r.URL.Path, "/setup.") {
					setupRequested.Store(true)
				}
				http.NotFound(w, r)
			}
		}))
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
	}
	server.StartTLS()
	defer server.Close()

	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	var output strings.Builder
	err := Run(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux", Stdout: &output,
		CommandContext: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(
				ctx, os.Args[0], "-test.run=TestTemporarySessionHelper$")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if setupRequested.Load() {
		t.Fatal("temporary mode downloaded the persistent setup script")
	}
	if !strings.Contains(output.String(), "temporary session ended") {
		t.Fatalf("output does not explain restoration:\n%s", output.String())
	}
}

func TestTemporarySessionHelper(t *testing.T) {
	if os.Getenv("PKGREG_SESSION") != "temporary" {
		return
	}
	if os.Getenv("PKGREG_PROJECT") != "team-a" ||
		os.Getenv("PIP_CERT") != "" ||
		!strings.HasPrefix(
			os.Getenv("PIP_INDEX_URL"),
			"http://127.0.0.1:") {
		t.Fatalf("unexpected temporary environment")
	}
	response, err := http.Get(os.Getenv("PKGREG_BRIDGE_URL") + "/readyz") // #nosec G107 -- test-only loopback URL.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "ready" {
		t.Fatalf("bridge response = %d %q", response.StatusCode, body)
	}
}

func TestFetchNeverUsesBootstrapConnectionForSetupScript(t *testing.T) {
	ca := testCA(t, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/ca.crt":
				_, _ = w.Write(ca)
			case strings.HasSuffix(r.URL.Path, "/setup.sh"):
				script, err := onboarding.Shell(onboarding.Config{
					Project: "team-a", Host: "127.0.0.1",
					UnifiedPort: 8443, ProxyPort: 3142, CAPEM: ca,
				})
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/x-shellscript")
				_, _ = w.Write(script)
			default:
				http.NotFound(w, r)
			}
		}))
	defer server.Close()
	fingerprint, _ := onboarding.FingerprintSHA256(ca)

	_, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux",
	})
	if err == nil || !strings.Contains(err.Error(), "download setup script") {
		t.Fatalf("setup script must require TLS signed by the pinned CA, got %v", err)
	}
}

func TestFetchRejectsPlainHTTPWithoutAnExplicitTestClient(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	_, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux",
	})
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("plain HTTP error = %v", err)
	}
}

func TestFetchRejectsFingerprintAndEmbeddedCAMismatch(t *testing.T) {
	ca := testCA(t, 1)
	other := testCA(t, 2)
	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	wrong, _ := onboarding.FingerprintSHA256(other)

	server := testServer(t, ca, ca, false)
	if _, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: wrong,
		OperatingSystem: "linux", Client: server.Client(),
	}); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("wrong fingerprint error = %v", err)
	}
	server.Close()

	server = testServer(t, ca, other, false)
	defer server.Close()
	if _, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux", Client: server.Client(),
	}); err == nil || !strings.Contains(err.Error(), "different CA") {
		t.Fatalf("embedded mismatch error = %v", err)
	}
}

func TestFetchUsesCookieFileWithoutPuttingSecretInArguments(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, true)
	defer server.Close()
	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	cookie := t.TempDir() + "/cookie"
	if err := os.WriteFile(cookie, []byte("pkgreg_session=test-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		CookieFile: cookie, OperatingSystem: "linux", Client: server.Client(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandPreservesDryRunUninstallAndOverrides(t *testing.T) {
	options := Options{
		DryRun: true, Uninstall: true,
		Host: "cache.internal", CacheIP: "10.20.30.40",
	}
	program, args, err := command("linux", "/tmp/setup.sh", options)
	if err != nil {
		t.Fatal(err)
	}
	if program != "/bin/bash" {
		t.Fatalf("program = %q", program)
	}
	for _, value := range []string{
		"--dry-run", "--uninstall", "--host", "cache.internal",
		"--cache-ip", "10.20.30.40",
	} {
		if !slices.Contains(args, value) {
			t.Errorf("shell args do not contain %q: %v", value, args)
		}
	}

	program, args, err = command("windows", `C:\Temp\setup.ps1`, options)
	if err != nil {
		t.Fatal(err)
	}
	if program != "powershell.exe" {
		t.Fatalf("program = %q", program)
	}
	for _, value := range []string{"-DryRun", "-Uninstall", "-HostName", "-CacheIP"} {
		if !slices.Contains(args, value) {
			t.Errorf("PowerShell args do not contain %q: %v", value, args)
		}
	}
}

func TestPrintNeverExecutesScript(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, _ := onboarding.FingerprintSHA256(ca)
	var output strings.Builder
	called := false
	err := Run(context.Background(), Options{
		Server: server.URL, Project: "team-a", ExpectedSHA256: fingerprint,
		OperatingSystem: "linux", Client: server.Client(),
		Persist: true, Print: true, Stdout: &output,
		CommandContext: func(context.Context, string, ...string) *exec.Cmd {
			called = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || !strings.HasPrefix(output.String(), "#!/usr/bin/env bash") {
		t.Fatalf("print called=%v output=%q", called, output.String()[:20])
	}
}

func TestMachineOperationsRequirePersist(t *testing.T) {
	for _, options := range []Options{
		{DryRun: true},
		{Uninstall: true},
		{Print: true},
		{Host: "cache.internal"},
		{CacheIP: "10.20.30.40"},
	} {
		err := Run(context.Background(), options)
		if err == nil || !strings.Contains(err.Error(), "persist") {
			t.Errorf("options %+v error = %v; want a persist requirement", options, err)
		}
	}
}

func TestProjectTokenFileIsSingleLineAndTemporaryOnly(t *testing.T) {
	name := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(name, []byte("id.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := readProjectToken(name)
	if err != nil || token != "id.secret" {
		t.Fatalf("token = %q, error = %v", token, err)
	}
	if err := os.WriteFile(name, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProjectToken(name); err == nil {
		t.Fatal("multiline token was accepted")
	}
	err = Run(context.Background(), Options{Persist: true, TokenFile: name})
	if err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("persistent token-file error = %v", err)
	}
}

func TestFetchAptProxyUsesProjectUsername(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	proxy, err := fetchAptProxy(
		context.Background(), server.Client(), base, "", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if proxy != "http://team-a@pkgcache.internal:3142" {
		t.Fatalf("proxy = %q", proxy)
	}
}

func TestResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, zeroReader{}, maxCABytes+1)
	}))
	defer server.Close()
	_, _, err := download(context.Background(), server.Client(), server.URL, "", maxCABytes)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("limit error = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
