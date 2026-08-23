package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/buildinfo"
	consoleweb "github.com/aabdlwahab/PKGCache/internal/web"
)

// AdminHandler serves the console, control API and operational surface.
func (a *App) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", a.Metrics.Handler())
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /version", a.version)
	mux.Handle("/api/", a.API)
	mux.Handle("/", a.Console)
	return a.noteActivity(securityHeaders(mux))
}

// healthz is liveness: the process is up and answering. It must not depend on any
// subsystem, or a transient storage problem would get the process killed rather than
// reported.
func (a *App) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// readyz is readiness: every dependency this process needs is actually usable.
// Distinct from liveness on purpose — a full disk should stop traffic, not restarts.
func (a *App) readyz(w http.ResponseWriter, _ *http.Request) {
	checks := map[string]string{}
	ready := true

	if err := a.Catalog.Ping(); err != nil {
		checks["catalog"] = err.Error()
		ready = false
	} else {
		checks["catalog"] = "ok"
	}
	if err := a.Control.Ping(); err != nil {
		checks["control"] = err.Error()
		ready = false
	} else {
		checks["control"] = "ok"
	}

	// Prove the store is writable, not merely present: a read-only filesystem is the
	// failure that otherwise shows up as every download mysteriously failing.
	if w, err := a.Blobs.Create(); err != nil {
		checks["blobs"] = err.Error()
		ready = false
	} else {
		_ = w.Abort()
		checks["blobs"] = "ok"
	}

	if a.listenersExpected.Load() {
		if a.listenersReady.Load() {
			checks["listeners"] = "ok"
		} else {
			checks["listeners"] = "not accepting requests"
			ready = false
		}
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

func (a *App) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

// securityHeaders applies the same policy the retired nginx config carried, so
// removing nginx does not quietly remove its hardening.
// noteActivity reports each request to App.Activity, when one is set.
//
// Installed at handler-construction time rather than checked per request, so a server
// — which never sets Activity — runs exactly the handler chain it ran before.
func (a *App) noteActivity(next http.Handler) http.Handler {
	if a.Activity == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isProbePath(r.URL.EscapedPath()) {
			a.Activity()
		}
		next.ServeHTTP(w, r)
	})
}

// isProbePath names the requests that ask whether this process is alive rather than
// asking it for anything.
//
// They are excluded from the activity signal deliberately. Counting them would mean
// that anything watching the cache keeps it running: a monitoring loop scraping
// /metrics, or pkgcache's own liveness check, would hold an idle daemon open forever
// and the idle timeout would silently never fire. Observing a thing must not be
// indistinguishable from using it.
func isProbePath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/version", "/metrics":
		return true
	}
	return false
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", consoleweb.ContentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// NewServer builds an http.Server with the timeouts this workload needs.
//
// There is deliberately no WriteTimeout or global ReadTimeout: a 2.5 GB wheel over a
// slow link legitimately takes many minutes, and a whole-request deadline would abort
// exactly the transfers the cache exists to make cheap. Slow-header attacks are bound
// by ReadHeaderTimeout instead, and stalled bodies by the per-request context.
func NewServer(addr string, h http.Handler, readHeaderTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       120 * time.Second,
	}
}
