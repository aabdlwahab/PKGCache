package app

import (
	"net"
	"net/http"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// siblingOnly is what another machine is told when it asks for anything but packages.
const siblingOnly = "this address serves other machines' caches: packages and the peer " +
	"protocol, read-only. The console and the control API answer on this machine only.\n"

// localGate serves this machine in full and every other machine as a sibling.
//
// A pkgcache has no certificate and no accounts, so its control plane must answer this
// machine alone. It used to be kept that way by refusing to bind anything but loopback,
// which also made the peer feature impossible across machines: the command the peer
// messages tell people to run, PKGCACHE_ADDR=0.0.0.0:41780, was refused at startup. The
// line is drawn per connection instead. Only a loopback peer address is this machine; a
// connection that arrives any other way — including from this machine's own network
// address — gets the sibling surface.
func (a *App) localGate(full http.Handler) http.Handler {
	sibling := a.SiblingHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromThisMachine(r) {
			full.ServeHTTP(w, r)
			return
		}
		sibling.ServeHTTP(w, r)
	})
}

func fromThisMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SiblingHandler is what another machine's cache may ask of this one: packages, read-only,
// the digest protocol, the list of projects `peer add` offers, and apt through the forward
// proxy for public host names. Nothing that writes, nothing that configures, and no tunnel.
//
// The proxy used to be refused outright, so a machine borrowing from this one sent its
// apt traffic around it — 487 MB straight to the mirrors in one field test's peer runs, however
// warm this cache was. What it guarded against was relaying anywhere for anyone, and the
// apt adapter now draws that line itself: a request marked as a sibling's may name a
// public DNS name only (eco.Ctx.OriginAllowed), exactly as a sibling's git or registry
// path may.
func (a *App) SiblingHandler() http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			writeText(w, http.StatusForbidden, siblingOnly)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeText(w, http.StatusMethodNotAllowed, siblingOnly)
			return
		}
		if router.IsProxyRequest(r) {
			a.Data.ServeProxy(w, r.WithContext(eco.FromSibling(r.Context())))
			return
		}
		path := r.URL.EscapedPath()
		switch {
		case path == "/healthz":
			a.healthz(w, r)
		case path == "/version":
			a.version(w, r)
		case path == "/api/v1/projects":
			a.API.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/peer/v1/"):
			a.Peer.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/api/") || consolePath(path) ||
			path == "/metrics" || path == "/readyz":
			writeText(w, http.StatusNotFound, siblingOnly)
		default:
			a.Data.ServeUnified(w, r.WithContext(eco.FromSibling(r.Context())))
		}
	})
	return a.noteActivity(securityHeaders(handler))
}
