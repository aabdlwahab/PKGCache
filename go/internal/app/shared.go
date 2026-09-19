package app

import (
	"net/http"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/router"
)

// SharedGate decides who may use the console on a socket other machines can reach.
//
// pkgcache has no accounts, and its control API answers anybody who can connect — which is
// safe on loopback and nowhere else. The shared socket puts one password in front of it.
// What checks the password lives with the daemon that stores it; which paths it guards is
// decided here, beside the router that knows what every path is.
type SharedGate interface {
	// Authorized reports whether the request carries a live session on this socket.
	Authorized(r *http.Request) bool
	// Login checks a password and starts a session. Logout ends one.
	Login(w http.ResponseWriter, r *http.Request)
	Logout(w http.ResponseWriter, r *http.Request)
	// Refuse answers a request that needs a session and has none.
	Refuse(w http.ResponseWriter, r *http.Request)
}

// SharedHandler serves the console to other machines, and nothing else.
//
// The console's own files are public — they are the same bytes in every copy of this
// program — so they load without a session, and the console's sign-in screen is what asks
// for the password. Everything with data behind it waits for one.
//
// Packages and the apt forward proxy are refused outright rather than put behind the
// password. The proxy relays wherever it is asked, and on this machine that includes
// addresses only this machine can reach; the package routes derive an upstream from the
// request for git and for registries a docker image names. Either would let the network
// reach through this machine to what is behind it, which is a different thing from
// letting it look at the cache.
func (a *App) SharedHandler(gate SharedGate) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if router.IsProxyRequest(r) || r.Method == http.MethodConnect {
			writeText(w, http.StatusForbidden, sharedConsoleOnly)
			return
		}
		path := r.URL.EscapedPath()
		switch {
		case path == "/healthz":
			a.healthz(w, r)
		case consolePath(path):
			a.Console.ServeHTTP(w, r)
		case path == "/api/v1/login" && r.Method == http.MethodPost:
			gate.Login(w, r)
		case path == "/api/v1/logout" && r.Method == http.MethodPost:
			gate.Logout(w, r)
		case !gate.Authorized(r):
			if strings.HasPrefix(path, "/api/") {
				gate.Refuse(w, r)
				return
			}
			writeText(w, http.StatusNotFound, sharedConsoleOnly)
		case path == "/api/v1/me":
			// Answered here rather than by the API, which knows nothing of this socket and
			// would report "open mode" — true of the loopback socket, and not what somebody
			// who just typed a password is looking at. "shared" is what gives them a way
			// to sign out again.
			writeJSON(w, http.StatusOK, map[string]any{
				"auth_enabled": false, "authenticated": true, "shared": true,
			})
		case strings.HasPrefix(path, "/api/"):
			a.API.ServeHTTP(w, r)
		case path == "/readyz":
			a.readyz(w, r)
		case path == "/metrics":
			a.Metrics.Handler().ServeHTTP(w, r)
		case path == "/version":
			a.version(w, r)
		default:
			writeText(w, http.StatusNotFound, sharedConsoleOnly)
		}
	})
	return a.noteActivity(securityHeaders(handler))
}

const sharedConsoleOnly = "this address serves the pkgcache console and nothing else; " +
	"packages are served only to the machine it runs on"
