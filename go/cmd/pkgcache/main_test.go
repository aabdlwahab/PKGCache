package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// TestMain refuses to be a daemon. local.Ensure starts one by re-executing os.Executable,
// which under go test is this binary: a regression that let a command start the cache
// would run every test again in the child, and the child would start another.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		fmt.Fprintln(os.Stderr, "pkgcache tests: a command started the cache daemon")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// A command in the table and missing from order works but is never mentioned in the
// help, which is how peer went unlisted.
func TestOrderListsEveryCommand(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range order {
		if listed[name] {
			t.Errorf("%s is listed twice in the help", name)
		}
		listed[name] = true
		if _, ok := commands[name]; !ok {
			t.Errorf("the help lists %s, which is not a command", name)
		}
	}
	for name := range commands {
		if !listed[name] {
			t.Errorf("%s is a command the help never mentions; add it to order", name)
		}
	}
}

// The usage text promises `pkgcache <command> -h` for a command's flags. Asking a question
// must not be what starts a daemon, creates a cache or runs docker — build and compose
// once did all three, because -h went to docker after the cache had been started for it.
func TestEveryCommandAnswersHelpWithoutSideEffects(t *testing.T) {
	dataDir, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PKGCACHE_DATA_DIR", dataDir)
	t.Setenv("PKGCACHE_ADDR", "127.0.0.1:1")
	t.Setenv("PKGCACHE_LIMIT", "none")

	// The usage text is the output, and a page of it per command is noise in a test log.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = stdout, stderr
		_ = devNull.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range order {
		for _, spelling := range []string{"-h", "--help"} {
			err := commands[name].run(ctx, []string{spelling})
			if err != nil && !errors.Is(err, flag.ErrHelp) {
				t.Errorf("pkgcache %s %s: %v", name, spelling, err)
			}
		}
	}
	for _, dir := range []string{dataDir, home} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 0 {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			slices.Sort(names)
			t.Errorf("asking for help wrote %v into %s", names, dir)
		}
	}
}
