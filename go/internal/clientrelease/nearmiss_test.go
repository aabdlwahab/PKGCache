package clientrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The signed macOS release shipped pkgreg-client-darwin-arm64.zip, which Parse rejects.
// publish-client skipped it in silence and reported three of five platforms, so macOS
// developers saw no download and nothing said why. The release now ships bare binaries;
// these tests keep the diagnosis working for anyone holding an older archive.

func TestNearMiss(t *testing.T) {
	cases := []struct {
		filename string
		inner    string
		ok       bool
	}{
		{"pkgreg-client-darwin-arm64.zip", "pkgreg-client-darwin-arm64", true},
		{"pkgreg-client-darwin-amd64.zip", "pkgreg-client-darwin-amd64", true},
		{"pkgreg-client-linux-amd64.tar.gz", "pkgreg-client-linux-amd64", true},
		{"pkgreg-client-linux-arm64.tgz", "pkgreg-client-linux-arm64", true},
		{"pkgreg-client-windows-amd64.exe.zip", "pkgreg-client-windows-amd64.exe", true},
		{"pkgreg-bridge-darwin-arm64.zip", "pkgreg-bridge-darwin-arm64", true},
		{"PKGREG-CLIENT-DARWIN-ARM64.ZIP", "", false}, // the grammar is lowercase
		{"pkgreg-client-darwin-arm64", "", false},     // already publishable
		{"pkgreg-client-plan9-amd64.zip", "", false},  // not a platform we build
		{"notes.zip", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		inner, ok := NearMiss(tc.filename)
		if ok != tc.ok || inner != tc.inner {
			t.Errorf("NearMiss(%q) = (%q, %v), want (%q, %v)",
				tc.filename, inner, ok, tc.inner, tc.ok)
		}
	}
}

func TestCollectNearMissesScansDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"pkgreg-client-darwin-arm64.zip",
		"pkgreg-client-darwin-amd64.zip",
		"pkgreg-client-linux-amd64", // publishable, not a near miss
		"README.md",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := CollectNearMisses([]string{dir})
	want := []string{
		"pkgreg-client-darwin-amd64.zip -> pkgreg-client-darwin-amd64",
		"pkgreg-client-darwin-arm64.zip -> pkgreg-client-darwin-arm64",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("CollectNearMisses = %v, want %v", got, want)
	}
}

// TestCollectExplainsAnArchivePassedByName: naming a .zip explicitly used to produce
// "is not a publishable name: expected one of ..." — technically true and useless,
// because the operator is holding the file the release told them to download.
func TestCollectExplainsAnArchivePassedByName(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "pkgreg-client-darwin-arm64.zip")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Collect([]string{archive})
	if err == nil {
		t.Fatal("Collect accepted an archive")
	}
	if !strings.Contains(err.Error(), "extract it to pkgreg-client-darwin-arm64") {
		t.Errorf("error does not say what to do: %v", err)
	}
}

// TestReleaseArtifactShapeIsPublishable is the shape assertion the release workflow
// enforces in CI. Keeping it here too means a change to ClientPlatforms or to the
// filename grammar fails in `go test` rather than at tag time.
func TestReleaseArtifactShapeIsPublishable(t *testing.T) {
	for _, name := range ClientPlatforms() {
		if _, ok := Parse(name); !ok {
			t.Errorf("ClientPlatforms names %q, which Parse rejects", name)
		}
		if _, archived := NearMiss(name); archived {
			t.Errorf("ClientPlatforms names %q, which looks like an archive", name)
		}
	}
}
