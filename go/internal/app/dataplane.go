package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/router"
)

type internalRequestKey struct{}

// DataPlane resolves a request to one project and one ecosystem. It owns no
// protocol logic: after stripping the routing prefix it hands the request to the
// adapter's own mux with a fully populated eco.Ctx.
type DataPlane struct {
	config *config.Store
	engine *engine.Engine
	ecos   *eco.Registry
	muxes  map[string]*router.Mux
	tokens tokenVerifier
	limits *rateLimiter
}

type tokenVerifier interface {
	VerifyToken(project, eco, scope, presented string) bool
}

// NewDataPlane mounts every registered ecosystem once.
func NewDataPlane(
	cfg *config.Store,
	cacheEngine *engine.Engine,
	ecosystems *eco.Registry,
	tokens tokenVerifier,
) *DataPlane {
	d := &DataPlane{
		config: cfg, engine: cacheEngine, ecos: ecosystems,
		muxes: make(map[string]*router.Mux, ecosystems.Len()), tokens: tokens,
		limits: newRateLimiter(),
	}
	for _, ecosystem := range ecosystems.All() {
		mux := router.New()
		for _, route := range ecosystem.Routes() {
			if route.Admin {
				mux.Handle(route.Methods, route.Pattern, route.Handler)
			}
		}
		for _, route := range ecosystem.Routes() {
			if !route.Admin {
				mux.Handle(route.Methods, route.Pattern, route.Handler)
			}
		}
		d.muxes[ecosystem.Descriptor().ID] = mux
	}
	return d
}

// UnifiedHandler serves the origin-form data plane plus its small operational
// surface. Absolute-form proxy requests receive a diagnostic pointing at the proxy
// listener.
func (a *App) UnifiedHandler() http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if router.IsProxyRequest(r) {
			proxy := a.Config.Current().Server.ProxyAddr
			writeText(w, http.StatusBadRequest,
				"this is the unified listener, not the apt/apk forward proxy; use "+proxy)
			return
		}
		a.serveUnified(w, r)
	})
	return a.noteActivity(securityHeaders(handler))
}

// SinglePortHandler serves origin-form HTTP through the unified router and
// absolute-form HTTP through apt/apk.
//
// This is the whole-namespace-over-cleartext form, and it is correct only where the
// process terminates no TLS at all: a deliberately plain deployment on a trusted
// network, or one behind a reverse proxy that terminates TLS itself. Where this
// process does hold a certificate, the plaintext side of the split gets
// SinglePortPlainHandler instead.
func (a *App) SinglePortHandler() http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if router.IsProxyRequest(r) || r.Method == http.MethodConnect {
			a.Data.ServeProxy(w, r)
			return
		}
		a.serveUnified(w, r)
	})
	return a.noteActivity(securityHeaders(handler))
}

// SinglePortPlainHandler is the cleartext half of a single port that also speaks TLS.
//
// The first-byte mux splits TLS from plaintext before either is parsed as HTTP, and
// the plaintext half used to be handed the same namespace as the TLS half. That was
// wrong in a way no reader of the configuration could have predicted: the address is
// presented everywhere as an HTTPS origin, yet http://host:8443/console, /metrics,
// /tutorial and the entire /api/v1 surface answered on it, and POST /api/v1/login over
// cleartext returned a usable twelve-hour session cookie — necessarily without the
// Secure attribute, because a Secure cookie would never be sent back. Credentials and
// a reusable session on the wire, from a URL the product's own pages advertise.
//
// The plaintext half exists for exactly one reason: apt and apk cannot speak to a TLS
// proxy, so a forward proxy has to be cleartext by protocol. That is all it serves
// here. Origin-form requests are permanently redirected to the TLS side rather than
// refused, because the overwhelmingly common cause is a person typing http:// at a
// port that does speak https, and a redirect fixes that without a support ticket.
//
// 308 rather than 301/302: it preserves the method and body, so a client that got here
// with a POST does not have it silently rewritten to GET.
func (a *App) SinglePortPlainHandler() http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if router.IsProxyRequest(r) || r.Method == http.MethodConnect {
			a.Data.ServeProxy(w, r)
			return
		}
		// Liveness stays answerable in the clear. It carries no information beyond
		// "the process is up", and probes routinely cannot follow a redirect — making
		// them fail here would turn a security fix into an outage.
		if r.URL.EscapedPath() == "/healthz" {
			a.healthz(w, r)
			return
		}
		if r.Host == "" {
			writeText(w, http.StatusBadRequest,
				"this port serves TLS; reconnect with https://, or use it as an "+
					"apt/apk forward proxy")
			return
		}
		w.Header().Set("Location", "https://"+r.Host+r.URL.RequestURI())
		// Never cached: the redirect is a property of how this port is configured
		// today, and an operator who disables TLS should not fight browser state.
		w.Header().Set("Cache-Control", "no-store")
		writeText(w, http.StatusPermanentRedirect,
			"this port serves TLS; retry over https://")
	})
	return a.noteActivity(securityHeaders(handler))
}

