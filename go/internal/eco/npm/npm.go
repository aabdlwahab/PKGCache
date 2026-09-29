// Package npm implements an npm pull-through cache.
//
// Packuments are small mutable documents, so the engine caches and revalidates
// them through refs. Tarballs use the ordinary streaming engine path. The adapter
// only understands npm's protocol: it never opens a file, talks to SQLite, hashes
// content, or coordinates concurrent downloads itself.
package npm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

const (
	// ID is the ecosystem identifier.
	ID = "npm"

	defaultOrigin = "https://registry.npmjs.org"
	packumentTTL  = time.Minute
)

// Repo is the npm ecosystem.
type Repo struct {
	origin string
	ttl    time.Duration
}

// New builds an npm adapter using the public npm registry.
func New() *Repo { return NewWithOrigin(defaultOrigin) }

// NewWithOrigin builds an npm adapter with a specific registry. It is useful for
// private registries and deterministic integration tests.
func NewWithOrigin(origin string) *Repo {
	if origin == "" {
		origin = defaultOrigin
	}
	return &Repo{origin: strings.TrimRight(origin, "/"), ttl: packumentTTL}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:        ID,
		Display:   "npm",
		Summary:   "npm packuments and package tarballs.",
		Storage:   eco.StorageBlob,
		Listener:  eco.ListenerPathPrefixed,
		Upstreams: eco.UpstreamSingle,
		DefaultUpstreams: map[string]string{
			"registry": r.origin,
		},
		Freshness: func(key string) eco.Freshness {
			if strings.HasPrefix(key, "packument/") {
				return eco.Revalidate(r.ttl)
			}
			return eco.Immutable
		},
		ParseArtifact: parseArtifactKey,
		Companions:    companions,
		Setup:         setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		// The router intentionally has no partial-segment captures. A scoped package
		// therefore captures "@scope" as an ordinary segment and validates it in
		// packageName. The one-segment forms also accept npm's encoded
		// "@scope%2Fname" packument request.
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/{scope}/{pkg}/-/{filename}", Handler: r.tarball},
		{Methods: []string{http.MethodGet, http.MethodHead}, Pattern: "/{pkg}/-/{filename}", Handler: r.tarball},
		{Methods: []string{http.MethodGet}, Pattern: "/{scope}/{pkg}/{version}", Handler: r.metadata},
		{Methods: []string{http.MethodGet}, Pattern: "/{scope}/{pkg}", Handler: r.metadata},
		{Methods: []string{http.MethodGet}, Pattern: "/{pkg}", Handler: r.metadata},
	}
}

// metadata answers a packument, or one version of it.
//
// The one-version form — /name/1.0.0, /name/latest — is npm's per-version document, and
// it is what corepack asks for: it resolves "latest" and finds a pinned package manager's
// tarball and signatures that way, and nothing else. It answered 404, so a Dockerfile's
// `corepack enable pnpm` fetched pnpm past the cache. The entry is taken from the
// packument this cache already holds for the tarball path, which carries everything
// corepack reads, so a version costs no request of its own and is there offline.
func (r *Repo) metadata(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	name, selector, ok := packageRequest(p)
	if !ok {
		_ = c.NotFound("invalid package name")
		return
	}

	body, err := r.loadPackument(c, name)
	if err != nil {
		c.WriteLookupError(err, "no metadata for "+name)
		return
	}
	rewritten, err := rewritePackument(body, name, c.ExternalBase())
	if err != nil {
		_ = c.Text(http.StatusBadGateway, "upstream returned an invalid npm packument")
		return
	}
	if selector != "" {
		entry, found := versionEntry(rewritten, selector)
		if !found {
			_ = c.NotFound("no version or tag " + selector + " of " + name)
			return
		}
		rewritten = entry
	}
	if err := c.ServeBytes(http.StatusOK, "application/json", rewritten); err != nil {
		c.WriteError(err)
	}
}

// packageRequest reads a metadata request in every spelling npm clients use:
//
//	/name   /@scope%2Fname   /@scope/name                        the packument
//	/name/1.0.0   /@scope%2Fname/latest   /@scope/name/1.0.0     one version
//
// Two segments are ambiguous only in appearance: a scope starts with "@", a name never
// does, so /@scope/name is a packument and /name/1.0.0 a version.
func packageRequest(p router.Params) (name, selector string, ok bool) {
	scope := p.Unescape("scope")
	switch {
	case p.Has("version"): // /@scope/name/1.0.0
		name, ok = packageName(p)
		selector = p.Unescape("version")
	case p.Has("scope") && (!strings.HasPrefix(scope, "@") || strings.Contains(scope, "/")):
		// /name/1.0.0, or /@scope%2Fname/1.0.0
		name, ok = validName(scope)
		selector = p.Unescape("pkg")
	default: // /name, /@scope%2Fname, /@scope/name
		name, ok = packageName(p)
		return name, "", ok
	}
	if !ok || selector == "" || strings.Contains(selector, "/") {
		return "", "", false
	}
	return name, selector, true
}

