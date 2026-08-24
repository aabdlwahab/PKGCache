package feed

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controlFor(name, version, arch string, extra string) string {
	return "Package: " + name + "\nVersion: " + version + "\nArchitecture: " + arch +
		"\nMaintainer: pkgreg <root@localhost>\nDescription: test package\n" + extra
}

// writeRepo publishes two architectures of two packages, which is the shape the Debian
// split in the plan produces: pkgcache and pkgcache-desktop, amd64 and arm64.
func writeRepo(t *testing.T, root string, when time.Time) (RepoResult, *PGPKey) {
	t.Helper()
	source := t.TempDir()
	var debs []string
	for _, spec := range []struct{ name, arch, extra string }{
		{"pkgcache", "amd64", ""},
		{"pkgcache", "arm64", ""},
		{"pkgcache-desktop", "amd64", "Source: pkgcache\nDepends: pkgcache (= 1.2.0)\n"},
		{"pkgcache-desktop", "arm64", "Source: pkgcache\nDepends: pkgcache (= 1.2.0)\n"},
	} {
		name := filepath.Join(source, spec.name+"_1.2.0_"+spec.arch+".deb")
		body := buildDeb(t, controlFor(spec.name, "1.2.0", spec.arch, spec.extra))
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
		debs = append(debs, name)
	}

	key := testSigningKey(t)
	result, err := WriteRepository(RepoOptions{
		Root: root, Debs: debs, Key: key,
		Origin: "pkgreg", Label: "pkgcache", Date: when,
	})
	if err != nil {
		t.Fatalf("WriteRepository: %v", err)
	}
	return result, key
}

func TestWriteRepositoryProducesTheWholeLayout(t *testing.T) {
	root := t.TempDir()
	result, _ := writeRepo(t, root, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))

	for _, want := range []string{
		"dists/stable/Release",
		"dists/stable/InRelease",
		"dists/stable/Release.gpg",
		"dists/stable/main/binary-amd64/Packages",
		"dists/stable/main/binary-amd64/Packages.gz",
		"dists/stable/main/binary-arm64/Packages",
		"dists/stable/main/binary-arm64/Packages.gz",
		"pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb",
		"pool/main/p/pkgcache/pkgcache-desktop_1.2.0_amd64.deb",
		"pkgcache-archive-keyring.asc",
	} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	if got := result.Architectures; len(got) != 2 || got[0] != "amd64" || got[1] != "arm64" {
		t.Errorf("architectures = %v, want [amd64 arm64]", got)
	}
	// Both binary packages share one pool directory, because they are built from one
	// source. That is what the Source field is for.
	if len(result.Packages) != 4 {
		t.Errorf("want 4 published packages, got %d", len(result.Packages))
	}
}

func TestPublishedIndexNamesEveryPackageForItsArchitecture(t *testing.T) {
	root := t.TempDir()
	writeRepo(t, root, time.Time{})

	body, err := os.ReadFile(filepath.Join(root, "dists/stable/main/binary-amd64/Packages"))
	if err != nil {
		t.Fatal(err)
	}
	index := string(body)
	for _, want := range []string{"Package: pkgcache", "Package: pkgcache-desktop"} {
		if !strings.Contains(index, want) {
			t.Errorf("the amd64 index is missing %q\n%s", want, index)
		}
	}
	// An architecture's index must not carry another's, or apt offers a package it
	// cannot install.
	if strings.Contains(index, "Architecture: arm64") {
		t.Errorf("the amd64 index contains arm64 packages\n%s", index)
	}
	// The version lock from the plan has to survive into the index, since that is the
	// only place apt reads it.
	if !strings.Contains(index, "Depends: pkgcache (= 1.2.0)") {
		t.Errorf("the desktop package's version lock is missing\n%s", index)
	}
}