// ProxyHandler is the explicit apt/apk forward-proxy listener.
func (a *App) ProxyHandler() http.Handler {
	return a.noteActivity(securityHeaders(http.HandlerFunc(a.Data.ServeProxy)))
}

func (a *App) serveUnified(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.EscapedPath(), "/peer/v1/") {
		a.Peer.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.EscapedPath(), "/api/") {
		a.API.ServeHTTP(w, r)
		return
	}
	if consolePath(r.URL.EscapedPath()) {
		a.Console.ServeHTTP(w, r)
		return
	}
	switch r.URL.EscapedPath() {
	case "/healthz":
		a.unifiedHealth(w, r)
		return
	case "/readyz":
		a.readyz(w, r)
		return
	case "/metrics":
		a.Metrics.Handler().ServeHTTP(w, r)
		return
	case "/version":
		a.version(w, r)
		return
	}
	a.Data.ServeUnified(w, r)
}

// consolePath decides what the single-port listener hands to the browser surface
// rather than to the package router. It has to name every console asset explicitly:
// anything missing here would be read as a package request against the "global"
// project and answered with a confusing 404 from the wrong subsystem.
func consolePath(name string) bool {
	switch name {
	case "/", "/landing", "/landing.html", "/tutorial", "/tutorial.html",
		"/console", "/theme.js", "/coords.js", "/tokens.css", "/landing.js", "/landing.css",
		"/tutorial.js", "/tutorial.css", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(name, "/console/") || strings.HasPrefix(name, "/fonts/")
}

func (a *App) unifiedHealth(w http.ResponseWriter, _ *http.Request) {
	roles := make([]string, 0)
	for _, descriptor := range a.Ecos.Descriptors() {
		if descriptor.Listener != eco.ListenerForwardProxy {
			roles = append(roles, descriptor.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "server": "unified", "roles": roles,
	})
}

// ServeUnified resolves /v2 OCI requests and /<project>/<eco>/ requests.
func (d *DataPlane) ServeUnified(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	snapshot := d.config.Current()
	knownProject := snapshot.HasProject

	if escaped == "/v2" || strings.HasPrefix(escaped, "/v2/") {
		if unknown := d.unknownOCIProject(escaped, knownProject); unknown != "" {
			writeText(w, http.StatusNotFound, unknownProjectMessage(unknown, "oci"))
			return
		}
		target, ok := router.ResolveOCI(escaped, "oci", knownProject)
		if !ok {
			writeText(w, http.StatusNotFound, "invalid OCI request path")
			return
		}
		if target.Path == "/v2" {
			target.Path = "/v2/"
		}
		d.dispatch(w, r, target)
		return
	}

	target, ok := router.ResolvePath(escaped, knownProject, func(id string) bool {
		_, found := d.ecos.Get(id)
		return found
	})
	if ok {
		d.dispatch(w, r, target)
		return
	}
	parts := plainSegments(escaped)
	if len(parts) >= 2 {
		if _, knownEco := d.ecos.Get(parts[1]); knownEco && !snapshot.HasProject(parts[0]) {
			writeText(w, http.StatusNotFound, unknownProjectMessage(parts[0], parts[1]))
			return
		}
	}
	writeText(w, http.StatusNotFound,
		"not found; use /<project>/<ecosystem>/... or /v2/... for OCI; "+
			"the default project is 'global'")
}

// ServeInternal runs a trusted in-process request through the same protocol adapter
// while bypassing client token checks. It is used by lockwarm; it is never mounted
// on a listener.
func (d *DataPlane) ServeInternal(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), internalRequestKey{}, true)
	d.ServeUnified(w, r.WithContext(ctx))
}

