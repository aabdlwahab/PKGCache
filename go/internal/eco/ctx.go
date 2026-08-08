package eco

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
	"github.com/brightskies/pkgreg/internal/upstream"
)

// Ctx is an adapter's entire view of the system.
//
// The rule this enforces: an adapter contains protocol logic and nothing else. No
// SQL, no filesystem, no HTTP client, no single-flight, no hashing, no integrity
// checking, no catalog bookkeeping. In the previous design each of those was
// reimplemented, slightly differently, in all six handlers — npm hand-rolled an
// atomic rename for its packument cache, apt hand-rolled a streaming download and a
// .meta sidecar, pypi hand-rolled its own index cache. Each variation was a place a
// bug could live alone.
type Ctx struct {
	W http.ResponseWriter
	R *http.Request

	// Project is the tenant this request belongs to.
	Project string
	// Eco is the ecosystem ID.
	Eco string
	// Params are the router's path captures, still percent-escaped.
	Params router.Params
	// Root is the URL prefix that was stripped to reach this adapter, e.g.
	// "/global/npm". Rewritten links must re-include it or a client will follow them
	// straight past the cache.
	Root string

	engine *engine.Engine
	cfg    *config.Snapshot
	desc   Descriptor
}

// NewCtx builds a request context. Called by the project router.
func NewCtx(
	w http.ResponseWriter, r *http.Request,
	project, root string, params router.Params,
	e *engine.Engine, cfg *config.Snapshot, desc Descriptor,
) *Ctx {
	return &Ctx{
		W: w, R: r,
		Project: project,
		Eco:     desc.ID,
		Params:  params,
		Root:    root,
		engine:  e,
		cfg:     cfg,
		desc:    desc,
	}
}

// Context returns the request context.
func (c *Ctx) Context() context.Context { return c.R.Context() }

// Descriptor returns the ecosystem's own descriptor, so a handler can consult its
// freshness policy or upstream defaults without a second copy of them.
func (c *Ctx) Descriptor() Descriptor { return c.desc }

// ---- serving --------------------------------------------------------------

// Serve runs the full cache pipeline for a resolution: hit, dedup, offline, or a
// single-flight fetch with progressive delivery.
func (c *Ctx) Serve(res engine.Resolution) error {
	if res.Project == "" {
		res.Project = c.Project
	}
	if res.Eco == "" {
		res.Eco = c.Eco
	}
	_, err := c.engine.Serve(c.W, c.R, res)
	return err
}

// Document fetches, caches and revalidates an upstream document — an index, a
// packument, a Release file. Concurrent callers collapse into one fetch.
func (c *Ctx) Document(spec engine.DocSpec) (*engine.Document, error) {
	if spec.Project == "" {
		spec.Project = c.Project
	}
	if spec.Eco == "" {
		spec.Eco = c.Eco
	}
	if spec.Credential == nil {
		spec.Credential = c.credentialForURL(spec.URL)
	}
	return c.engine.Document(c.Context(), spec)
}

// ServeBytes writes a generated response body.
func (c *Ctx) ServeBytes(status int, contentType string, body []byte) error {
	c.W.Header().Set("Content-Type", contentType)
	c.W.WriteHeader(status)
	if c.R.Method == http.MethodHead {
		return nil
	}
	_, err := c.W.Write(body)
	return err
}

// JSON writes a JSON response.
func (c *Ctx) JSON(status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("eco: marshal response: %w", err)
	}
	return c.ServeBytes(status, "application/json", body)
}

// Text writes a plain-text response, used for protocol-level errors that clients
// surface to a human.
func (c *Ctx) Text(status int, msg string) error {
	return c.ServeBytes(status, "text/plain; charset=utf-8", []byte(msg+"\n"))
}

// NotFound is the conventional miss.
func (c *Ctx) NotFound(msg string) error { return c.Text(http.StatusNotFound, msg) }

// ---- cache primitives -----------------------------------------------------

// Ref returns a mutable pointer this project holds.
func (c *Ctx) Ref(name string) (catalog.Ref, bool) {
	return c.engine.Ref(c.Project, c.Eco, name)
}

// SetRef records a mutable pointer.
func (c *Ctx) SetRef(name, target string, f Freshness) error {
	return c.engine.SetRef(catalog.Ref{
		RefKey: catalog.RefKey{Project: c.Project, Eco: c.Eco, Name: name},
		Target: target,
		TTL:    f.TTL,
	})
}

// ListRefs returns this project's refs, optionally prefix-filtered.
func (c *Ctx) ListRefs(prefix string) ([]catalog.Ref, error) {
	return c.engine.ListRefs(c.Project, c.Eco, prefix)
}

// DeleteRef removes a mutable pointer.
func (c *Ctx) DeleteRef(name string) error {
	return c.engine.DeleteRef(c.Project, c.Eco, name)
}