func TestReleaseIsSignedOverTheRealIndexes(t *testing.T) {
	root := t.TempDir()
	_, key := writeRepo(t, root, time.Time{})

	public, err := key.ArmoredPublic()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := os.ReadFile(filepath.Join(root, "dists/stable/InRelease"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := VerifyClearSigned(public, signed)
	if err != nil {
		t.Fatalf("InRelease does not verify: %v", err)
	}

	// The chain that makes the whole repository trusted: the signature covers Release,
	// Release names each index with its hash, each index names each package with its hash.
	// If the hash in Release is not the hash of the file on disk, apt reports a mismatch
	// and the repository is unusable.
	plain, err := os.ReadFile(filepath.Join(root, "dists/stable/main/binary-amd64/Packages"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex(plain)
	if !strings.Contains(string(payload), digest) {
		t.Errorf("Release does not carry the amd64 index's real SHA256 (%s)\n%s",
			digest, payload)
	}

	// And the unsigned Release on disk must be the bytes that were signed.
	onDisk, err := os.ReadFile(filepath.Join(root, "dists/stable/Release"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(onDisk)) != strings.TrimSpace(string(payload)) {
		t.Error("Release on disk differs from what InRelease vouches for")
	}
}

func TestWriteRepositoryIsReproducible(t *testing.T) {
	// "Has the repository changed?" should have an answer. Two publishes of one input at
	// one timestamp must produce identical indexes.
	when := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	first, second := t.TempDir(), t.TempDir()
	writeRepo(t, first, when)
	writeRepo(t, second, when)

	for _, name := range []string{
		"dists/stable/Release",
		"dists/stable/main/binary-amd64/Packages",
		"dists/stable/main/binary-amd64/Packages.gz",
	} {
		a, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Errorf("%s differs between two publishes of the same input", name)
		}
	}
}

func TestWriteRepositoryRefusesToPublishUnsigned(t *testing.T) {
	_, err := WriteRepository(RepoOptions{Root: t.TempDir()})
	if err == nil {
		t.Fatal("an unsigned repository must be refused")
	}
	if !strings.Contains(err.Error(), "signed") {
		t.Errorf("the refusal should explain itself: %v", err)
	}
}

func TestWriteRepositoryStopsBeforeWritingAnythingOnABadPackage(t *testing.T) {
	// apt caches indexes, so a client that fetched a half-written repository keeps it
	// until something changes again. One unreadable .deb has to stop the whole publish.
	root := t.TempDir()
	source := t.TempDir()
	good := filepath.Join(source, "pkgcache_1.2.0_amd64.deb")
	if err := os.WriteFile(good, buildDeb(t, controlFor("pkgcache", "1.2.0", "amd64", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(source, "broken_1.0.0_amd64.deb")
	if err := os.WriteFile(bad, []byte("not a deb"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteRepository(RepoOptions{
		Root: root, Debs: []string{good, bad}, Key: testSigningKey(t),
	}); err == nil {
		t.Fatal("a broken package must stop the publish")
	}
	if _, err := os.Stat(filepath.Join(root, "dists")); err == nil {
		t.Error("nothing should have been written")
	}
}

func TestGPGVerifiesTheWholePublishedRepository(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg is not installed; skipping the external check")
	}
	root := t.TempDir()
	writeRepo(t, root, time.Time{})

	home := t.TempDir()
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gpg, append([]string{"--homedir", home, "--batch"}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("--import", filepath.Join(root, "pkgcache-archive-keyring.asc")); err != nil {
		t.Fatalf("the keyring this publishes cannot be imported: %v\n%s", err, out)
	}
	// Both signature forms, because a client may look for either.
	for _, check := range [][]string{
		{"--verify", filepath.Join(root, "dists/stable/InRelease")},
		{"--verify", filepath.Join(root, "dists/stable/Release.gpg"),
			filepath.Join(root, "dists/stable/Release")},
	} {
		out, err := run(check...)
		if err != nil || !strings.Contains(out, "Good signature") {
			t.Errorf("gpg rejected %v: %v\n%s", check, err, out)
		}
	}
}

func TestSourcesLineNamesItsOwnKeyring(t *testing.T) {
	got := SourcesLine("https://cache.internal:8443/apt", "", "")
	for _, want := range []string{
		"Types: deb",
		"URIs: https://cache.internal:8443/apt",
		"Suites: stable",
		"Components: main",
		"Signed-By: /usr/share/keyrings/pkgcache-archive-keyring.asc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the sources file is missing %q\n%s", want, got)
		}
	}
	// Trusting one named key rather than every key on the system is the whole reason for
	// preferring deb822 here.
	if strings.Contains(got, "trusted=yes") {
		t.Error("a repository should never be published with trust turned off")
	}
}