// ServeProxy resolves the project from the Basic proxy username and dispatches to
// apt/apk. An explicit unknown username is never allowed to fall back to global.
func (d *DataPlane) ServeProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		writeText(w, http.StatusMethodNotAllowed,
			"CONNECT is not supported; configure an HTTP apt/apk repository")
		return
	}
	snapshot := d.config.Current()
	if project, supplied := router.ProxyProject(r); supplied &&
		project != config.GlobalProject && !snapshot.HasProject(project) {
		writeText(w, http.StatusNotFound, unknownProjectMessage(project, "apt"))
		return
	}
	target := router.ResolveProxy(r, "apt", snapshot.HasProject)
	d.dispatch(w, r, target)
}

func (d *DataPlane) dispatch(w http.ResponseWriter, r *http.Request, target router.Target) {
	ecosystem, ok := d.ecos.Get(target.Eco)
	if !ok {
		writeText(w, http.StatusNotFound, "unknown ecosystem "+target.Eco)
		return
	}
	project := d.config.Current().Projects[target.Project]
	internal, _ := r.Context().Value(internalRequestKey{}).(bool)
	scope := "read"
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		scope = "write"
	}
	var authenticated *control.Token
	presented := requestToken(r)
	if verifier, ok := d.tokens.(interface {
		Authenticate(string, string, string, string) (control.Token, bool)
	}); ok && presented != "" {
		if token, valid := verifier.Authenticate(target.Project, target.Eco, scope, presented); valid {
			authenticated = &token
		}
	}
	if project.DataPlaneAuth == "token" && !internal {
		valid := authenticated != nil
		if _, supportsMetadata := d.tokens.(interface {
			Authenticate(string, string, string, string) (control.Token, bool)
		}); !supportsMetadata {
			valid = d.tokens != nil && d.tokens.VerifyToken(
				target.Project, target.Eco, scope, presented,
			)
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pkgreg"`)
			writeText(w, http.StatusUnauthorized, "valid project token required")
			return
		}
	}
	if !internal {
		rate, burst := project.RateLimit, project.RateBurst
		identity := target.Project + "\x00ip\x00" + clientAddress(r, d.config.Current().Server.TrustProxy)
		if authenticated != nil {
			identity = target.Project + "\x00token\x00" + authenticated.ID
			if authenticated.RateLimit > 0 {
				rate, burst = authenticated.RateLimit, authenticated.RateBurst
			}
		}
		if allowed, retry := d.limits.Allow(identity, rate, burst, time.Now()); !allowed {
			w.Header().Set("Retry-After", retryAfter(retry))
			writeText(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}
	mux := d.muxes[target.Eco]
	request := r
	if target.Eco != "apt" || !router.IsProxyRequest(r) {
		request = rewritePath(r, target.Path)
	}
	ctx := eco.NewCtx(w, request, target.Project, target.Root, router.Params{},
		d.engine, d.config.Current(), ecosystem.Descriptor())
	mux.ServeHTTP(w, eco.Bind(request, ctx))
}

func requestToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) > 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-Token"))
}

func (d *DataPlane) unknownOCIProject(path string, known router.KnownProject) string {
	parts := plainSegments(path)
	if len(parts) < 4 || parts[0] != "v2" {
		return ""
	}
	first, second := parts[1], parts[2]
	if first == config.GlobalProject || known(first) {
		return ""
	}
	ociRepo, ok := d.ecos.Get("oci")
	if !ok {
		return ""
	}
	aliases := ociRepo.Descriptor().DefaultUpstreams
	if _, firstIsAlias := aliases[first]; firstIsAlias {
		return ""
	}
	if _, secondIsAlias := aliases[second]; secondIsAlias {
		return first
	}
	return ""
}

func rewritePath(r *http.Request, escaped string) *http.Request {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		decoded = escaped
	}
	cloned := r.Clone(r.Context())
	clonedURL := *r.URL
	clonedURL.Path = decoded
	if decoded != escaped {
		clonedURL.RawPath = escaped
	} else {
		clonedURL.RawPath = ""
	}
	cloned.URL = &clonedURL
	return cloned
}

func plainSegments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func unknownProjectMessage(project, ecosystem string) string {
	return fmt.Sprintf("unknown project %q; create it first, or use /global/%s/...",
		project, ecosystem)
}

func writeText(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
