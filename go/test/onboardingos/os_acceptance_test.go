package onboardingos

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/onboarding"
)

const (
	acceptanceEnv = "PKGREG_ONBOARDING_OS_ACCEPTANCE"
	project       = "pkgreg-os-acceptance"
	cacheHost     = "pkgreg-os-acceptance.internal"
)

func TestPrivilegedInstallIdempotencyTrustAndUninstall(t *testing.T) {
	if os.Getenv(acceptanceEnv) != "1" {
		t.Skip("privileged OS acceptance is opt-in with " + acceptanceEnv + "=1")
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		t.Fatal("OS acceptance must run explicitly as root")
	}

	caPEM, serverCertificate := certificates(t)
	server := tlsFixture(t, serverCertificate)
	defer server.Close()
	_, rawPort, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}

	config := onboarding.Config{
		Project: project, Host: cacheHost, UnifiedPort: port,
		ProxyPort: port, CAPEM: caPEM,
	}
	script, extension, err := platformScript(config)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(t.TempDir(), "setup."+extension)
	if err := os.WriteFile(scriptPath, script, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("pre-existing acceptance state at %s", stateFile())
	}
	runScript(t, scriptPath, true, false)
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatal("dry-run changed the machine")
	}
	if output, err := curl(serverURL(port)); err == nil {
		t.Fatalf("private endpoint unexpectedly trusted before install: %s", output)
	}

	installed := false
	defer func() {
		if installed {
			runScript(t, scriptPath, false, true)
		}
	}()
	runScript(t, scriptPath, false, false)
	installed = true
	runScript(t, scriptPath, false, false) // idempotency

	assertInstalled(t, port)
	output, err := curl(serverURL(port))
	if err != nil {
		t.Fatalf("trusted request failed after install: %v\n%s", err, output)
	}
	if strings.TrimSpace(output) != "pkgreg onboarding acceptance" {
		t.Fatalf("trusted response = %q", output)
	}

	runScript(t, scriptPath, false, true)
	installed = false
	assertUninstalled(t, port)
	if output, err := curl(serverURL(port)); err == nil {
		t.Fatalf("private endpoint still trusted after uninstall: %s", output)
	}
}

func platformScript(config onboarding.Config) ([]byte, string, error) {
	if runtime.GOOS == "windows" {
		script, err := onboarding.PowerShell(config)
		return script, "ps1", err
	}
	script, err := onboarding.Shell(config)
	return script, "sh", err
}

func runScript(t *testing.T, script string, dryRun, uninstall bool) {
	t.Helper()
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		args := []string{
			"-NoLogo", "-NoProfile", "-NonInteractive",
			"-ExecutionPolicy", "Bypass", "-File", script,
			"-CacheIP", "127.0.0.1",
		}
		if dryRun {
			args = append(args, "-DryRun")
		}
		if uninstall {
			args = append(args, "-Uninstall")
		}
		command = exec.Command("powershell.exe", args...)
	} else {
		args := []string{script, "--cache-ip", "127.0.0.1"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		if uninstall {
			args = append(args, "--uninstall")
		}
		command = exec.Command("/bin/bash", args...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("setup script failed: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

func stateFile() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "pkgreg", "projects", project, "state.json")
	}
	return filepath.Join("/etc/pkgreg/projects", project, "state")
}

func dockerCA(port int) string {
	authority := net.JoinHostPort(cacheHost, strconv.Itoa(port))
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "docker", "certs.d", authority, "ca.crt")
	case "darwin":
		// Docker Desktop reads certs.d from the invoking user's home, not root's and not
		// /etc — which is what the installer writes, and why it resolves the home from
		// SUDO_USER (see docker_ca_path in internal/onboarding). This test runs under
		// sudo too, so it has to resolve it the same way or it asserts a path that is
		// never written on a Mac.
		return filepath.Join(desktopHome(), ".docker", "certs.d", authority, "ca.crt")
	default:
		return filepath.Join("/etc/docker/certs.d", authority, "ca.crt")
	}
}

