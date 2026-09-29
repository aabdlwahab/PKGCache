package onboarding

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCertificate(t *testing.T, isCA bool) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "pkgreg test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Project: "team-a", Host: "pkgcache.internal", UnifiedPort: 8443,
		ProxyPort: 3142, CAPEM: testCertificate(t, true),
	}
}

func TestShellIsProjectSpecificAuditableAndSyntacticallyValid(t *testing.T) {
	script, err := Shell(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"--dry-run",
		"--uninstall",
		"--cache-ip",
		"https://pkgcache.internal:8443/team-a/pypi/root/pypi/+simple/",
		"https://pkgcache.internal:8443/team-a/npm/",
		"http://team-a@pkgcache.internal:3142",
		"PKGREG_DOCKER_REGISTRY",
		"PKGREG_GIT_URL",
		"this script never invokes sudo",
		"/etc/ca-certificates/trust-source/anchors",
		"/etc/pki/ca-trust/source/anchors",
		"CA_SHA256=",
		"BEGIN CERTIFICATE",
		"export PNPM_CONFIG_REGISTRY='$npm_url'",
		`corepack_url="https://${URL_HOST}:${UNIFIED_PORT}/${PROJECT}/npm"`,
		"export UV_SYSTEM_CERTS='true'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("shell script does not contain %q", want)
		}
	}
	// The script is one format string: a shell "%" written without doubling is mangled.
	// UV_INDEX_URL reaches uv's project commands too; see the environment-file test.
	for _, forbidden := range []string{
		"Authorization: Bearer", "ca.key", "server.key", "%!",
		"export UV_INDEX_URL", "export UV_DEFAULT_INDEX",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("shell script unexpectedly contains %q", forbidden)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	command := exec.Command(bash, "-n")
	command.Stdin = strings.NewReader(text)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
	command = exec.Command(bash, "-s", "--", "--dry-run", "--cache-ip", "10.20.30.40")
	command.Stdin = strings.NewReader(text)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, output)
	}
	if want := "+ write /etc/pkgreg/projects/team-a/xdg/uv/uv.toml (mode 0644)"; !strings.Contains(string(output), want) {
		t.Errorf("dry run does not report %q:\n%s", want, output)
	}
	command = exec.Command(bash, "-s", "--", "--dry-run", "--uninstall")
	command.Stdin = strings.NewReader(text)
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run uninstall: %v\n%s", err, output)
	}
	if want := "/etc/pkgreg/projects/team-a/xdg/uv/uv.toml"; !strings.Contains(string(output), want) {
		t.Errorf("uninstall does not remove %s:\n%s", want, output)
	}
}

