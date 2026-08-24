package app

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/feed"
)

// Serving the apt repository.
//
// A repository is a directory tree fetched by a program that cannot log in, so this is
// the one surface here that is deliberately public and deliberately plain: GET a path,
// get a file. apt has no credentials, no cookies and no ability to follow an
// authentication flow, and a repository behind a session check is a repository nothing
// can install from.
//
// What guards it instead is the signature. Every byte apt trusts is named, with its hash,
// in a Release file signed by the instance's key — so serving these files to anybody is
// not a weakness, and neither is a cached or mirrored copy. The interesting attack is not
// reading this tree, it is writing it, and that is `publish-apt` on the host.
//
// The other reason it is separate from the downloads API: that one serves a flat list of
// filenames matched against a strict grammar, and a repository is nested paths. Reusing it
// would have meant loosening the grammar that keeps it safe.

// aptPrefix is the URL prefix the repository lives under.
const aptPrefix = "/apt/"

// aptPath reports whether a request belongs to the repository.
func aptPath(name string) bool {
	return name == strings.TrimSuffix(aptPrefix, "/") || strings.HasPrefix(name, aptPrefix)
}

// serveApt hands out the published repository.
func (a *App) serveApt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeText(w, http.StatusMethodNotAllowed, "the repository is read-only")
		return
	}

	root := feed.RepoDir(a.Config.Current().DataDir)
	// os.Root rather than filepath.Join and a hopeful check. It resolves every component
	// inside the root, so neither "../" nor a symlink planted in the tree can escape —
	// and this handler serves a path straight from a URL, which is exactly where that
	// distinction earns its keep.
	opened, err := os.OpenRoot(root)
	if err != nil {
		// The normal state of an instance that has never published a package. Said as
		// such, because "404" alone sends an operator looking for a routing bug.
		writeText(w, http.StatusNotFound,
			"this instance publishes no apt repository yet.\n"+
				"The operator creates one with `pkgreg publish-apt`.")
		return
	}
	defer func() { _ = opened.Close() }()

	relative := strings.TrimPrefix(r.URL.Path, aptPrefix)
	relative = strings.TrimPrefix(path.Clean("/"+relative), "/")
	if relative == "" || relative == "." {
		// No listing at the root. Nothing needs it: apt is told exactly which paths to
		// fetch by the sources file, and a listing is only ever a map for somebody else.
		writeText(w, http.StatusNotFound, "no index here; see dists/ and pool/")
		return
	}

	file, err := opened.Open(relative)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeText(w, http.StatusNotFound, "no such file in the repository")
			return
		}
		// A traversal attempt lands here rather than being served. It is not worth a
		// distinct status: as far as a client is concerned the file is simply not there.
		writeText(w, http.StatusNotFound, "no such file in the repository")
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		writeText(w, http.StatusNotFound, "no such file in the repository")
		return
	}

	w.Header().Set("Content-Type", aptContentType(relative))
	// The indexes change whenever a package is published and are small; the pool is
	// content-addressed by name and version and never changes under a client. Telling a
	// cache the difference is most of what keeps `apt update` cheap.
	if strings.HasPrefix(relative, "pool/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	// ServeContent, not io.Copy: it handles Range and conditional requests, and a 15 MB
	// package over a slow link is exactly where a resumable download matters.
	http.ServeContent(w, r, path.Base(relative), info.ModTime(), file)
}

// aptContentType names what a repository file is.
//
// Plain text for everything a person might open in a browser while working out why an
// update failed, which is most of the tree.
func aptContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".deb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(name, ".gz"):
		return "application/gzip"
	default:
		return "text/plain; charset=utf-8"
	}
}