// versionEntry is one version of a packument, found by version or by dist-tag.
func versionEntry(packument []byte, selector string) ([]byte, bool) {
	var root struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if json.Unmarshal(packument, &root) != nil {
		return nil, false
	}
	if entry, ok := root.Versions[selector]; ok {
		return entry, true
	}
	if version, ok := root.DistTags[selector]; ok {
		entry, found := root.Versions[version]
		return entry, found
	}
	return nil, false
}

func (r *Repo) tarball(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	name, ok := packageName(p)
	if !ok {
		_ = c.NotFound("invalid package name")
		return
	}
	filename := p.Unescape("filename")
	if filename == "" || strings.Contains(filename, "/") {
		_ = c.NotFound("invalid tarball name")
		return
	}

	key := name + "/-/" + filename
	// The conventional address, and where its fetch went, relay included: the address
	// an upstream error names.
	conventional, triedAt := "", ""

	// Straight to the registry's own path first. A tarball used to be found through its
	// packument, and that turned every tarball into two fetches at every tier: a
	// frozen-lockfile install asks for tarballs and no packuments, so each one cost a
	// multi-megabyte document nobody requested before a byte of the tarball moved — and
	// behind a team cache, twice. On a congested link a 168 KB tarball took 73 s that
	// way and pnpm gave up on it. Every registry in common use — npmjs, a team cache,
	// Verdaccio, Nexus, Artifactory, GitLab — keeps a tarball at <name>/-/<file>; one
	// that keeps it elsewhere answers 404 there and says where in its packument.
	if version, ok := tarballVersion(name, filename); ok {
		if origin, ok := c.SingleUpstream(); ok {
			conventional = eco.JoinURL(origin, packagePath(name)+"/-/"+url.PathEscape(filename))
			triedAt = c.UpstreamRequest(conventional, nil).URL
			err := r.serveTarball(c, key, name, version, conventional, nil)
			if status, isStatus := engine.UpstreamStatus(err); err == nil ||
				!isStatus || status != http.StatusNotFound {
				if err != nil {
					c.WriteError(err)
				}
				return
			}
		}
	}

	body, err := r.loadPackument(c, name)
	if err != nil {
		c.WriteError(err)
		return
	}
	version, upstreamURL, extra, ok := findTarball(body, filename)
	if !ok {
		_ = c.NotFound("unknown tarball " + filename)
		return
	}
	if upstreamURL == conventional {
		// The packument names the address that just answered 404: that is the answer.
		_ = c.NotFound("tarball " + filename + " is missing from the registry")
		return
	}
	// Both addresses are fetched under the tarball's one key, so this request can meet
	// another's first try, still asking the conventional path, and be handed its 404 —
	// an answer about the wrong address. Twelve clients asking for one tarball at once
	// did exactly that. That fetch is over by the time its error arrives, so asking again
	// joins the fetch of the right address, or starts it.
	for attempt := 1; ; attempt++ {
		err := r.serveTarball(c, key, name, version, upstreamURL, extra)
		if triedAt != "" && missedAt(err) == triedAt && attempt < 5 {
			continue
		}
		if err != nil {
			c.WriteError(err)
		}
		return
	}
}

// missedAt is the address an upstream answered 404 for, or "" for any other outcome.
func missedAt(err error) string {
	var status *engine.UpstreamHTTPError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return status.URL
	}
	return ""
}

func (r *Repo) serveTarball(
	c *eco.Ctx, key, name, version, upstreamURL string, extra map[string]any,
) error {
	return c.Serve(engine.Resolution{
		Key:       key,
		Upstream:  c.UpstreamRequest(upstreamURL, nil),
		MediaType: "application/octet-stream",
		Artifact: &catalog.Artifact{
			Name: name, Version: version, Origin: upstreamURL, Extra: extra,
		},
		AccessName: name,
	})
}

// tarballVersion reads the version from a tarball's conventional name, <name>-<version>.tgz
// with the scope left off. Found by its prefix rather than by the last hyphen, which is
// inside the version whenever the version is a prerelease: react-19.0.0-rc-1.tgz.
func tarballVersion(name, filename string) (string, bool) {
	unscoped := name[strings.LastIndex(name, "/")+1:]
	version, ok := strings.CutPrefix(filename, unscoped+"-")
	if !ok {
		return "", false
	}
	version, ok = strings.CutSuffix(version, ".tgz")
	return version, ok && version != ""
}

