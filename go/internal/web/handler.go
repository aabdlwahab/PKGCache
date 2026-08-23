// Package web serves the browser console and its landing/tutorial assets.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// ContentSecurityPolicy is byte-identical to the retired nginx console policy.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// headlessBody is what a browser gets under --headless. Plain text rather than JSON:
// the reader is a person who typed the address, not a program.
const headlessBody = "pkgreg is running with the console disabled (--headless).\n" +
	"The control API is at /api/v1 and metrics are at /metrics.\n"

// asset is one preloaded file. The whole bundle is a few hundred kilobytes of
// embedded data, so it is read and digested once at construction. Serving then costs
// a map lookup and a write, and every response carries a content ETag for free.
type asset struct {
	body        []byte
	etag        string
	contentType string
}

// Handler serves the embedded console.
type Handler struct {
	assets  map[string]asset
	enabled bool
	bytes   int64
}

// New builds the static handler. Pass enabled=false for --headless, where every
// console path answers 404 and only the API and operational endpoints remain.
func New(enabled bool) *Handler {
	h := &Handler{assets: make(map[string]asset), enabled: enabled}
	root := assetFS()
	walk := func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		h.assets[name] = asset{
			body: body,
			// 96 bits of digest. An ETag identifies bytes, and a collision would have
			// to be constructed against a filesystem baked into the binary.
			etag:        `"` + hex.EncodeToString(sum[:12]) + `"`,
			contentType: contentType(path.Ext(name)),
		}
		h.bytes += int64(len(body))
		return nil
	}
	if err := fs.WalkDir(root, ".", walk); err != nil {
		// Reading a compiled-in filesystem cannot fail for any reason an operator
		// could act on; a failure here is a build defect.
		panic("web: reading embedded assets: " + err.Error())
	}
	return h
}

// Enabled reports whether this handler serves the console.
func (h *Handler) Enabled() bool { return h.enabled }

// Assets reports how much is embedded, for `pkgreg doctor`.
func (h *Handler) Assets() (files int, bytes int64) { return len(h.assets), h.bytes }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setSecurityHeaders(w.Header())

	if !h.enabled {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(headlessBody)))
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, headlessBody)
		}
		return
	}

	found, ok := h.assets[assetName(r.URL.Path)]
	if !ok {
		// There is no catch-all fallback to the console shell. Views are addressed by
		// fragment (#/cache), which never reaches the server, so any path that misses
		// here is genuinely wrong — and answering a wrong path with HTML is how a
		// typo ends up cached as a page.
		http.NotFound(w, r)
		return
	}

	header := w.Header()
	header.Set("ETag", found.etag)
	// Asset names are stable, so a URL cannot carry its own version and everything
	// must revalidate. The ETag turns that into a ~200-byte 304 instead of a
	// re-download, which is the right trade for a bundle this size on a LAN.
	header.Set("Cache-Control", "no-cache")
	if matchesETag(r.Header.Get("If-None-Match"), found.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	header.Set("Content-Type", found.contentType)
	header.Set("Content-Length", strconv.Itoa(len(found.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(found.body)
	}
}

// matchesETag implements the If-None-Match list comparison. A cache is allowed to
// weaken a strong validator on the way back, so W/"x" and "x" name the same bytes.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// contentType resolves types from an explicit table rather than mime.TypeByExtension.
// That function consults /etc/mime.types, which a static binary on a scratch image
// does not have — the answer must not depend on what happens to be installed around us.
func contentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".txt", ".md":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// assetName maps a request path to a file in the embedded tree. The console is a
// self-contained subtree under console/, so its modules cannot collide with the
// landing page's assets and both can use unqualified names.
func assetName(requestPath string) string {
	clean := path.Clean("/" + requestPath)
	switch clean {
	case "/", "/landing", "/landing.html":
		return "landing.html"
	case "/tutorial", "/tutorial.html":
		return "tutorial.html"
	case "/console":
		return "console/index.html"
	case "/widget":
		// A second shell over the same modules, for a window somebody leaves open. It
		// lives under console/ so its imports resolve against the console's own tree
		// rather than duplicating any of it.
		return "console/widget.html"
	}
	return strings.TrimPrefix(clean, "/")
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", ContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}
