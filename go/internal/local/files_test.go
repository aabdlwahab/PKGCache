//go:build !windows

package local

import (
	"os"
	"path/filepath"
	"testing"
)

func newFiles(t *testing.T) (*Files, string) {
	t.Helper()
	home := t.TempDir()
	return &Files{DataDir: t.TempDir(), Home: home}, home
}

// The starting places are the whole of the first screen, and they are named rather than
// spelled: nobody thinks of the stick they just plugged in as /run/media/<user>.
func TestBrowseOffersNamedStartingPlaces(t *testing.T) {
	files, home := newFiles(t)
	if err := os.MkdirAll(ShuttleOut(files.DataDir), 0o755); err != nil {
		t.Fatal(err)
	}

	listing, err := files.Browse("")
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, entry := range listing.Entries {
		if !entry.Dir {
			t.Errorf("a starting place that is not a directory: %+v", entry)
		}
		labels[entry.Label] = entry.Path
	}
	if labels["Home"] != home {
		t.Errorf("Home is %q, want %q", labels["Home"], home)
	}
	if labels["This cache's outbox"] != ShuttleOut(files.DataDir) {
		t.Errorf("the outbox is %q", labels["This cache's outbox"])
	}
	// The inbox does not exist in this cache, and a dead row is worse than none.
	if _, offered := labels["This cache's inbox"]; offered {
		t.Error("a starting place was offered for a directory that does not exist")
	}
}

// Directories and packs, and nothing else. This is for choosing a pack or a place to put
// one; listing every file in a home directory would invite it to be used as something
// else entirely.
func TestBrowseListsOnlyDirectoriesAndPacks(t *testing.T) {
	files, home := newFiles(t)
	for _, name := range []string{"work.tar", "notes.txt", "archive.tar.gz", ".hidden.tar"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	listing, err := files.Browse(home)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	if len(names) != 2 || names[0] != "sub" || names[1] != "work.tar" {
		t.Fatalf("listed %v, want the directory then the pack", names)
	}
	if !listing.Entries[0].Dir || listing.Entries[1].Dir {
		t.Fatalf("directories did not sort first: %+v", listing.Entries)
	}
	if listing.Entries[1].Size != 1 {
		t.Errorf("pack size is %d", listing.Entries[1].Size)
	}
	if listing.Parent != filepath.Dir(home) {
		t.Errorf("parent is %q", listing.Parent)
	}
}

// Writability is asked by writing, not by reading the mode bits: a read-only mount and a
// full disk both say yes to the bits and no to the write, and an export that believed the
// bits would find out after building gigabytes of pack.
func TestBrowseReportsWritability(t *testing.T) {
	files, home := newFiles(t)
	listing, err := files.Browse(home)
	if err != nil {
		t.Fatal(err)
	}
	if !listing.Writable {
		t.Error("a fresh temp directory reports as unwritable")
	}

	locked := filepath.Join(home, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, where the mode bits do not stop a write")
	}
	listing, err = files.Browse(locked)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Writable {
		t.Error("a directory with no write permission reports as writable")
	}
	// And nothing was left behind by asking.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "locked" {
			t.Errorf("the writability probe left %s behind", entry.Name())
		}
	}
}
