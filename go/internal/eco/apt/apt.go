// Package apt implements the shared apt/apk HTTP forward proxy.
//
// Volatile repository indexes are buffered documents and conditionally revalidated
// through catalog refs. Package files and by-hash indexes are immutable streaming
// entries. The adapter accepts absolute-form proxy targets and never implements
// CONNECT: apt/apk repositories must use HTTP when routed through this listener.
package apt

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

const (
	// ID is the ecosystem identifier for the combined apt/apk proxy.
	ID = "apt"
)

var volatileExact = map[string]bool{
	"InRelease":       true,
	"Release":         true,
	"Release.gpg":     true,
	"APKINDEX.tar.gz": true,
}

var volatilePrefixes = []string{"Packages", "Sources", "Contents"}

// Repo is the apt/apk ecosystem.
type Repo struct{}

// New builds the apt/apk forward-proxy adapter.
func New() *Repo { return &Repo{} }

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:        ID,
		Display:   "apt / apk",
		Summary:   "Debian and Alpine repository indexes and packages.",
		Storage:   eco.StorageBlob,
		Listener:  eco.ListenerForwardProxy,
		Upstreams: eco.UpstreamNone,
		Freshness: func(key string) eco.Freshness {
			if strings.HasPrefix(key, "volatile/") {
				return eco.Revalidate(0)
			}
			return eco.Immutable
		},
		ParseArtifact: parseArtifactKey,
		Setup:         setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{{
		Methods: []string{http.MethodGet, http.MethodHead},
		Pattern: "/{path...}",
		Handler: r.proxy,
	}}
}

func (r *Repo) proxy(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	target, err := reconstructTarget(req)
	if err != nil {
		_ = c.Text(http.StatusBadRequest, err.Error())
		return
	}
	if !c.ProxyHostAllowed(target.Hostname()) {
		_ = c.Text(http.StatusForbidden,
			"proxy target is not present in server.proxy_allowlist")
		return
	}

	filename := fileName(target)
	key := cacheKey(target)
	if isVolatile(filename) {
		r.serveVolatile(c, target, key)
		return
	}
	r.serveImmutable(c, target, key, filename)
}

func (r *Repo) serveVolatile(c *eco.Ctx, target *url.URL, key string) {
	name := "volatile/" + key
	doc, err := c.Document(engine.DocSpec{
		Name: name,
		Key:  name,
		URL:  target.String(),
		TTL:  0,
	})
	if err != nil {
		if status, ok := engine.UpstreamStatus(err); ok {
			c.W.WriteHeader(status)
			return
		}
		c.WriteError(err)
		return
	}

	// Forward the validators recorded by the shared ref machinery. This lets apt's
	// own cache make conditional requests to pkgreg without exposing sidecar files.
	if ref, ok := c.Ref(name); ok {
		if ref.ETag != "" {
			c.W.Header().Set("ETag", ref.ETag)
		}
		if ref.LastModified != "" {
			c.W.Header().Set("Last-Modified", ref.LastModified)
		}
	}
	c.W.Header().Set("Content-Length", fmt.Sprint(len(doc.Body)))
	c.W.Header().Set("Cache-Control", "no-cache")
	mediaType := doc.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if err := c.ServeBytes(http.StatusOK, mediaType, doc.Body); err != nil {
		c.WriteError(err)
	}
}

func (r *Repo) serveImmutable(c *eco.Ctx, target *url.URL, key, filename string) {
	var artifact *catalog.Artifact
	if parsed, ok := parseArtifact(target); ok {
		artifact = &catalog.Artifact{
			Name: parsed.name, Version: parsed.version, Arch: parsed.arch,
			Origin: target.String(),
			Extra:  map[string]any{"format": parsed.format},
		}
	}
	resolution := engine.Resolution{
		Key:      "immutable/" + key,
		Upstream: c.UpstreamRequest(target.String(), nil),
		// Match the retired FileResponse contract for package archives. In
		// particular, .apk is gzip-framed internally but was intentionally served
		// as opaque bytes rather than application/x-gzip.
		MediaType: "application/octet-stream",
		Artifact:  artifact,
	}
	if artifact != nil {
		resolution.AccessName = artifact.Name
	}
	if err := c.Serve(resolution); err != nil {
		if status, ok := engine.UpstreamStatus(err); ok {
			c.W.WriteHeader(status)
			return
		}
		c.WriteError(err)
	}
}

