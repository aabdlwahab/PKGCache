package api

import (
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/auth"
)

// Guest confinement.
//
// The rule is default-deny, keyed on the route pattern rather than on anything a
// handler decides. Every route is closed to a guest session unless it appears in
// guestRoutes below, so an endpoint added next month is refused without anyone
// remembering that guests exist. The alternative — a check inside each handler — is
// the arrangement where one missing line silently publishes the audit log.
//
// Two rules apply on top of membership:
//
//  1. Safe methods only. GET, HEAD and OPTIONS. The single exception is logout, which
//     mutates only the caller's own session and must stay reachable or a guest cannot
//     leave.
//  2. The global project only. Any route naming a project must name that one, and any
//     query parameter selecting a project is rewritten to it rather than rejected, so
//     a guest hitting an instance-wide statistics endpoint gets the global slice
//     instead of an error or, far worse, everyone's totals.

// guestRoutes is the allowlist, keyed exactly as the routes are registered.
//
// Scope: what a newcomer needs to see what the cache holds and point their tools at
// it — the Overview, Cache and Connect views. Deliberately absent: upstreams and
// snapshots (Sources and Transfer), which expose internal mirror hostnames and
// air-gap cadence; and tokens, accounts and the audit log, which are operational
// secrets.
//
// The live event stream is included, but only because it is filtered per subscriber:
// see eventFilter in sse.go, which scopes a guest to the global project and withholds
// audit and job frames. Listing it here without that filter would publish every
// tenant's activity to anyone who clicked "browse as guest".
var guestRoutes = map[string]bool{
	"GET /api/v1/me":                           true,
	"POST /api/v1/logout":                      true,
	"GET /api/v1/coordinates":                  true,
	"GET /api/v1/ecosystems":                   true,
	"GET /api/v1/downloads":                    true,
	"GET /api/v1/events":                       true,
	"GET /api/v1/downloads/{name}":             true,
	"GET /api/v1/projects":                     true,
	"GET /api/v1/projects/{project}":           true,
	"GET /api/v1/projects/{project}/artifacts": true,
	"GET /api/v1/projects/{project}/endpoints": true,
	"GET /api/v1/projects/{project}/setup.sh":  true,
	"GET /api/v1/projects/{project}/setup.ps1": true,
	"GET /api/v1/stats":                        true,
	"GET /api/v1/stats/series":                 true,
	"GET /api/v1/stats/storage":                true,
	"GET /api/v1/stats/ages":                   true,
}

// GuestRoutes exposes the allowlist for tests that check it against the real route
// table. A renamed route would otherwise fall out of the list silently and remove a
// guest's access without anyone noticing until someone clicked.
func GuestRoutes() map[string]bool {
	out := make(map[string]bool, len(guestRoutes))
	for pattern, allowed := range guestRoutes {
		out[pattern] = allowed
	}
	return out
}

// guestGate authorizes one request from a guest session against the allowlist, and
// confines it to the global project. It returns an error to refuse, and otherwise may
// have rewritten the request's project selection.
//
// Called from API.route, which is the one place every control route passes through.
func (a *API) guestGate(pattern string, r *http.Request) (*http.Request, error) {
	if !a.guard.IsGuest(r) {
		return r, nil
	}
	if !guestRoutes[pattern] {
		return nil, guestRefusal("this view needs an account")
	}
	// Logout is the one allowed mutation, and it changes nothing but the caller's own
	// session. Everything else on the allowlist is a read.
	if r.Method != http.MethodPost && !safeMethod(r.Method) {
		return nil, guestRefusal("this request would change something")
	}

	// A path-addressed project must be the global one. Rewriting would silently show
	// the wrong data, so this refuses.
	if named := r.PathValue("project"); named != "" && named != config.GlobalProject {
		return nil, guestRefusal("project " + named + " is not visible")
	}

	// A query-selected project is rewritten instead. These are the statistics
	// endpoints, where an absent project means "the whole instance" — the one case
	// where doing nothing would leak every tenant's totals to a guest.
	if query := r.URL.Query(); query.Get("project") != config.GlobalProject {
		clone := *r
		url := *r.URL
		query.Set("project", config.GlobalProject)
		url.RawQuery = query.Encode()
		clone.URL = &url
		return &clone, nil
	}
	return r, nil
}

func guestRefusal(what string) error {
	return control.NewError(http.StatusForbidden, "guest_read_only",
		"guest sessions are read-only and limited to the %s project: %s. "+
			"Sign in with an account to continue.", config.GlobalProject, what)
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead ||
		method == http.MethodOptions
}

// guestAvailable reports whether this instance offers sign-in-free browsing.
//
// False when authentication is not enforced at all: with no accounts there is nothing
// to be a guest of, the console is already fully open, and offering a "browse as
// guest" button there would imply a restriction that does not exist.
func (a *API) guestAvailable() bool {
	return a.Accounts.Enabled() && a.Config.Current().Auth.GuestRead
}

// loginGuest mints a credential-free, read-only session.
func (a *API) loginGuest(w http.ResponseWriter, r *http.Request) error {
	snapshot := a.Config.Current()
	if err := refuseCleartextLogin(r, snapshot); err != nil {
		return err
	}
	if !a.guestAvailable() {
		if !a.Accounts.Enabled() {
			return control.NewError(http.StatusConflict, "guest_unnecessary",
				"this instance has no accounts, so the console is already open; "+
					"run `pkgreg init` to enable authentication")
		}
		return control.NewError(http.StatusForbidden, "guest_read_disabled",
			"guest browsing is disabled on this instance (auth.guest_read)")
	}
	token, err := a.Sessions.CreateGuest()
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r, snapshot),
		MaxAge: int(snapshot.Auth.SessionTTL.Seconds()),
	})
	// Audited like any other sign-in. A read-only visitor is still someone who was
	// here, and an operator reading the log should see that rather than a gap.
	a.audit(r, auth.GuestActor(), "session.guest", auth.GuestUser, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"username": auth.GuestUser, "role": auth.RoleGuest,
		"guest": true, "readonly": true, "project": config.GlobalProject,
	})
	return nil
}