// PutBlob stores an uploaded body under a cache key, hashing it as it streams.
func (c *Ctx) PutBlob(key string, r *http.Request, o engine.PutOptions) (engine.PutResult, error) {
	return c.engine.Put(c.Project, c.Eco, key, r.Body, o)
}

// PutBytes stores generated content under a cache key.
func (c *Ctx) PutBytes(key string, body []byte, mediaType string) (blob.Digest, error) {
	return c.engine.PutBytes(c.Project, c.Eco, key, body, mediaType)
}

// Entry looks up a cache key without serving it.
func (c *Ctx) Entry(key string) (catalog.Entry, bool) {
	e, err := c.engine.Entry(c.Project, c.Eco, key)
	return e, err == nil
}

// BlobExists reports whether a digest is already in the shared CAS.
func (c *Ctx) BlobExists(d blob.Digest) bool { return c.engine.BlobExists(d) }

// ListEntries returns cached keys under a prefix. This is how the files role builds
// a directory listing: a catalog query rather than a readdir, so it always agrees
// with what the cache will actually serve.
func (c *Ctx) ListEntries(prefix string) ([]catalog.Entry, error) {
	return c.engine.ListEntries(c.Project, c.Eco, prefix)
}

// DeleteEntry removes a cache key. The blob stays for the garbage collector, which
// is the only thing that knows whether anything else still references it.
func (c *Ctx) DeleteEntry(key string) error {
	return c.engine.DeleteEntry(c.Project, c.Eco, key)
}

// RecordArtifact adds an inventory row.
func (c *Ctx) RecordArtifact(a catalog.Artifact) error {
	a.Project, a.Eco = c.Project, c.Eco
	return c.engine.RecordArtifact(a)
}

// DeleteArtifacts removes every version of one inventory name.
func (c *Ctx) DeleteArtifacts(name string) error {
	return c.engine.DeleteArtifacts(c.Project, c.Eco, name)
}

// DeleteArtifactVersion removes all architecture rows for one package version.
func (c *Ctx) DeleteArtifactVersion(name, version string) error {
	return c.engine.DeleteArtifactVersion(c.Project, c.Eco, name, version)
}

// ManagedDir returns the directory this ecosystem owns for this project. Only
// meaningful for StorageManagedDir; anything else is a programming error.
func (c *Ctx) ManagedDir() (string, error) {
	if c.desc.Storage != StorageManagedDir {
		return "", fmt.Errorf("eco: %s does not use managed-dir storage", c.Eco)
	}
	return c.engine.ManagedDir(c.Eco, c.Project)
}

// ---- environment ----------------------------------------------------------

// RegistryMirror is the upstream an unprefixed OCI repository resolves against, or ""
// when this instance is not acting as a registry mirror.
func (c *Ctx) RegistryMirror() string { return c.cfg.Server.RegistryMirror }

// Offline reports whether this project must serve from cache only.
func (c *Ctx) Offline() bool { return c.cfg.OfflineFor(c.Project) }

