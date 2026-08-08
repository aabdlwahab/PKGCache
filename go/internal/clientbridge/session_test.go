package clientbridge

import (
	"slices"
	"strings"
	"testing"
)

func TestSessionEnvironmentIsTemporaryAndUsesLoopback(t *testing.T) {
	environment := sessionEnvironment(
		[]string{
			"PATH=/usr/bin",
			"PIP_CERT=/etc/pkgreg/ca.crt",
			"PIP_INDEX_URL=https://cache:8443/global/pypi/root/pypi/+simple/",
			"NPM_CONFIG_CAFILE=/etc/pkgreg/ca.crt",
			"GIT_SSL_CAINFO=/etc/pkgreg/ca.crt",
			"NO_PROXY=example.test",
		},
		"127.0.0.1:43210",
		SessionOptions{
			Server: "https://cache:8443", Project: "team-a",
			CAFingerprint: "AA:BB", AptProxy: "http://team-a@cache:3142",
		},
	)
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"PATH=/usr/bin",
		"PKGREG_SESSION=temporary",
		"PKGREG_BRIDGE_URL=http://127.0.0.1:43210",
		"PKGREG_DOCKER_REGISTRY=127.0.0.1:43210",
		"PKGREG_GIT_URL=http://127.0.0.1:43210/team-a/git",
		"PIP_INDEX_URL=http://127.0.0.1:43210/team-a/pypi/root/pypi/+simple/",
		"NPM_CONFIG_REGISTRY=http://127.0.0.1:43210/team-a/npm/",
		"PKGREG_APT_PROXY=http://team-a@cache:3142",
		"NO_PROXY=example.test,127.0.0.1,localhost",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("temporary environment missing %q:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{
		"PIP_CERT=", "NPM_CONFIG_CAFILE=", "GIT_SSL_CAINFO=",
		"PIP_INDEX_URL=https://cache",
	} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("temporary environment retained %q:\n%s", unwanted, joined)
		}
	}
}

func TestSessionShellUsesInteractivePlatformShell(t *testing.T) {
	program, args, err := sessionShell(SessionOptions{
		OperatingSystem: "linux", Shell: "/bin/bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if program != "/bin/bash" || !slices.Contains(args, "-i") {
		t.Fatalf("Unix shell = %q %v", program, args)
	}

	program, args, err = sessionShell(SessionOptions{OperatingSystem: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if program != "powershell.exe" || !slices.Contains(args, "-NoLogo") {
		t.Fatalf("Windows shell = %q %v", program, args)
	}
}
