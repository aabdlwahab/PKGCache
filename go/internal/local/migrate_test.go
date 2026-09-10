//go:build !windows

// Hardlink preservation is the half of a move worth testing hardest, and hardlinks are
// what the Unix half of fileIdentity reports on.
package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// cache builds something shaped like a cache: two blobs, one of them linked twice the
// way the store links a blob shared by two projects, a database, a half-finished
// download in staging, and the files a running daemon leaves.
func cache(t *testing.T, root string) {
	t.Helper()
	write := func(rel, content string) string {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	blob := write("blobs/sha256/aa/bb/aabb", "one wheel, stored once")
	write("blobs/sha256/cc/dd/ccdd", "another")
	write("db/catalog.db", "sqlite goes here")
	write("budget.json", `{"limit_bytes":-1}`)
	write("blobs/staging/half.part", "an interrupted download")
	write("daemon.json", `{"pid":1234}`)

	// The same blob under a second name, which is what the move has to preserve.
	linked := filepath.Join(root, "managed", "linked")
	if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, linked); err != nil {
		t.Fatal(err)
	}
}

// acrossFilesystems makes the copy path the one under test. Two directories in one
// temporary directory are always the same filesystem, so without this every test would
// exercise the rename and none would exercise the copy.
func acrossFilesystems(t *testing.T) {
	t.Helper()
	original := renameMove
	renameMove = func(string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { renameMove = original })
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	first, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(first, second)
}

