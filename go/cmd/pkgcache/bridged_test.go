package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// A -no-cache machine promises no store and no database, and reaches only the team. The
// commands that started a cache for themselves broke both: pull created the databases,
// and the daemon it started had no chain, so the image came from the public registry.
func TestNoCacheMachineRefusesToStartACache(t *testing.T) {
	dataDir := t.TempDir()
	set := local.TeamSet{}
	set.Set(config.GlobalProject, local.Team{
		Server: "https://cache.internal:8443", Project: "team-a", NoCache: true,
		Fingerprint: "AB", CAPEM: "unused",
	})
	if err := local.WriteTeams(dataDir, set); err != nil {
		t.Fatal(err)
	}
	snap := &config.Snapshot{DataDir: dataDir}

	_, err := ensureCaching(context.Background(), snap, "pkgcache pull", "for Docker to pull from")
	if err == nil {
		t.Fatal("a -no-cache machine started a cache")
	}
	for _, want := range []string{"-no-cache", "https://cache.internal:8443", "-team-project team-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	for _, made := range []string{"db", "blobs"} {
		if _, statErr := os.Stat(filepath.Join(dataDir, made)); statErr == nil {
			t.Errorf("refusing still created %s", made)
		}
	}
}
