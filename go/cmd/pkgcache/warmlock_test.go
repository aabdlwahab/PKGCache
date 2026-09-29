package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The walk takes every lock a monorepo actually has, and none of the ones that belong to
// a dependency, a virtualenv or a tool's cache.
func TestFindLocksTakesTheProjectsAndNotTheirDependencies(t *testing.T) {
	root := t.TempDir()
	write := func(path string) {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"uv.lock",
		"services/a/uv.lock",
		"services/b/deep/nested/pnpm-lock.yaml",
		"web/package-lock.json",
		// Two formats in one directory are both taken: a polyglot service needs both
		// warmed, and picking one of them arbitrarily would warm half of it.
		"web/yarn.lock",
		"tools/npm-shrinkwrap.json",
	}
	for _, path := range want {
		write(path)
	}
	for _, path := range []string{
		// A dependency's own lock, which nobody installs from.
		"web/node_modules/leftpad/package-lock.json",
		"web/node_modules/@scope/pkg/yarn.lock",
		"vendor/thing/uv.lock",
		"services/a/.venv/uv.lock",
		"services/a/venv/uv.lock",
		"services/a/.tox/py312/uv.lock",
		".git/modules/x/uv.lock",
		"lib/site-packages/uv.lock",
		// The copy this command itself leaves behind must never be warmed as a lock.
		"services/a/uv.lock.old",
		"web/package-lock.json.old",
		// Not a lock file at all.
		"services/a/poetry.lock",
		"README.md",
	} {
		write(path)
	}

	found, err := findLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(found))
	for _, path := range found {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(relative))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("findLocks found\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestFindLocksRejectsWhatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "uv.lock")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := findLocks(file); err == nil {
		t.Error("findLocks accepted a file as a root")
	}
	if _, err := findLocks(filepath.Join(root, "nowhere")); err == nil {
		t.Error("findLocks accepted a root that does not exist")
	}
}

// An empty tree is reported by the caller, not mistaken for a tree of failures.
func TestFindLocksOnATreeWithNone(t *testing.T) {
	found, err := findLocks(t.TempDir())
	if err != nil || len(found) != 0 {
		t.Errorf("findLocks = %v, %v", found, err)
	}
}

// The single-lock default and the recursive walk have to agree on what a lock is, or one
// of them warms a file the other refuses to.
func TestLockNamesMatchesLockOrder(t *testing.T) {
	if len(lockNames) != len(lockOrder) {
		t.Fatalf("lockNames has %d entries, lockOrder %d", len(lockNames), len(lockOrder))
	}
	for _, name := range lockOrder {
		if !lockNames[name] {
			t.Errorf("lockOrder has %q and lockNames does not", name)
		}
	}
}

// A lock is left as it is unless -rewrite asks for it: the address a rewrite writes is this
// machine's loopback, which a Docker build and every other machine cannot reach, and it
// used to be written by default. -warm-only is still accepted, and contradicting it is an
// error rather than a guess.
func TestWarmlockLeavesTheLockAloneUnlessAskedToRewrite(t *testing.T) {
	for _, c := range []struct {
		args     []string
		warmOnly bool
		fails    bool
	}{
		{args: nil, warmOnly: true},
		{args: []string{"-warm-only"}, warmOnly: true},
		{args: []string{"-rewrite"}, warmOnly: false},
		{args: []string{"-rewrite", "-warm-only"}, fails: true},
	} {
		request, err := parseWarmlock(append([]string{"-lock", "uv.lock"}, c.args...))
		if c.fails {
			if err == nil {
				t.Errorf("%v: accepted", c.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if request.opts.warmOnly != c.warmOnly {
			t.Errorf("%v: warm only = %v, want %v", c.args, request.opts.warmOnly, c.warmOnly)
		}
	}
}
