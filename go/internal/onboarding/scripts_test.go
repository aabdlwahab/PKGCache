package onboarding

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os/exec"
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
	} {
		if !strings.Contains(text, want) {
			t.Errorf("shell script does not contain %q", want)
		}
	}
	for _, forbidden := range []string{"Authorization: Bearer", "ca.key", "server.key"} {
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
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dry run: %v\n%s", err, output)
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
		"PKGREG_DOCKER_REGISTRY",
		"PKGREG_GIT_URL",
		"CA SHA-256",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("PowerShell script does not contain %q", want)
		}
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
