// Package files is the generic artifact store: the one ecosystem with a write path.
//
// Paths are addressed like a filesystem, but nothing is stored like one. Content
// lives in the content-addressed blob store and the directory listing is a catalog
// query, so a listing always agrees with what the cache will actually serve — there
// is no tree to drift out of sync, and an artifact ten projects uploaded is one copy
// on disk.
package files

import (
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
)

// ID is the ecosystem identifier.
const ID = "files"

// TokenVerifier checks a scoped project token. Backed by the control plane; an
// interface so this package needs no dependency on it.
type TokenVerifier interface {
	// HasToken reports whether a matching credential exists.
	HasToken(project, eco, scope string) bool
	// VerifyToken verifies one plaintext presentation against persisted token hashes.
	VerifyToken(project, eco, scope, presented string) bool
}

// Repo is the files ecosystem.
type Repo struct {
	tokens   TokenVerifier
	maxBytes int64
}

// New builds the ecosystem. maxBytes of zero means uploads are unlimited.
func New(tokens TokenVerifier, maxBytes int64) *Repo {
	return &Repo{tokens: tokens, maxBytes: maxBytes}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:       ID,
		Display:  "Files",
		Summary:  "Generic artifact store: anonymous download, token-gated upload.",
		Storage:  eco.StorageBlob,
		Listener: eco.ListenerPathPrefixed,
		// There is no upstream: content arrives by upload, not by pull-through.
		Upstreams: eco.UpstreamNone,
		// Uploaded content is immutable until explicitly overwritten, so there is
		// nothing to revalidate against.
		Freshness: func(string) eco.Freshness { return eco.Immutable },
		// Every uploaded path is an artifact in its own right; there is no version
		// or architecture to parse out of it.
		ParseArtifact: func(key string) (string, string, string, bool) {
			if key == "" {
				return "", "", "", false
			}
			return key, "", "", true
		},
		Setup: setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/{path...}", Handler: r.get},
		{Methods: []string{http.MethodPut}, Pattern: "/{path...}", Handler: r.put},
		{Methods: []string{http.MethodDelete}, Pattern: "/{path...}", Handler: r.delete},
	}
}

// reserved names the role owns; they must never be writable or listable.
var reserved = map[string]bool{"ledger.db": true, "catalog.db": true}

// cleanKey turns a request path into a cache key, or reports it as unusable.
//
// Traversal is not a filesystem concern here — content is content-addressed, so a
// key never becomes a path — but a key containing ".." would still be confusing to
// list and impossible to address consistently, so it is rejected at the boundary.
func cleanKey(raw string) (string, bool) {
	raw = strings.Trim(raw, "/")
	if raw == "" {
		return "", true // the root listing
	}
	parts := strings.Split(raw, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch {
		case p == "", p == ".", p == "..":
			return "", false
		case strings.HasPrefix(p, "+"):
			// A leading "+" is the administrative namespace across every ecosystem.
			return "", false
		case reserved[p], strings.HasPrefix(p, "ledger.db"), strings.HasSuffix(p, ".part"):
			return "", false
		}
		out = append(out, p)
	}
	return strings.Join(out, "/"), true
}

// ---- GET / HEAD -----------------------------------------------------------

func (r *Repo) get(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	raw := p.Unescape("path")
	key, ok := cleanKey(raw)
	if !ok {
		_ = plainText(c, http.StatusNotFound, "not found")
		return
	}

	// A trailing slash unambiguously means "list this directory" — wget -r depends
	// on it, and a path can legitimately be both a file and a directory prefix. So
	// the slash decides, and only a slash-less path may resolve to content.
	wantsListing := raw == "" || strings.HasSuffix(raw, "/")

	// Otherwise an exact match wins over a listing of the same prefix.
	if key != "" && !wantsListing {
		if entry, found := c.Entry(key); found {
			err := c.Serve(engine.Resolution{
				Key:        key,
				MediaType:  mediaTypeFor(key, entry.MediaType),
				AccessName: key,
			})
			if err != nil {
				c.WriteError(err)
			}
			return
		}
	}
	r.autoindex(c, key)
}

