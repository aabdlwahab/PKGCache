package eco

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
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
	if spec.Fallbacks == nil {
		spec.Fallbacks = c.fallbacksFor(spec.URL)
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
	for name, origin := range c.desc.DefaultUpstreams {
		out[name] = origin
	}
	for name, chain := range c.chains() {
		if len(chain) > 0 {
			// The head of the chain: the origin this index is normally answered from.
			// Callers that need the rest — the engine, when the head is unreachable —
			// ask for UpstreamChain instead.
			out[name] = chain[0].URL
		}
	}
	return out
}

// UpstreamChain returns every origin configured for one index name, in the order they
// should be tried. Empty when the name is unknown.
//
// A chain of one is the shape every configuration had before chains existed, and is
// what a descriptor's DefaultUpstreams still produces.
func (c *Ctx) UpstreamChain(name string) []config.Endpoint {
	if chain, ok := c.chains()[name]; ok && len(chain) > 0 {
		return chain
	}
	if origin, ok := c.desc.DefaultUpstreams[name]; ok {
		return []config.Endpoint{{URL: origin}}
	}
	return nil
}

// chains is this project's configured overrides for this ecosystem.
func (c *Ctx) chains() map[string][]config.Endpoint {
	if ecosystems := c.cfg.ProjectUpstreams[c.Project]; ecosystems != nil {
		return ecosystems[c.Eco]
	}
	return nil
}

// Upstream returns one named origin.
func (c *Ctx) Upstream(name string) (string, bool) {
	v, ok := c.Upstreams()[name]
	return v, ok
}

// SingleUpstream returns the sole origin for an ecosystem shaped that way.
//
// Sorted rather than "whatever the map yields first". A map iteration is deliberately
// randomised in Go, so with more than one name configured this used to return a
// different origin between requests — an ecosystem declaring UpstreamSingle should
// only ever have one, but a configuration that grew a second one would have failed
// intermittently rather than visibly.
func (c *Ctx) SingleUpstream() (string, bool) {
	origins := c.Upstreams()
	names := make([]string, 0, len(origins))
	for name := range origins {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return origins[names[0]], true
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
// UpstreamRequest builds an outbound request for a URL an ecosystem has composed.
//
// The fallbacks are derived rather than declared, which is what keeps the six adapters
// out of this entirely. Every one of them builds a URL as origin + an ecosystem-specific
// path, so an alternate origin for the same path is the same URL with its prefix
// swapped: the path is the ecosystem's business and the origin is not. An adapter that
// gained chain awareness would be six copies of this logic that could disagree.
func (c *Ctx) UpstreamRequest(url string, headers http.Header) upstream.Request {
	request := upstream.Request{URL: url, Headers: headers, Eco: c.Eco}
	request.Credential = c.credentialForURL(url)
	request.Fallbacks = c.fallbacksFor(url)
	return request
}

// fallbacksFor finds the chain a URL was composed from and re-composes the same path
// against every later origin in it.
func (c *Ctx) fallbacksFor(url string) []upstream.Fallback {
	chain, head := c.chainForURL(url)
	if len(chain) < 2 {
		return nil
	}
	suffix := strings.TrimPrefix(url, head)
	out := make([]upstream.Fallback, 0, len(chain)-1)
	for _, endpoint := range chain[1:] {
		if endpoint.URL == head {
			continue
		}
		fallback := upstream.Fallback{URL: strings.TrimRight(endpoint.URL, "/") + suffix}
		if !endpoint.Anonymous() {
			fallback.Credential = &upstream.Credential{
				Kind: endpoint.Credential.Kind, Username: endpoint.Credential.Username,
				Password: endpoint.Credential.Password, Token: endpoint.Credential.Token,
			}
		}
		out = append(out, fallback)
	}
	return out
}

// chainForURL identifies which configured chain a composed URL belongs to, returning
// the chain and the exact origin prefix that matched.
//
// Longest prefix wins, for the same reason it does when choosing a credential: an origin
// that includes a path must beat the bare host it sits on.
func (c *Ctx) chainForURL(url string) (chain []config.Endpoint, head string) {
	longest := 0
	for _, candidate := range c.chains() {
		if len(candidate) == 0 {
			continue
		}
		origin := strings.TrimRight(candidate[0].URL, "/")
		if len(origin) <= longest {
			continue
		}
		if url != origin && !strings.HasPrefix(url, origin+"/") {
			continue
		}
		longest, chain, head = len(origin), candidate, origin
	}
	return chain, head
}

// credentialForURL finds the credential belonging to the origin a URL came from.
//
// Longest-prefix wins, so a credential configured for a specific path on a host does
// not lose to one configured for the host itself. Every endpoint in every chain is a
// candidate: a chain's second entry is a different origin with its own credential, and
// looking only at the head would send the team cache's token to the public registry
// behind it.
func (c *Ctx) credentialForURL(url string) *upstream.Credential {
	longest := 0
	var selected *upstream.Credential
	consider := func(endpoint config.Endpoint) {
		origin := endpoint.URL
		if endpoint.Anonymous() || len(origin) <= longest {
			return
		}
		if url != origin && !strings.HasPrefix(url, strings.TrimRight(origin, "/")+"/") {
			return
		}
		longest = len(origin)
		selected = &upstream.Credential{
			Kind: endpoint.Credential.Kind, Username: endpoint.Credential.Username,
			Password: endpoint.Credential.Password, Token: endpoint.Credential.Token,
		}
	}
	for _, chain := range c.chains() {
		for _, endpoint := range chain {
			consider(endpoint)
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