func reconstructTarget(r *http.Request) (*url.URL, error) {
	var target url.URL
	switch {
	case r.URL.IsAbs():
		target = *r.URL
	default:
		if r.Host == "" {
			return nil, fmt.Errorf("forward proxy requires an absolute target or Host header")
		}
		target = url.URL{
			Scheme:   "http",
			Host:     r.Host,
			Path:     r.URL.Path,
			RawPath:  r.URL.RawPath,
			RawQuery: r.URL.RawQuery,
		}
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("unsupported proxy target scheme %q", target.Scheme)
	}
	if target.Host == "" || target.Hostname() == "" {
		return nil, fmt.Errorf("proxy target has no host")
	}
	if target.User != nil {
		return nil, fmt.Errorf("proxy target must not contain upstream credentials")
	}
	target.Fragment = ""
	if target.Path == "" {
		target.Path = "/"
	}
	return &target, nil
}

func cacheKey(target *url.URL) string {
	host := strings.ToLower(target.Host)
	key := target.Scheme + "://" + host + target.EscapedPath()
	if target.RawQuery != "" {
		key += "?" + target.RawQuery
	}
	return key
}

func fileName(target *url.URL) string {
	raw := path.Base(target.EscapedPath())
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

func isVolatile(filename string) bool {
	if volatileExact[filename] {
		return true
	}
	for _, prefix := range volatilePrefixes {
		if strings.HasPrefix(filename, prefix) {
			return true
		}
	}
	return false
}

type artifactIdentity struct {
	name, version, arch, format string
}

func parseArtifact(target *url.URL) (artifactIdentity, bool) {
	filename := fileName(target)
	if strings.HasSuffix(filename, ".apk") {
		stem := strings.TrimSuffix(filename, ".apk")
		parts := strings.Split(stem, "-")
		if len(parts) < 3 {
			return artifactIdentity{name: stem, format: "apk"}, true
		}
		revision := parts[len(parts)-1]
		version := parts[len(parts)-2] + "-" + revision
		name := strings.Join(parts[:len(parts)-2], "-")
		arch := apkArch(target.Path)
		return artifactIdentity{name: name, version: version, arch: arch, format: "apk"}, true
	}
	for _, suffix := range []string{".deb", ".udeb"} {
		if !strings.HasSuffix(filename, suffix) {
			continue
		}
		stem := strings.TrimSuffix(filename, suffix)
		parts := strings.SplitN(stem, "_", 3)
		identity := artifactIdentity{name: parts[0], format: strings.TrimPrefix(suffix, ".")}
		if len(parts) > 1 {
			identity.version = parts[1]
		}
		if len(parts) > 2 {
			identity.arch = parts[2]
		}
		return identity, identity.name != ""
	}
	return artifactIdentity{}, false
}

func apkArch(targetPath string) string {
	parts := strings.Split(strings.Trim(targetPath, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	candidate := parts[len(parts)-2]
	switch candidate {
	case "aarch64", "armhf", "armv7", "ppc64le", "riscv64", "s390x", "x86", "x86_64":
		return candidate
	default:
		return ""
	}
}

func parseArtifactKey(key string) (name, version, arch string, ok bool) {
	u, err := url.Parse(strings.TrimPrefix(strings.TrimPrefix(key, "immutable/"), "volatile/"))
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	identity, ok := parseArtifact(u)
	return identity.name, identity.version, identity.arch, ok
}

func setupSteps(_ eco.SetupContext) []eco.SetupStep {
	return []eco.SetupStep{
		{Comment: "The pkgreg shell provides the apt/apk proxy address. sudo below is for changing operating-system packages, not for pkgreg setup:"},
		{Command: `sudo apt-get -o Acquire::http::Proxy="$PKGREG_APT_PROXY" update`},
		{Command: `http_proxy="$PKGREG_APT_PROXY" apk add --no-cache <package>`},
	}
}
