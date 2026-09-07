package local

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
)

// Browsing this machine, for the two things a pack has to name: where it goes, and which
// one to read back.
//
// Only directories and .tar files are ever listed. That is not a security boundary — the
// daemon runs as the user and could read anything they can — it is an honesty one: this
// exists to choose a pack or a place to put one, and listing a home directory's every
// file would invite it to be used as something else.

// Files implements the control plane's machine browser for a local cache.
type Files struct {
	// DataDir is the cache, whose shuttle directories are two of the starting places.
	DataDir string
	// Home overrides the user's home directory. Empty means the real one; tests set it.
	Home string
}

// packSuffix is the only file extension this lists. A pack is a tar and nothing else is
// any of this surface's business.
const packSuffix = ".tar"

// Browse lists one directory, or the starting places when the path is empty.
func (f *Files) Browse(path string) (controlapi.Listing, error) {
	if strings.TrimSpace(path) == "" {
		return f.roots(), nil
	}
	// Cleaned and made absolute before anything touches the disk, so "." and a trail of
	// ".." resolve to one canonical answer rather than to different ones per caller.
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return controlapi.Listing{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return controlapi.Listing{}, err
	}
	listing := controlapi.Listing{Path: path, Writable: writableDir(path)}
	if parent := filepath.Dir(path); parent != path {
		listing.Parent = parent
	}
	for _, entry := range entries {
		name := entry.Name()
		// Dotfiles are hidden for the reason every file chooser hides them: a person
		// looking for a USB stick is not looking through .cache.
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case entry.IsDir():
			listing.Entries = append(listing.Entries, controlapi.DirEntry{
				Name: name, Path: filepath.Join(path, name), Dir: true,
			})
		case strings.HasSuffix(name, packSuffix):
			row := controlapi.DirEntry{Name: name, Path: filepath.Join(path, name)}
			if info, err := entry.Info(); err == nil {
				row.Size = info.Size()
			}
			listing.Entries = append(listing.Entries, row)
		}
	}
	// Directories first, then packs, each by name: the order somebody scanning for a
	// place to descend into reads them in.
	sort.SliceStable(listing.Entries, func(i, j int) bool {
		if listing.Entries[i].Dir != listing.Entries[j].Dir {
			return listing.Entries[i].Dir
		}
		return listing.Entries[i].Name < listing.Entries[j].Name
	})
	return listing, nil
}

// roots are the places worth starting from, which is the whole of what the first screen
// shows.
//
// Named rather than spelled as paths, because "/run/media/ayasser" is not how anybody
// thinks about the stick they just plugged in. Only those that exist are offered: an
// empty list of removable drives is more useful than three dead rows.
func (f *Files) roots() controlapi.Listing {
	listing := controlapi.Listing{}
	add := func(label, path string) {
		if path == "" {
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		listing.Entries = append(listing.Entries, controlapi.DirEntry{
			Name: filepath.Base(path), Path: path, Dir: true, Label: label,
		})
	}
	home := f.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	add("Home", home)
	if f.DataDir != "" {
		add("This cache's outbox", ShuttleOut(f.DataDir))
		add("This cache's inbox", ShuttleIn(f.DataDir))
	}
	// Where Linux mounts removable media, which is what a pack is usually going onto.
	// Both spellings: /media/<user> is Debian and Ubuntu, /run/media/<user> is what
	// udisks2 uses on Fedora and friends.
	if user := filepath.Base(home); user != "" && user != string(filepath.Separator) {
		add("Removable media", filepath.Join("/media", user))
		add("Removable media", filepath.Join("/run/media", user))
	}
	add("Media", "/media")
	add("Mounted", "/mnt")
	return listing
}

// writableDir reports whether a pack could be written into this directory.
//
// Asked by creating and removing a file rather than by reading the mode bits, because
// the mode is not the answer: a read-only mount, a full disk and an ACL all say yes to
// the bits and no to the write. The export that follows would fail after building a
// pack, which for gigabytes is a long way to go to find out.
func writableDir(path string) bool {
	probe, err := os.CreateTemp(path, ".pkgcache-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// Files is what it claims to be.
var _ controlapi.LocalFiles = (*Files)(nil)
