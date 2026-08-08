package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdInstallToStagingDirectories(t *testing.T) {
	base := t.TempDir()
	unitDir := filepath.Join(base, "units")
	binDir := filepath.Join(base, "bin")
	dataDir := "/var/lib/pkgreg-test"
	err := runSystemd(context.Background(), []string{
		"install",
		"-unit-dir", unitDir,
		"-bin-dir", binDir,
		"-data-dir", dataDir,
		"-hostnames", "cache.example,10.0.0.5",
		"-start=false",
	})
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Stat(filepath.Join(binDir, "pkgreg"))
	if err != nil {
		t.Fatal(err)
	}
	if binary.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %o", binary.Mode().Perm())
	}
	body, err := os.ReadFile(filepath.Join(unitDir, unitName))
	if err != nil {
		t.Fatal(err)
	}
	unit := string(body)
	for _, required := range []string{
		"DynamicUser=yes",
		"StateDirectory=pkgreg-test",
		"ExecStartPre=",
		"cache.example,10.0.0.5",
		"ExecStart=",
		"ProtectSystem=strict",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("unit lacks %q:\n%s", required, unit)
		}
	}
}
