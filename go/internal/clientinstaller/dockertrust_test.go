package clientinstaller

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/onboarding"
)

// dockerTrustEnv points the Docker-Desktop branch at a temporary directory, which is what
// DOCKER_CONFIG means to the docker CLI too.
func dockerTrustEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(dockerConfigEnv, root)
	return filepath.Join(root, "certs.d")
}

func TestDockerTrustInstallsOneVerifiedCertificate(t *testing.T) {
	ca := testCA(t, 1)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, err := onboarding.FingerprintSHA256(ca)
	if err != nil {
		t.Fatal(err)
	}
	certs := dockerTrustEnv(t)

	var out bytes.Buffer
	options := Options{
		Server: server.URL, Project: "global", ExpectedSHA256: fingerprint,
		DockerTrust: true, OperatingSystem: "darwin",
		Client: server.Client(), Stdout: &out,
	}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("docker trust: %v", err)
	}

	authority := strings.TrimPrefix(server.URL, "http://")
	target := filepath.Join(certs, authority, "ca.crt")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed CA: %v", err)
	}
	if !bytes.Equal(body, ca) {
		t.Error("installed certificate is not the one the server served")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon may run as another user than the developer who installed it.
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
	// The output has to name the address to pull from; a certificate with no usable
	// command leaves the reader exactly where they started.
	if !strings.Contains(out.String(), "docker pull "+authority+"/dockerhub/") {
		t.Errorf("output does not give a pull command:\n%s", out.String())
	}
	if !strings.Contains(out.String(), fingerprint) {
		t.Errorf("output does not report the pinned fingerprint:\n%s", out.String())
	}
}

