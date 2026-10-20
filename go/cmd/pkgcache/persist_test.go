package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/local"
)

// -dry-run promises to change nothing, and once installed the activation service anyway:
// a launchd agent or a systemd unit registered in the session of whoever asked for a
// preview. The settings files were previewed correctly; the one change that reaches
// outside the home directory was made for real.
func TestPersistDryRunInstallsNothing(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no socket activation to install on " + runtime.GOOS)
	}
	home, dataDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGCACHE_ADDR", "127.0.0.1:1")
	t.Setenv("PKGCACHE_LIMIT", "none")

	previous := installService
	t.Cleanup(func() { installService = previous })
	installService = func(string, string, bool, io.Writer) (local.Availability, error) {
		t.Fatal("a dry run installed the activation service")
		return local.AvailabilityUnknown, nil
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	stdout := os.Stdout
	os.Stdout = devNull
	t.Cleanup(func() { os.Stdout = stdout })

	if err := runPersist(context.Background(), []string{"-dry-run", "-data-dir", dataDir}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	written, err := filepath.Glob(filepath.Join(home, "*"))
	if err != nil {
		t.Fatal(err)
	}
	hidden, _ := filepath.Glob(filepath.Join(home, ".*"))
	if all := append(written, hidden...); len(all) > 0 {
		t.Errorf("a dry run wrote into the home directory: %v", all)
	}
}