func (r *Repo) loadPackument(c *eco.Ctx, name string) ([]byte, error) {
	origin, ok := c.SingleUpstream()
	if !ok {
		return nil, fmt.Errorf("npm: no upstream configured")
	}
	headers := http.Header{}
	headers.Set("Accept", "application/vnd.npm.install-v1+json, application/json")
	doc, err := c.Document(engine.DocSpec{
		Name:         "packument/" + name,
		Key:          "packument/" + name,
		URL:          eco.JoinURL(origin, packagePath(name)),
		TTL:          r.ttl,
		Headers:      headers,
		Compressible: true,
	})
	if err != nil {
		return nil, err
	}
	return doc.Body, nil
}

// packageName handles both npm spellings for scoped packages:
//
//	/@scope%2Fname       one encoded segment
//	/@scope/name         two literal segments
func packageName(p router.Params) (string, bool) {
	if p.Has("scope") {
		scope, pkg := p.Unescape("scope"), p.Unescape("pkg")
		if !strings.HasPrefix(scope, "@") || len(scope) == 1 ||
			pkg == "" || strings.Contains(pkg, "/") {
			return "", false
		}
		return scope + "/" + pkg, true
	}
	return validName(p.Unescape("pkg"))
}

// validName accepts "name" and "@scope/name", decoded.
func validName(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if strings.HasPrefix(name, "@") {
		parts := strings.Split(name, "/")
		if len(parts) != 2 || len(parts[0]) == 1 || parts[1] == "" {
			return "", false
		}
		return name, true
	}
	return name, !strings.Contains(name, "/")
}

func packagePath(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// rewritePackument changes only versions.*.dist.tarball. Every unknown field is
// retained as json.RawMessage, so npm extensions survive instead of being projected
// through a lossy hand-written struct.
func rewritePackument(body []byte, name, externalBase string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("npm: decode packument: %w", err)
	}
	var versions map[string]json.RawMessage
	if raw := root["versions"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, fmt.Errorf("npm: decode versions: %w", err)
		}
	}
	for version, raw := range versions {
		var meta map[string]json.RawMessage
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		var dist map[string]json.RawMessage
		if json.Unmarshal(meta["dist"], &dist) != nil {
			continue
		}
		var tarball string
		if json.Unmarshal(dist["tarball"], &tarball) != nil || tarball == "" {
			continue
		}
		filename := tarballFilename(tarball)
		if filename == "" {
			continue
		}
		rewritten := strings.TrimRight(externalBase, "/") + "/" +
			packagePath(name) + "/-/" + url.PathEscape(filename)
		dist["tarball"], _ = json.Marshal(rewritten)
		meta["dist"], _ = json.Marshal(dist)
		versions[version], _ = json.Marshal(meta)
	}
	root["versions"], _ = json.Marshal(versions)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("npm: encode packument: %w", err)
	}
	return out, nil
}

func findTarball(body []byte, filename string) (
	version, upstreamURL string, extra map[string]any, ok bool,
) {
	var root struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if json.Unmarshal(body, &root) != nil {
		return "", "", nil, false
	}
	for ver, raw := range root.Versions {
		var meta struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Shasum    string `json:"shasum"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		}
		if json.Unmarshal(raw, &meta) != nil ||
			tarballFilename(meta.Dist.Tarball) != filename {
			continue
		}
		extra = map[string]any{}
		if meta.Dist.Shasum != "" {
			extra["shasum"] = meta.Dist.Shasum
		}
		if meta.Dist.Integrity != "" {
			extra["integrity"] = meta.Dist.Integrity
		}
		if len(extra) == 0 {
			extra = nil
		}
		return ver, meta.Dist.Tarball, extra, true
	}
	return "", "", nil, false
}

func tarballFilename(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	filename, err := url.PathUnescape(path.Base(u.EscapedPath()))
	if err != nil || filename == "." || filename == "/" {
		return ""
	}
	return filename
}

func parseArtifactKey(key string) (name, version, arch string, ok bool) {
	name, filename, found := strings.Cut(key, "/-/")
	if !found || name == "" || filename == "" {
		return "", "", "", false
	}
	if version, ok := tarballVersion(name, filename); ok {
		return name, version, "", true
	}
	stem := strings.TrimSuffix(filename, ".tgz")
	if i := strings.LastIndex(stem, "-"); i >= 0 && i+1 < len(stem) {
		version = stem[i+1:]
	}
	return name, version, "", true
}

func setupSteps(_ eco.SetupContext) []eco.SetupStep {
	return []eco.SetupStep{
		{Comment: "The pkgreg shell already points npm at this project. This downloads a tarball without changing package.json:"},
		{Command: "npm pack <package>"},
	}
}
