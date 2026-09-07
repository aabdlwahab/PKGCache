// Package gomod serves the Go module proxy protocol out of this cache.
//
// The shape is PyPI's, deliberately. A project rides the path — /<project>/gomod/… —
// the upstreams are a named set with one public default, metadata revalidates on a TTL
// while artifacts are immutable, and a module zip becomes a catalog artifact the same
// way a wheel does. Everything a person already knows about how this cache treats
// Python is true of how it treats Go.
//
// What is different is that GOPROXY is simpler than a simple index. Every request maps
// to exactly one upstream URL — there is no document to parse and no links to rewrite,
// because the protocol addresses files directly. So this adapter is mostly routing:
//
//	<module>/@v/list          the versions, which grow, so revalidated
//	<module>/@latest          the newest, likewise
//	<module>/@v/<v>.info      immutable once published
//	<module>/@v/<v>.mod       immutable
//	<module>/@v/<v>.zip       immutable, and the artifact people mean by "a module"
//
// The checksum database is deliberately absent. sum.golang.org is a transparency log
// rather than a package index: caching it would mean either serving a stale view of an
// append-only log, or proxying every lookup live, and neither is a cache. Modules are
// still verified — go.sum is checked locally against every zip this serves — so what a
// person gives up by pointing GONOSUMDB at this cache is the log's third-party witness,
// not integrity. See setupSteps, which says so where somebody is about to decide.
package gomod

import (
	"net/http"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

const (
	// ID is the ecosystem identifier.
	ID = "gomod"

	// listTTL bounds how long a version list is trusted. The same five minutes PyPI's
	// simple index uses, and for the same reason: a list that grows is worth
	// revalidating, and a build that resolves a version published four minutes ago is
	// not a case anybody is waiting on.
	listTTL = 5 * time.Minute
)

// defaultProxies is the upstream every project starts with.
//
// One name, one segment. PyPI's indexes are spelled "root/pypi" and could be, because
// its route ends at a literal "+simple" that bounds the greedy capture. Here the module
// path is the greedy capture — it is the thing with slashes in it — so the proxy name
// has to be a single segment for the router to tell one from the other.
var defaultProxies = map[string]string{
	"goproxy": "https://proxy.golang.org",
}

// Repo is the Go module ecosystem.
type Repo struct {
	proxies map[string]string
	ttl     time.Duration
}

// New builds an adapter with the public Go module proxy.
func New() *Repo { return NewWithProxies(defaultProxies) }

// NewWithProxies builds an adapter with an explicit name-to-origin map.
func NewWithProxies(proxies map[string]string) *Repo {
	copied := make(map[string]string, len(proxies))
	for name, origin := range proxies {
		copied[strings.Trim(name, "/")] = strings.TrimRight(origin, "/")
	}
	return &Repo{proxies: copied, ttl: listTTL}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:               ID,
		Display:          "Go modules",
		Summary:          "Go module zips, metadata and version lists, over the module proxy protocol.",
		Storage:          eco.StorageBlob,
		Listener:         eco.ListenerPathPrefixed,
		Upstreams:        eco.UpstreamNamedSet,
		DefaultUpstreams: cloneMap(r.proxies),
		Freshness: func(key string) eco.Freshness {
			// A version list and @latest are answers about a moving target. Everything
			// else the protocol serves is immutable by the module system's own rule:
			// a published version never changes, which is what go.sum depends on.
			if strings.HasPrefix(key, listPrefix) || strings.HasPrefix(key, latestPrefix) {
				return eco.Revalidate(r.ttl)
			}
			return eco.Immutable
		},
		ParseArtifact: parseArtifactKey,
		Setup:         setupSteps,
	}
}

// Key prefixes, which double as the freshness classes above.
const (
	listPrefix   = "list/"
	latestPrefix = "latest/"
	// filesInfix separates the proxy name from the module in an artifact key, the way
	// PyPI's "+f" does. A module path cannot contain it, because "@" is reserved by the
	// protocol itself.
	filesInfix = "/@v/"
)

// Routes implements eco.Ecosystem.
//
// One greedy capture each, bounded by a literal suffix: the module path is the greedy
// one, and "@v" is what tells the router where it ends.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		{
			Methods: []string{http.MethodGet}, Pattern: "/+indexes",
			Handler: r.listProxies, Admin: true,
		},
		{
			Methods: []string{http.MethodGet, http.MethodHead},
			Pattern: "/{index}/{module...}/@v/list", Handler: r.versionList,
		},
		{
			Methods: []string{http.MethodGet, http.MethodHead},
			Pattern: "/{index}/{module...}/@latest", Handler: r.latest,
		},
		{
			Methods: []string{http.MethodGet, http.MethodHead},
			Pattern: "/{index}/{module...}/@v/{file}", Handler: r.file,
		},
	}
}

func (r *Repo) listProxies(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	if err := c.JSON(http.StatusOK, c.Upstreams()); err != nil {
		c.WriteError(err)
	}
}

// versionList serves <module>/@v/list, which is plain text, one version per line.
func (r *Repo) versionList(w http.ResponseWriter, req *http.Request, p router.Params) {
	r.document(w, req, p, "@v/list", listPrefix, "text/plain; charset=utf-8")
}

// latest serves <module>/@latest, which is the JSON of one version.
func (r *Repo) latest(w http.ResponseWriter, req *http.Request, p router.Params) {
	r.document(w, req, p, "@latest", latestPrefix, "application/json")
}