// The reason this is a client mode rather than three lines of documentation: a
// copy-pasted curl trusts whatever answers, and this must not.
func TestDockerTrustRefusesAMismatchedCABeforeWritingAnything(t *testing.T) {
	served, other := testCA(t, 2), testCA(t, 3)
	server := testServer(t, served, served, false)
	defer server.Close()
	wrong, err := onboarding.FingerprintSHA256(other)
	if err != nil {
		t.Fatal(err)
	}
	certs := dockerTrustEnv(t)

	err = Run(context.Background(), Options{
		Server: server.URL, Project: "global", ExpectedSHA256: wrong,
		DockerTrust: true, OperatingSystem: "darwin",
		Client: server.Client(), Stdout: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("a mismatched fingerprint was accepted")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
	if entries, statErr := os.ReadDir(certs); statErr == nil && len(entries) > 0 {
		t.Errorf("a refused install still wrote %d entries", len(entries))
	}
}

func TestDockerTrustDryRunChangesNothing(t *testing.T) {
	ca := testCA(t, 4)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, err := onboarding.FingerprintSHA256(ca)
	if err != nil {
		t.Fatal(err)
	}
	certs := dockerTrustEnv(t)

	var out bytes.Buffer
	if err := Run(context.Background(), Options{
		Server: server.URL, Project: "global", ExpectedSHA256: fingerprint,
		DockerTrust: true, DryRun: true, OperatingSystem: "darwin",
		Client: server.Client(), Stdout: &out,
	}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, statErr := os.Stat(certs); statErr == nil {
		t.Error("dry run created the certificate directory")
	}
	if !strings.Contains(out.String(), "+ write ") ||
		!strings.Contains(out.String(), "Nothing was changed") {
		t.Errorf("dry run did not describe the change:\n%s", out.String())
	}
}

// Removal must be surgical: every sibling directory under certs.d belongs to a different
// registry, and some of them will not be pkgreg's.
func TestDockerTrustUninstallLeavesOtherRegistriesAlone(t *testing.T) {
	ca := testCA(t, 5)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, err := onboarding.FingerprintSHA256(ca)
	if err != nil {
		t.Fatal(err)
	}
	certs := dockerTrustEnv(t)
	neighbour := filepath.Join(certs, "registry.example:5000")
	if err := os.MkdirAll(neighbour, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neighbour, "ca.crt"), []byte("someone else"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := Options{
		Server: server.URL, Project: "global", ExpectedSHA256: fingerprint,
		DockerTrust: true, OperatingSystem: "darwin", Client: server.Client(),
	}
	install := base
	install.Stdout = &bytes.Buffer{}
	if err := Run(context.Background(), install); err != nil {
		t.Fatalf("install: %v", err)
	}

	remove := base
	remove.Uninstall = true
	var out bytes.Buffer
	remove.Stdout = &out
	if err := Run(context.Background(), remove); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	authority := strings.TrimPrefix(server.URL, "http://")
	if _, statErr := os.Stat(filepath.Join(certs, authority)); statErr == nil {
		t.Error("uninstall left its own directory behind")
	}
	if _, statErr := os.Stat(filepath.Join(neighbour, "ca.crt")); statErr != nil {
		t.Errorf("uninstall removed another registry's certificate: %v", statErr)
	}

	// Running it twice is how anyone cleans up after a failure, so it reports rather
	// than errors.
	if err := Run(context.Background(), remove); err != nil {
		t.Errorf("second uninstall failed: %v", err)
	}
	if !strings.Contains(out.String(), "removed ") {
		t.Errorf("uninstall said nothing useful:\n%s", out.String())
	}
}

// The paths differ per platform and the difference is load-bearing: setup.sh wrote the
// Linux path on macOS, where Docker Desktop does not read it, so the certificate was
// installed and the pull still failed. Docker Desktop 29.6.2 on darwin/arm64 was verified
// to honour ~/.docker/certs.d.
func TestDockerCertificateDirectoryFollowsThePlatform(t *testing.T) {
	t.Run("darwin honours DOCKER_CONFIG", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(dockerConfigEnv, root)
		directory, system, err := dockerCertsDir("darwin")
		if err != nil {
			t.Fatalf("darwin: %v", err)
		}
		if directory != filepath.Join(root, "certs.d") || system {
			t.Errorf("darwin = %q system=%v", directory, system)
		}
	})

	// A per-registry directory would have to be named "<host>:<port>", and a colon is
	// not legal in a Windows path — so there is nothing to write, and saying so beats
	// creating something the daemon will never read. --persist uses the certificate
	// store there instead.
	t.Run("windows refuses instead of writing an impossible path", func(t *testing.T) {
		t.Setenv(dockerConfigEnv, "")
		_, _, err := dockerCertsDir("windows")
		if err == nil {
			t.Fatal("windows was given a certs.d directory")
		}
		if !strings.Contains(err.Error(), "--persist") {
			t.Errorf("error does not point at the path that works: %v", err)
		}
	})

	t.Run("linux ignores DOCKER_CONFIG", func(t *testing.T) {
		// dockerd reads only /etc/docker/certs.d. Writing under a client-side override
		// would report success and be ignored by the daemon.
		t.Setenv(dockerConfigEnv, t.TempDir())
		directory, root, err := dockerCertsDir("linux")
		if err != nil {
			t.Fatal(err)
		}
		if directory != "/etc/docker/certs.d" || !root {
			t.Errorf("linux = %q root=%v, want the system path", directory, root)
		}
	})

	if _, _, err := dockerCertsDir("plan9"); err == nil {
		t.Error("an unknown platform silently got a certificate directory")
	}
}

func TestDockerTrustRejectsIncompatibleFlags(t *testing.T) {
	ca := testCA(t, 6)
	server := testServer(t, ca, ca, false)
	defer server.Close()
	fingerprint, err := onboarding.FingerprintSHA256(ca)
	if err != nil {
		t.Fatal(err)
	}
	dockerTrustEnv(t)

	base := Options{
		Server: server.URL, Project: "global", ExpectedSHA256: fingerprint,
		DockerTrust: true, OperatingSystem: "darwin", Client: server.Client(),
		Stdout: &bytes.Buffer{},
	}
	for name, mutate := range map[string]func(*Options){
		// Both write a Docker certificate; running both would install it twice by two
		// different mechanisms and leave uninstall ambiguous.
		"persist":    func(o *Options) { o.Persist = true },
		"print":      func(o *Options) { o.Print = true },
		"host":       func(o *Options) { o.Host = "cache.example" },
		"cache-ip":   func(o *Options) { o.CacheIP = "10.0.0.1" },
		"token-file": func(o *Options) { o.TokenFile = "/tmp/token" },
	} {
		options := base
		mutate(&options)
		if err := Run(context.Background(), options); err == nil {
			t.Errorf("-docker-trust accepted -%s", name)
		}
	}
}