// autoindex renders a directory listing from the catalog.
//
// This is a prefix query, not a readdir: there is no directory on disk to read. It
// also means a listing can never show an entry the cache would then fail to serve.
func (r *Repo) autoindex(c *eco.Ctx, prefix string) {
	q := prefix
	if q != "" {
		q += "/"
	}
	entries, err := c.ListEntries(q)
	if err != nil {
		c.WriteError(err)
		return
	}
	// The retired filesystem-backed server always had a root directory, even
	// before the first upload, and rendered an empty index for it. The catalog
	// has no directory rows, so preserve that observable contract explicitly
	// for the root while keeping unknown nested prefixes as 404s.
	if len(entries) == 0 && prefix != "" {
		_ = plainText(c, http.StatusNotFound, "not found")
		return
	}

	dirs, files := groupChildren(entries, q)
	base := "/" + q
	var b strings.Builder
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head><meta charset=utf-8><title>Index of %s</title>"+
		"</head><body><h1>Index of %s</h1><pre>\n", html.EscapeString(base), html.EscapeString(base))
	if prefix != "" {
		b.WriteString(`<a href="../">../</a>` + "\n")
	}
	for _, d := range dirs {
		fmt.Fprintf(&b, "<a href=\"%s/\">%s/</a>\n", html.EscapeString(d), html.EscapeString(d))
	}
	for _, f := range files {
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a>\t%s\t%s\n",
			html.EscapeString(f.name), html.EscapeString(f.name),
			f.modified.UTC().Format("2006-01-02 15:04"), formatSize(f.size))
	}
	if len(dirs) == 0 && len(files) == 0 {
		// Python joined an empty row slice between two newline delimiters.
		b.WriteByte('\n')
	}
	b.WriteString("</pre></body></html>\n")
	_ = c.ServeBytes(http.StatusOK, "text/html; charset=utf-8", []byte(b.String()))
}

type child struct {
	name     string
	size     int64
	modified time.Time
}

// groupChildren collapses a flat key list into one level of directories and files,
// which is what makes a flat catalog look like a tree to wget.
func groupChildren(entries []catalog.Entry, prefix string) (dirs []string, files []child) {
	seenDir := map[string]bool{}
	for _, e := range entries {
		rest := strings.TrimPrefix(e.Key, prefix)
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			name := rest[:i]
			if !seenDir[name] {
				seenDir[name] = true
				dirs = append(dirs, name)
			}
			continue
		}
		files = append(files, child{name: rest, size: e.Size, modified: e.CachedAt})
	}
	sort.Strings(dirs)
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return dirs, files
}

// ---- PUT ------------------------------------------------------------------

func (r *Repo) put(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	if !r.authorize(c) {
		return
	}
	raw := p.Unescape("path")
	if strings.HasSuffix(raw, "/") || strings.Trim(raw, "/") == "" {
		_ = plainText(c, http.StatusBadRequest, "PUT requires a file path")
		return
	}
	key, ok := cleanKey(raw)
	if !ok || key == "" {
		_ = plainText(c, http.StatusForbidden, "reserved or invalid path")
		return
	}

	opts := engine.PutOptions{
		MediaType: mediaTypeFor(key, req.Header.Get("Content-Type")),
		Overwrite: isTrue(req.URL.Query().Get("overwrite")),
		MaxBytes:  r.maxBytes,
		Origin:    "upload:" + clientIP(req),
		Artifact:  &catalog.Artifact{Name: key},
	}
	// An X-Checksum-Sha256 header lets a client prove the upload arrived intact.
	if want := req.Header.Get("X-Checksum-Sha256"); want != "" {
		d, err := blob.ParseDigest(want)
		if err != nil {
			_ = plainText(c, http.StatusBadRequest, "X-Checksum-Sha256 is not a sha256 digest")
			return
		}
		opts.ExpectDigest = d
	}

	res, err := c.PutBlob(key, req, opts)
	switch {
	case errors.Is(err, engine.ErrExists):
		_ = plainText(c, http.StatusConflict,
			"already exists (write-once) — retry with ?overwrite=1 to replace")
		return
	case errors.Is(err, engine.ErrChecksum):
		_ = plainText(c, http.StatusBadRequest, "checksum mismatch: the upload does not match X-Checksum-Sha256")
		return
	case errors.Is(err, engine.ErrTooLarge):
		_ = plainText(c, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, engine.ErrReadOnly):
		_ = plainText(c, http.StatusForbidden,
			"read-only: uploads are disabled while this project is offline")
		return
	case err != nil:
		c.WriteError(err)
		return
	}

	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	_ = c.JSON(status, map[string]any{
		"path":   key,
		"size":   res.Size,
		"sha256": res.Digest.String(),
		"url":    c.ExternalBase() + "/" + key,
	})
}

