// Package npm implements an npm pull-through cache.
//
// Packuments are small mutable documents, so the engine caches and revalidates
// them through refs. Tarballs use the ordinary streaming engine path. The adapter
// only understands npm's protocol: it never opens a file, talks to SQLite, hashes
// content, or coordinates concurrent downloads itself.
package npm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
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
		{Methods: []string{http.MethodGet}, Pattern: "/{scope}/{pkg}", Handler: r.metadata},
		{Methods: []string{http.MethodGet}, Pattern: "/{pkg}", Handler: r.metadata},
	}
}

func (r *Repo) metadata(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	name, ok := packageName(p)
	if !ok {
		_ = c.NotFound("invalid package name")
		return
	}

	body, err := r.loadPackument(c, name)
	if err != nil {
		if c.Offline() {
			c.WriteError(err)
		} else {
			_ = c.NotFound("no cached metadata for " + name)
		}
		return
	}
	rewritten, err := rewritePackument(body, name, c.ExternalBase())
	if err != nil {
		_ = c.Text(http.StatusBadGateway, "upstream returned an invalid npm packument")
		return
	}
	if err := c.ServeBytes(http.StatusOK, "application/json", rewritten); err != nil {
		c.WriteError(err)
	}
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

	key := name + "/-/" + filename
	err = c.Serve(engine.Resolution{
		Key:       key,
		Upstream:  c.UpstreamRequest(upstreamURL, nil),
		MediaType: "application/octet-stream",
		Artifact: &catalog.Artifact{
			Name: name, Version: version, Origin: upstreamURL, Extra: extra,
		},
		AccessName: name,
	})
	if err != nil {
		c.WriteError(err)
	}
}

func (r *Repo) loadPackument(c *eco.Ctx, name string) ([]byte, error) {
	origin, ok := c.SingleUpstream()
	if !ok {
		return nil, fmt.Errorf("npm: no upstream configured")
	}
	headers := http.Header{}
	headers.Set("Accept", "application/vnd.npm.install-v1+json, application/json")
	doc, err := c.Document(engine.DocSpec{
		Name:    "packument/" + name,
		Key:     "packument/" + name,
		URL:     eco.JoinURL(origin, packagePath(name)),
		TTL:     r.ttl,
		Headers: headers,
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
	name := p.Unescape("pkg")
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