func TestMigrateCopiesACacheWholeAndKeepsItsHardlinks(t *testing.T) {
	acrossFilesystems(t)
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)

	result, err := Migrate(context.Background(), MigrateOptions{
		From: from, To: to, Signpost: from, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Renamed {
		t.Error("a move across filesystems cannot be a rename")
	}

	content, err := os.ReadFile(filepath.Join(to, "blobs", "sha256", "aa", "bb", "aabb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one wheel, stored once" {
		t.Errorf("blob content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(to, "db", "catalog.db")); err != nil {
		t.Errorf("the catalog did not arrive: %v", err)
	}

	// The whole point: one blob under two names is still one blob. A copy that loses
	// this stores the cache twice and nothing ever says so.
	if !sameFile(t,
		filepath.Join(to, "blobs", "sha256", "aa", "bb", "aabb"),
		filepath.Join(to, "managed", "linked")) {
		t.Error("a blob shared by two names was copied twice")
	}

	// What a move deliberately leaves behind.
	if _, err := os.Stat(filepath.Join(to, "blobs", "staging", "half.part")); !os.IsNotExist(err) {
		t.Error("an interrupted download was carried across")
	}
	if _, err := os.Stat(filepath.Join(to, "blobs", "staging")); err != nil {
		t.Errorf("the staging directory itself should still be there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "daemon.json")); !os.IsNotExist(err) {
		t.Error("the state of a daemon on the machine we left was carried across")
	}

	// The old copy is gone, and what stands in its place says where it went.
	if entries, err := os.ReadDir(from); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 || entries[0].Name() != config.MovedToName {
		t.Errorf("the old location holds %d entries, want only the signpost", len(entries))
	}
	target, moved := config.MovedTo(from)
	if !moved || target != to {
		t.Errorf("signpost = %q, %v; want %q", target, moved, to)
	}
}

func TestMigrateWithinAFilesystemIsARename(t *testing.T) {
	base := t.TempDir()
	from, to := filepath.Join(base, "pkgcache"), filepath.Join(base, "moved")
	cache(t, from)

	result, err := Migrate(context.Background(), MigrateOptions{
		From: from, To: to, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Renamed {
		t.Error("a move within one filesystem should not copy anything")
	}
	if _, err := os.Stat(filepath.Join(to, "db", "catalog.db")); err != nil {
		t.Errorf("the catalog did not arrive: %v", err)
	}
	// A rename takes the whole directory, in-flight downloads and all. That is not a
	// contradiction of the skip rules: nothing was copied, so nothing was chosen.
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Error("the old location is still there")
	}
}

func TestMigrateKeepsTheOldCopyWhenAsked(t *testing.T) {
	acrossFilesystems(t)
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)

	result, err := Migrate(context.Background(), MigrateOptions{
		From: from, To: to, Keep: true, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Kept {
		t.Error("result does not record that the old copy was kept")
	}
	if _, err := os.Stat(filepath.Join(from, "db", "catalog.db")); err != nil {
		t.Errorf("the old copy should still be complete: %v", err)
	}
	if result.Files == 0 || result.Bytes == 0 {
		t.Errorf("nothing was counted: %+v", result)
	}
}

func TestMigrateRefusesACacheThatIsInUse(t *testing.T) {
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)

	// Standing in for a daemon: the lock is what one holds, and what a move must not
	// be able to talk its way past.
	held, err := Acquire(LockPath(from), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	_, err = Migrate(context.Background(), MigrateOptions{From: from, To: to, Out: io.Discard})
	if err == nil {
		t.Fatal("a cache in use was moved out from under a daemon")
	}
	if !strings.Contains(err.Error(), "pkgcache stop") {
		t.Errorf("error does not say what to do about it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(from, "db", "catalog.db")); err != nil {
		t.Errorf("the cache should be untouched: %v", err)
	}
}

func TestMigrateRefusesTheDestinationsItCannotUse(t *testing.T) {
	from := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, from)
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "somebody.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		name, to, wants string
	}{
		{"the same directory", from, "already where the cache is"},
		{"inside itself", filepath.Join(from, "deeper"), "inside the cache"},
		{"its own parent", filepath.Dir(from), "contains the cache"},
		{"somebody else's files", occupied, "not empty"},
		{"nowhere at all", "", "name the directory"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := Migrate(context.Background(),
				MigrateOptions{From: from, To: row.to, Out: io.Discard})
			if err == nil {
				t.Fatalf("moving to %q was allowed", row.to)
			}
			if !strings.Contains(err.Error(), row.wants) {
				t.Errorf("error = %v, want it to mention %q", err, row.wants)
			}
		})
	}
}

func TestMigrateBackToTheDefaultLocationTakesTheSignpostDown(t *testing.T) {
	acrossFilesystems(t)
	base := filepath.Join(t.TempDir(), "pkgcache")
	away := filepath.Join(t.TempDir(), "moved")
	cache(t, base)

	if _, err := Migrate(context.Background(), MigrateOptions{
		From: base, To: away, Signpost: base, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	// Back again, with no signpost asked for — which is what the command does when the
	// destination is the default location.
	if _, err := Migrate(context.Background(), MigrateOptions{
		From: away, To: base, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if target, moved := config.MovedTo(base); moved {
		t.Errorf("a signpost still points at %q, from the cache's own location", target)
	}
	if _, err := os.Stat(filepath.Join(base, "db", "catalog.db")); err != nil {
		t.Errorf("the cache did not come back: %v", err)
	}
}

func TestMigrateDryRunChangesNothing(t *testing.T) {
	acrossFilesystems(t)
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)

	var said strings.Builder
	result, err := Migrate(context.Background(), MigrateOptions{
		From: from, To: to, DryRun: true, Signpost: from, Out: &said,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files == 0 {
		t.Error("a dry run should still say how much there is to move")
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Error("a dry run created the destination")
	}
	if _, err := os.Stat(filepath.Join(from, "db", "catalog.db")); err != nil {
		t.Errorf("a dry run disturbed the cache: %v", err)
	}
	if !strings.Contains(said.String(), "would move") {
		t.Errorf("a dry run said nothing useful: %q", said.String())
	}
}

func TestMigrateVerifiesTheCopyBeforeRemovingTheOriginal(t *testing.T) {
	acrossFilesystems(t)
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)

	held, err := measure(from)
	if err != nil {
		t.Fatal(err)
	}
	if held.links != 1 {
		t.Errorf("the fixture should hold one extra link, got %d", held.links)
	}
	if _, err := Migrate(context.Background(), MigrateOptions{
		From: from, To: to, Keep: true, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	arrived, err := measure(to)
	if err != nil {
		t.Fatal(err)
	}
	// Equality of both numbers is what the move itself checks before it deletes
	// anything, so it is what the test asserts.
	if arrived != held {
		t.Errorf("moved cache = %+v, original = %+v", arrived, held)
	}
}

func TestMigrateRefusesAFilesystemWithoutHardlinks(t *testing.T) {
	// Simulated rather than mounted: the check is "did link(2) work", and a test that
	// needed an exFAT partition would run nowhere.
	to := t.TempDir()
	if err := os.Chmod(to, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(to, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory, so there is nothing to refuse")
	}
	err := checkLinks(to)
	if err == nil {
		t.Fatal("a destination that cannot be written to was accepted")
	}
	if !errors.Is(err, os.ErrPermission) && !strings.Contains(err.Error(), "cannot") {
		t.Errorf("error = %v", err)
	}
}