// desktopHome mirrors the installer's desktop_home: the home of the user who invoked
// sudo, falling back to this process's own when it was not invoked through sudo.
func desktopHome() string {
	if user := os.Getenv("SUDO_USER"); user != "" {
		if resolved, err := exec.Command("sh", "-c", "eval printf '%s' ~"+user).Output(); err == nil {
			if home := strings.TrimSpace(string(resolved)); home != "" {
				if info, statErr := os.Stat(home); statErr == nil && info.IsDir() {
					return home
				}
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func hostsFile() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

func assertInstalled(t *testing.T, port int) {
	t.Helper()
	files := []string{stateFile()}
	if runtime.GOOS != "windows" {
		files = append(files, dockerCA(port))
	}
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("installed file %s: %v", file, err)
		}
	}
	hosts, err := os.ReadFile(hostsFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), cacheHost+" # pkgreg:"+project) {
		t.Errorf("hosts entry not installed:\n%s", hosts)
	}
	if runtime.GOOS == "windows" {
		output, err := exec.Command(
			"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			`[Environment]::GetEnvironmentVariable("PKGREG_PROJECT","Machine")`,
		).CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != project {
			t.Errorf("machine environment = %q, %v", output, err)
		}
	} else {
		envFile := filepath.Join("/etc/pkgreg/projects", project, "env.sh")
		body, err := os.ReadFile(envFile)
		if err != nil || !strings.Contains(string(body), "export PIP_CERT=") ||
			!strings.Contains(string(body), "export NPM_CONFIG_CAFILE=") {
			t.Errorf("tool environment %s is incomplete: %v", envFile, err)
		}
	}
}

func assertUninstalled(t *testing.T, port int) {
	t.Helper()
	files := []string{stateFile()}
	if runtime.GOOS != "windows" {
		files = append(files, dockerCA(port))
	}
	for _, file := range files {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("file was not removed: %s (%v)", file, err)
		}
	}
	hosts, err := os.ReadFile(hostsFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hosts), "# pkgreg:"+project) {
		t.Errorf("hosts entry was not removed:\n%s", hosts)
	}
}

func curl(target string) (string, error) {
	program := "curl"
	args := []string{"--silent", "--show-error", "--fail", "--noproxy", "*"}
	if runtime.GOOS == "windows" {
		program = "curl.exe"
		// Windows curl verifies through schannel, which insists on a revocation check
		// and treats "could not check" as fatal: CRYPT_E_NO_REVOCATION_CHECK. pkgreg's
		// CA is private and publishes no CRL or OCSP endpoint, so there is nothing for
		// schannel to reach and never will be.
		//
		// This is a property of curl on Windows, not of the trust the script installs.
		// The clients it actually configures — pip, npm, git, uv — are pointed at the CA
		// file through PIP_CERT, NPM_CONFIG_CAFILE, GIT_SSL_CAINFO and NODE_EXTRA_CA_CERTS
		// and use their own TLS stacks, none of which requires a revocation endpoint. So
		// the flag belongs to the probe rather than to the product.
		args = append(args, "--ssl-revoke-best-effort")
	}
	command := exec.Command(program, append(args, target)...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func serverURL(port int) string {
	return fmt.Sprintf("https://%s:%d/", cacheHost, port)
}

func tlsFixture(t *testing.T, certificate tls.Certificate) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "pkgreg onboarding acceptance")
		}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{ // #nosec G402 -- the fixture requires modern verified TLS.
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
	}
	server.StartTLS()
	return server
}

func certificates(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pkgreg OS acceptance CA"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(
		rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cacheHost},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		DNSNames:    []string{cacheHost},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(
		rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	serverPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey),
	})
	certificate, err := tls.X509KeyPair(serverPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return caPEM, certificate
}
