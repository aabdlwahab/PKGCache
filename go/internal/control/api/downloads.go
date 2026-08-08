package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/brightskies/pkgreg/internal/clientrelease"
	"github.com/brightskies/pkgreg/internal/control"
)

// Client binaries are served from disk rather than embedded; internal/clientrelease
// explains why and owns the filename grammar, the checksum format and the platform
// set. This file is only the HTTP surface over it.
//
// The tutorial asks what is there and offers only that, so a cache with nothing
// published shows guidance instead of dead links.

func (a *API) downloadRoutes() {
	a.route("GET /api/v1/downloads", a.listDownloads)
	a.route("GET /api/v1/downloads/{name}", a.getDownload)
}

func (a *API) downloadsDir() string {
	return clientrelease.Dir(a.DataDir)
}

// listDownloads reports which client and bridge binaries this instance can hand out.
//
// Unauthenticated, like the CA: you need the client before you have anything to
// authenticate with, and a filename plus a size is not a secret.
func (a *API) listDownloads(w http.ResponseWriter, _ *http.Request) error {
	list, err := clientrelease.List(a.downloadsDir())
	if err != nil || len(list) == 0 {
		// An unreadable or absent directory is the normal state of a fresh instance,
		// not a fault. Either way the answer is "nothing yet, here is how to fix it",
		// and the hint names a command that works on a host holding only this binary.
		writeJSON(w, http.StatusOK, map[string]any{
			"downloads": []clientrelease.Binary{},
			"published": false,
			"hint":      "the operator runs `pkgreg publish-client` on the cache host",
		})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"downloads": list,
		"published": true,
	})
	return nil
}

func (a *API) getDownload(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	if _, ok := clientrelease.Parse(name); !ok {
		return control.NewError(http.StatusNotFound, "not_found", "no such download")
	}
	path := filepath.Join(a.downloadsDir(), name)
	file, err := os.Open(path) // #nosec G304 -- the name is constrained to the release grammar.
	if err != nil {
		return control.NewError(http.StatusNotFound, "not_found",
			"%s has not been published on this instance", name)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return control.NewError(http.StatusNotFound, "not_found", "no such download")
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if sum := clientrelease.Checksums(a.downloadsDir())[name]; sum != "" {
		// Lets a scripted download verify itself without a second request.
		w.Header().Set("X-Pkgreg-SHA256", sum)
	}
	// ServeContent rather than io.Copy: it handles Range and conditional requests, and
	// a 7 MB binary over a slow link is exactly where a resumable download matters.
	http.ServeContent(w, r, name, info.ModTime(), file)
	return nil
}
