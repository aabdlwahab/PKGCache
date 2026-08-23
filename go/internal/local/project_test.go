package local

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// The current project is a preference, and a preference file must never be able to
// break a build. Every one of these asserts that: a missing file, a corrupt file and an
// empty choice all resolve to the project everything worked in before projects existed.
func TestCurrentProjectDefaultsToGlobal(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	dir := t.TempDir()
	if got := CurrentProject(dir); got != config.GlobalProject {
		t.Fatalf("a fresh cache is in %q, want %q", got, config.GlobalProject)
	}
	if HasCurrentProject(dir) {
		t.Fatal("a fresh cache reports a chosen project")
	}
}

func TestSetCurrentProjectRoundTrip(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	dir := t.TempDir()
	if err := SetCurrentProject(dir, "work"); err != nil {
		t.Fatal(err)
	}
	if got := CurrentProject(dir); got != "work" {
		t.Fatalf("current project is %q, want work", got)
	}
	if !HasCurrentProject(dir) {
		t.Fatal("a chosen project is not reported as chosen")
	}
	ClearCurrentProject(dir)
	if got := CurrentProject(dir); got != config.GlobalProject {
		t.Fatalf("after clearing, current project is %q, want %q", got, config.GlobalProject)
	}
}

// Choosing the global project is choosing the default, and the default is the absence
// of a file. A cache that has been told "global" and one that has never been asked must
// be indistinguishable, or `status` would report a choice nobody made.
func TestChoosingGlobalStoresNothing(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	dir := t.TempDir()
	if err := SetCurrentProject(dir, "work"); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrentProject(dir, config.GlobalProject); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "project.json")); !os.IsNotExist(err) {
		t.Fatalf("choosing global left a file behind: %v", err)
	}
	if HasCurrentProject(dir) {
		t.Fatal("global is reported as a chosen project")
	}
}

// The variable is what a pipeline sets: it must win over whatever a developer selected
// in a shell, and must not change what was selected.
func TestProjectEnvironmentOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ProjectEnvVar, "")
	if err := SetCurrentProject(dir, "work"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProjectEnvVar, "ci")
	if got := CurrentProject(dir); got != "ci" {
		t.Fatalf("with %s=ci the project is %q", ProjectEnvVar, got)
	}
	t.Setenv(ProjectEnvVar, "")
	if got := CurrentProject(dir); got != "work" {
		t.Fatalf("the stored choice was overwritten: %q", got)
	}
}

func TestMalformedProjectFileReadsAsGlobal(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte("{oh dear"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CurrentProject(dir); got != config.GlobalProject {
		t.Fatalf("a corrupt preference file yielded %q, want %q", got, config.GlobalProject)
	}
}

// The cache directory is private, and every file this package writes into it is too.
func TestProjectFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	t.Setenv(ProjectEnvVar, "")
	dir := t.TempDir()
	if err := SetCurrentProject(dir, "work"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("project.json is %o, want 600", mode)
	}
}

func TestSetCurrentProjectRefusesAnEmptyName(t *testing.T) {
	if err := SetCurrentProject(t.TempDir(), "  "); err == nil {
		t.Fatal("an empty project name was accepted")
	}
}