// ProxyHostAllowed applies the configured apt/apk relay allowlist. Entries are
// case-insensitive hostnames; a leading "*." admits subdomains but not the parent.
// Ports in either the request or an allowlist entry are ignored.
// A bare "*" is the explicit opt-in to relaying anywhere, so that deliberately keeping
// the historical behaviour is distinguishable from never having configured it. An empty
// list still relays — changing that would break every existing apt deployment on
// upgrade — but `pkgreg doctor` fails on it and `pkgreg serve` warns at startup.
func (c *Ctx) ProxyHostAllowed(host string) bool {
	allowed := c.cfg.Server.ProxyAllowlist
	if len(allowed) == 0 || c.cfg.Server.AllowsAnyProxyHost() {
		return true
	}
	host = canonicalHost(host)
	for _, entry := range allowed {
		entry = strings.TrimSpace(strings.ToLower(entry))
		if h, _, err := net.SplitHostPort(entry); err == nil {
			entry = h
		}
		entry = strings.TrimSuffix(entry, ".")
		if strings.HasPrefix(entry, "*.") {
			suffix := strings.TrimPrefix(entry, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

func canonicalHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// Upstreams returns the configured origins for this ecosystem and project, falling
// back to the descriptor's defaults.
func (c *Ctx) Upstreams() map[string]string {
	out := make(map[string]string, len(c.desc.DefaultUpstreams))
	for k, v := range c.desc.DefaultUpstreams {
		out[k] = v
	}
	if ecosystems := c.cfg.ProjectUpstreams[c.Project]; ecosystems != nil {
		for name, origin := range ecosystems[c.Eco] {
			out[name] = origin
		}
	}
	return out
}

// Upstream returns one named origin.
func (c *Ctx) Upstream(name string) (string, bool) {
	v, ok := c.Upstreams()[name]
	return v, ok
}

// SingleUpstream returns the sole origin for an ecosystem shaped that way.
func (c *Ctx) SingleUpstream() (string, bool) {
	for _, v := range c.Upstreams() {
		return v, true
	}
	return "", false
}

// ExternalBase is the absolute scheme://host prefix a client used to reach this
// ecosystem, including the stripped project/eco prefix.
//
// Rewritten links must be built from this. A packument whose tarball URLs point at
// registry.npmjs.org sends every client straight past the cache; one built from the
// internal listen address is unreachable from anywhere else.
func (c *Ctx) ExternalBase() string {
	scheme := "https"
	if c.R.TLS == nil {
		scheme = "http"
	}
	if c.cfg.Server.TrustProxy {
		if p := c.R.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = p
		}
	}
	host := c.R.Host
	if c.cfg.Server.TrustProxy {
		if h := c.R.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	return scheme + "://" + host + c.Root
}

// ---- errors ---------------------------------------------------------------

// WriteError renders an engine error as the right HTTP status.
//
// Centralised so every ecosystem answers an offline miss the same way, with a reason
// a human can act on rather than a bare 404.
func (c *Ctx) WriteError(err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, engine.ErrNotCached):
		msg := "not cached"
		if c.Offline() {
			msg = "not cached, and this project is offline — " +
				"import it from the online side, or bring the project online"
		}
		_ = c.Text(http.StatusNotFound, msg)
	case errors.Is(err, engine.ErrDigestMismatch), errors.Is(err, engine.ErrSizeMismatch):
		// Upstream served something that did not match what the index promised. That
		// is a bad gateway, not our failure, and nothing was cached.
		_ = c.Text(http.StatusBadGateway, "upstream content failed verification: "+err.Error())
	case errors.Is(err, engine.ErrUpstreamStatus):
		_ = c.Text(http.StatusBadGateway, err.Error())
	case errors.Is(err, catalog.ErrQuota):
		var quota *catalog.QuotaError
		if errors.As(err, &quota) {
			_ = c.JSON(http.StatusInsufficientStorage, map[string]any{
				"error": "quota_exceeded", "kind": quota.Kind,
				"usage": quota.Usage, "limit": quota.Limit, "attempt": quota.Attempt,
			})
			return
		}
		_ = c.Text(http.StatusInsufficientStorage, "project quota exceeded")
	case errors.Is(err, context.Canceled):
		// The client hung up. There is nobody left to tell.
	default:
		_ = c.Text(http.StatusInternalServerError, "cache error")
	}
}

// ---- helpers --------------------------------------------------------------

// UpstreamRequest builds an outbound request carrying this ecosystem's label.
func (c *Ctx) UpstreamRequest(url string, headers http.Header) upstream.Request {
	request := upstream.Request{URL: url, Headers: headers, Eco: c.Eco}
	request.Credential = c.credentialForURL(url)
	return request
}

func (c *Ctx) credentialForURL(url string) *upstream.Credential {
	origins := c.Upstreams()
	credentials := c.cfg.ProjectCredentials[c.Project][c.Eco]
	longest := 0
	var selected *upstream.Credential
	for name, origin := range origins {
		credential, found := credentials[name]
		if !found || len(origin) <= longest ||
			(url != origin && !strings.HasPrefix(url, strings.TrimRight(origin, "/")+"/")) {
			continue
		}
		longest = len(origin)
		selected = &upstream.Credential{
			Kind: credential.Kind, Username: credential.Username,
			Password: credential.Password, Token: credential.Token,
		}
	}
	return selected
}

// Exchange performs a small non-cacheable request through the shared outbound
// boundary. Artifact bodies should use Serve instead.
func (c *Ctx) Exchange(
	req upstream.Request, maxBytes int64,
) (status int, header http.Header, body []byte, err error) {
	if req.Eco == "" {
		req.Eco = c.Eco
	}
	return c.engine.Exchange(c.Context(), c.Project, req, maxBytes)
}

// JoinURL joins a base and a path without doubling or dropping the slash.
func JoinURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// ---- request-scoped plumbing ----------------------------------------------

// ctxKey is unexported so nothing outside this package can collide with it.
type ctxKey struct{}

// Bind attaches a Ctx to a request so a router.Handler — whose signature carries
// only (writer, request, params) — can recover it.
//
// The project router builds one Ctx per request with the tenant, engine and config
// already resolved; handlers complete it with their own writer and captures.
func Bind(r *http.Request, c *Ctx) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))
}

// CtxFrom recovers the bound Ctx and completes it for this handler.
//
// Panicking on an unbound request is deliberate: it can only happen if an ecosystem
// handler was mounted outside the project router, which is a wiring error that must
// surface in a test rather than as a nil dereference in production.
func CtxFrom(w http.ResponseWriter, r *http.Request, p router.Params) *Ctx {
	base, ok := r.Context().Value(ctxKey{}).(*Ctx)
	if !ok || base == nil {
		panic("eco: handler reached without a bound Ctx — mount it through the project router")
	}
	c := *base
	c.W, c.R, c.Params = w, r, p
	return &c
}