// document serves one of the two mutable answers through the revalidating path.
func (r *Repo) document(
	w http.ResponseWriter, req *http.Request, p router.Params,
	suffix, prefix, mediaType string,
) {
	c := eco.CtxFrom(w, req, p)
	index, module, origin, ok := r.target(c, p)
	if !ok {
		return
	}
	doc, err := c.Document(engine.DocSpec{
		Name: prefix + index + "/" + module,
		Key:  prefix + index + "/" + module,
		URL:  eco.JoinURL(origin, module+"/"+suffix),
		TTL:  r.ttl,
	})
	if err != nil {
		if c.Offline() {
			c.WriteError(err)
		} else {
			_ = c.NotFound("no cached " + suffix + " for " + module)
		}
		return
	}
	// The upstream's own content type where it gave one: a proxy that answers @latest
	// as JSON has said something this does not need to second-guess.
	if doc.MediaType != "" {
		mediaType = doc.MediaType
	}
	_ = c.ServeBytes(http.StatusOK, mediaType, doc.Body)
}

// file serves <module>/@v/<version>.<ext>, which never changes once published.
func (r *Repo) file(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	index, module, origin, ok := r.target(c, p)
	if !ok {
		return
	}
	name := p.Unescape("file")
	version, extension, valid := splitFile(name)
	if !valid {
		_ = c.NotFound("unsupported module file " + name)
		return
	}

	upstreamURL := eco.JoinURL(origin, module+"/@v/"+name)
	var artifact *catalog.Artifact
	if extension == "zip" {
		// The zip is the module. .info and .mod are metadata about it, and recording
		// them as artifacts would list three rows for one dependency.
		artifact = &catalog.Artifact{
			Name: module, Version: version, Origin: upstreamURL,
			Extra: map[string]any{"index": index},
		}
	}
	err := c.Serve(engine.Resolution{
		Key:        index + filesInfix + module + "/" + name,
		Upstream:   c.UpstreamRequest(upstreamURL, nil),
		MediaType:  mediaTypeFor(extension),
		Artifact:   artifact,
		AccessName: module,
	})
	if err != nil {
		c.WriteError(err)
	}
}

// target resolves the proxy and validates the module path, or answers and reports false.
func (r *Repo) target(c *eco.Ctx, p router.Params) (index, module, origin string, ok bool) {
	index = strings.Trim(p.Unescape("index"), "/")
	module = strings.Trim(p.Unescape("module"), "/")
	if index == "" || !validModulePath(module) {
		_ = c.NotFound("invalid proxy or module path")
		return "", "", "", false
	}
	origin, found := c.Upstream(index)
	if !found {
		_ = c.NotFound("unknown proxy " + index)
		return "", "", "", false
	}
	return index, module, origin, true
}

// validModulePath rejects what must never reach an upstream URL or a cache key.
//
// The protocol's own escaping puts "!" before an upper-case letter, so a path arriving
// here is lower-case and safe; what it must not contain is a traversal, an empty segment
// or the "@" the protocol reserves to mark where the module path ends.
func validModulePath(module string) bool {
	if module == "" || strings.Contains(module, "@") {
		return false
	}
	for _, segment := range strings.Split(module, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// splitFile separates "v1.2.3.zip" into its version and extension.
//
// Only the three the protocol defines are served. Anything else is a request this cache
// has no rule for, and forwarding it would mean caching a response whose freshness
// nobody here can reason about.
func splitFile(name string) (version, extension string, ok bool) {
	for _, candidate := range []string{"info", "mod", "zip"} {
		if trimmed, found := strings.CutSuffix(name, "."+candidate); found && trimmed != "" {
			return trimmed, candidate, true
		}
	}
	return "", "", false
}

func mediaTypeFor(extension string) string {
	switch extension {
	case "info":
		return "application/json"
	case "mod":
		return "text/plain; charset=utf-8"
	default:
		return "application/zip"
	}
}

// parseArtifactKey derives the inventory row from a cache key.
//
// Keys are "<proxy>/@v/<module>/<version>.<ext>", and only the zip is an artifact: it is
// the thing a person means by "this module is cached", and the other two are notes about
// it. Arch is empty, because a module is source and has none.
func parseArtifactKey(key string) (name, version, arch string, ok bool) {
	_, rest, found := strings.Cut(key, filesInfix)
	if !found {
		return "", "", "", false
	}
	at := strings.LastIndex(rest, "/")
	if at <= 0 {
		return "", "", "", false
	}
	module, file := rest[:at], rest[at+1:]
	parsedVersion, extension, valid := splitFile(file)
	if !valid || extension != "zip" || module == "" {
		return "", "", "", false
	}
	return module, parsedVersion, "", true
}

func setupSteps(ctx eco.SetupContext) []eco.SetupStep {
	base := "http://" + eco.ClientAuthority(ctx.Host, ctx.Port) + "/" + ctx.Project +
		"/" + ID + "/goproxy"
	return []eco.SetupStep{
		{Comment: "Point the Go toolchain at this cache:"},
		{Command: "export GOPROXY=" + base},
		{Comment: "The checksum database is not served here — it is a transparency log, not a package index, and a cached view of an append-only log is a stale one. go.sum still verifies every module this serves; what this turns off is the third-party witness:"},
		{Command: "export GONOSUMDB=* GONOSUMCHECK=1 GOFLAGS=-mod=mod"},
		{Comment: "Or keep the witness and let the toolchain reach sum.golang.org directly, which needs the network this cache exists to avoid:"},
		{Command: "export GOSUMDB=sum.golang.org"},
	}
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
