package api

import (
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Choosing a place on this machine, from the window.
//
// Export writes a pack and import reads one, and until now a browser could name neither:
// the job took a basename and put it in the cache's own shuttle directory, because "a
// browser cannot move a file off the machine anyway". True of a browser talking to a
// server across a network — and false of the one case this surface exists for, where the
// window and the cache are the same machine and the pack is going onto a USB stick the
// person is holding.
//
// So it is local-only, in the same sense as LocalSources and LocalSelection: a server
// supplies nothing and answers 404, and no amount of asking will make a remote pkgreg
// list its filesystem. What it widens on a local cache is real but narrow — the window
// gains what `pkgcache export -file /media/usb/work.tar` has always been able to do from
// a terminal, on the same machine, as the same user.
//
// It never returns file contents. Directories and .tar files, by name and size, and
// nothing else is readable through it.

// LocalFiles browses the machine a local cache runs on, so a window can pick a
// destination for a pack or a pack to import.
type LocalFiles interface {
	// Browse lists one directory. An empty path means the starting places.
	Browse(path string) (Listing, error)
}

// Listing is one directory, or the set of places to start from.
type Listing struct {
	// Path is the directory listed, empty for the starting places.
	Path string `json:"path"`
	// Parent is the directory above, empty at a root or at the starting places.
	Parent string `json:"parent"`
	// Writable reports whether a pack could be written here, which is the question the
	// export side of this is asking.
	Writable bool       `json:"writable"`
	Entries  []DirEntry `json:"entries"`
}

// DirEntry is one directory or one pack.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
	// Label names a starting place — "Home", "This cache's outbox" — so the first
	// screen reads as places rather than as paths.
	Label string `json:"label,omitempty"`
}

// requireFiles refuses where nothing implements the surface, which is every server.
func (a *API) requireFiles() error {
	if a.Files == nil {
		return control.NewError(http.StatusNotFound, "no_local_files",
			"this instance does not browse a machine of its own")
	}
	return nil
}

func (a *API) browseFiles(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireFiles(); err != nil {
		return err
	}
	// RequireOperate rather than RequireAuthed: listing somebody's filesystem is not a
	// thing a read-only viewer of one project should be able to do, even on a cache
	// where that distinction is currently theoretical.
	if _, _, err := a.requireOperate(r, projectName(r)); err != nil {
		return err
	}
	listing, err := a.Files.Browse(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	if listing.Entries == nil {
		listing.Entries = []DirEntry{}
	}
	writeJSON(w, http.StatusOK, listing)
	return nil
}