// installedFiles runs the script's own install_client, as a dry run with write_file
// capturing what it would write, and returns those files by path. The files are rendered
// only by a real install, which needs root; this is the same code short of the write.
func installedFiles(t *testing.T, bash string, script []byte) map[string]string {
	t.Helper()
	const run = "if [[ \"$MODE\" == uninstall ]]; then uninstall_client; else install_client; fi\n"
	text := string(script)
	if !strings.HasSuffix(text, run) {
		t.Fatalf("the script no longer ends by running install_client:\n%s", text[len(text)-200:])
	}
	out := t.TempDir()
	harness := strings.TrimSuffix(text, run) + `
write_file() { mkdir -p "$OUT$(dirname "$1")"; printf '%s' "$3" >"$OUT$1"; }
install_client >/dev/null
`
	command := exec.Command(bash, "-s", "--", "--dry-run")
	command.Stdin = strings.NewReader(harness)
	command.Env = append(os.Environ(), "OUT="+out)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install_client: %v\n%s", err, output)
	}
	files := map[string]string{}
	err := filepath.WalkDir(out, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary directory.
		files[strings.TrimPrefix(path, out)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// UV_INDEX_URL in the environment file reached uv's project commands as well as uv pip:
// uv lock and uv sync rewrote a project's uv.lock to name the cache, and uv sync --locked
// refused a lock made against PyPI, on every machine this script had set up. uv now finds
// the index in a [pip] table, which uv pip alone reads, in a directory only the shells
// that load the environment file put in XDG_CONFIG_DIRS.
func TestEnvironmentFilePointsOnlyUVPipAtTheCache(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for _, machine := range []string{"/etc/uv/uv.toml", "/etc/xdg/uv/uv.toml"} {
		if _, err := os.Stat(machine); err == nil {
			t.Skipf("%s exists here, and the environment file rightly leaves it in charge", machine)
		}
	}
	script, err := Shell(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	files := installedFiles(t, bash, script)
	const dir = "/etc/pkgreg/projects/team-a/xdg"
	env, uv := files["/etc/pkgreg/projects/team-a/env.sh"], files[dir+"/uv/uv.toml"]
	if strings.Contains(env, "export UV_INDEX_URL") || strings.Contains(env, "export UV_DEFAULT_INDEX") {
		t.Errorf("the environment file still sets uv's index:\n%s", env)
	}
	want := "[pip]\nindex-url = \"https://pkgcache.internal:8443/team-a/pypi/root/pypi/+simple/\""
	if !strings.Contains(uv, want) || strings.Count(uv, "[") != 1 {
		t.Errorf("uv.toml is not a lone [pip] table with the cache's index:\n%s", uv)
	}
	envFile := filepath.Join(t.TempDir(), "env.sh")
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	shells := []string{"sh", "bash", "zsh"}
	for _, test := range []struct {
		name, inherited, want string
		twice                 bool
	}{
		// Unset means /etc/xdg to everything else that reads the variable, so it stays.
		{name: "unset", want: dir + ":/etc/xdg"},
		{name: "a desktop session's", inherited: "/etc/xdg/xdg-ubuntu:/etc/xdg",
			want: dir + ":/etc/xdg/xdg-ubuntu:/etc/xdg"},
		// A nested login shell loads the file again.
		{name: "loaded twice", want: dir + ":/etc/xdg", twice: true},
	} {
		for _, shell := range shells {
			path, err := exec.LookPath(shell)
			if err != nil {
				continue
			}
			source := `. "$1"; `
			if test.twice {
				source += `. "$1"; `
			}
			command := exec.Command(path, "-c", source+`printf '%s' "$XDG_CONFIG_DIRS"`, shell, envFile)
			command.Env = []string{"PATH=" + os.Getenv("PATH")}
			if test.inherited != "" {
				command.Env = append(command.Env, "XDG_CONFIG_DIRS="+test.inherited)
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s, %s: %v\n%s", shell, test.name, err, output)
			}
			if string(output) != test.want {
				t.Errorf("%s, %s: XDG_CONFIG_DIRS = %q, want %q", shell, test.name, output, test.want)
			}
		}
	}
}

func TestPowerShellPreservesEnvironmentForUninstall(t *testing.T) {
	script, err := PowerShell(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{
		"[switch]$DryRun",
		"[switch]$Uninstall",
		"PreviousEnvironment",
		// The store, not the means of reaching it. This used to assert the Cert: drive,
		// which named an implementation that turned out not to exist on every runner —
		// the assertion held while the script failed.
		`X509Store]::new("Root", "LocalMachine")`,
		"PIP_INDEX_URL",
		"NPM_CONFIG_REGISTRY",
		"PNPM_CONFIG_REGISTRY",
		"COREPACK_NPM_REGISTRY",
		"PKGREG_DOCKER_REGISTRY",
		"PKGREG_GIT_URL",
		"CA SHA-256",
		`UV_NATIVE_TLS="true"; UV_SYSTEM_CERTS="true"`,
		`$UVConfig = Join-Path $env:ProgramData "uv\uv.toml"`,
		"[pip]\r\nindex-url = \"$pypi\"",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("PowerShell script does not contain %q", want)
		}
	}
	// Named so that install clears one an earlier version set, and never given a value.
	if strings.Contains(text, "UV_INDEX_URL=") || strings.Contains(text, "UV_DEFAULT_INDEX=") {
		t.Error("PowerShell script still sets uv's index in the machine environment")
	}
	if strings.Contains(text, "\n") && !strings.Contains(text, "\r\n") {
		t.Error("PowerShell download is not CRLF encoded")
	}
	if strings.Contains(text, `docker\certs.d`) {
		t.Error("PowerShell should trust Docker through LocalMachine Root, not an invalid host:port path")
	}
}

func TestFingerprintSHA256UsesOutOfBandDisplayFormat(t *testing.T) {
	fingerprint, err := FingerprintSHA256(testCertificate(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprint) != 95 || strings.Count(fingerprint, ":") != 31 {
		t.Fatalf("fingerprint = %q", fingerprint)
	}
}

func TestGenerationRejectsUnsafeInputsAndNonCA(t *testing.T) {
	base := testConfig(t)
	tests := []Config{
		func() Config { c := base; c.Project = "bad/project"; return c }(),
		func() Config { c := base; c.Host = "host;touch-pwned"; return c }(),
		func() Config { c := base; c.UnifiedPort = 0; return c }(),
		func() Config { c := base; c.CAPEM = []byte("not a certificate"); return c }(),
		func() Config { c := base; c.CAPEM = testCertificate(t, false); return c }(),
	}
	for i, cfg := range tests {
		if _, err := Shell(cfg); err == nil {
			t.Errorf("case %d: Shell accepted unsafe input", i)
		}
		if _, err := PowerShell(cfg); err == nil {
			t.Errorf("case %d: PowerShell accepted unsafe input", i)
		}
	}
}