// ---- DELETE ---------------------------------------------------------------

func (r *Repo) delete(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	if !r.authorize(c) {
		return
	}
	key, ok := cleanKey(p.Unescape("path"))
	if !ok || key == "" {
		_ = plainText(c, http.StatusForbidden, "reserved or invalid path")
		return
	}
	if _, found := c.Entry(key); !found {
		_ = plainText(c, http.StatusNotFound, "not found")
		return
	}
	if err := c.DeleteEntry(key); err != nil {
		c.WriteError(err)
		return
	}
	// The blob stays until garbage collection: another project may hold the same
	// bytes, and only the collector knows.
	_ = c.DeleteArtifacts(key)
	c.W.WriteHeader(http.StatusNoContent)
}

// ---- authorization --------------------------------------------------------

// authorize gates writes on the project's token, writing the rejection itself.
func (r *Repo) authorize(c *eco.Ctx) bool {
	if r.tokens == nil || !r.tokens.HasToken(c.Project, ID, "write") {
		_ = plainText(c, http.StatusForbidden,
			"no write token set for this project — generate one in the console")
		return false
	}
	presented := bearerToken(c.R)
	if !r.tokens.VerifyToken(c.Project, ID, "write", presented) {
		_ = plainText(c, http.StatusUnauthorized, "invalid or missing write token")
		return false
	}
	if c.Offline() {
		_ = plainText(c, http.StatusForbidden,
			"read-only: writes are disabled on the air-gapped (OFFLINE) side")
		return false
	}
	return true
}

func plainText(c *eco.Ctx, status int, msg string) error {
	// Starlette's PlainTextResponse does not append a newline. Keep that exact
	// files-role contract because command-line clients surface these errors.
	return c.ServeBytes(status, "text/plain; charset=utf-8", []byte(msg))
}

func bearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); len(auth) > 7 &&
		strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-Token"))
}

// ---- helpers --------------------------------------------------------------

func mediaTypeFor(key, declared string) string {
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	if t := mime.TypeByExtension(path.Ext(key)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func setupSteps(ctx eco.SetupContext) []eco.SetupStep {
	base := fmt.Sprintf("https://%s/%s/files/",
		eco.ClientAuthority(ctx.Host, ctx.Port), ctx.Project)
	return []eco.SetupStep{
		{Comment: "Use the temporary files address from the pkgreg shell. Downloads are anonymous " +
			"(the direct address for managed hosts is " + base + "):"},
		{Command: `wget "${PKGREG_FILES_URL}<path>"`},
		{Comment: "Upload (needs this project's write token, from the console):"},
		{Command: "curl -T <file> \\\n" +
			"     -H \"Authorization: Bearer $TOKEN\" \\\n" +
			"     \"${PKGREG_FILES_URL}<path>\""},
		{Comment: "Uploads are write-once; add ?overwrite=1 to replace."},
	}
}
