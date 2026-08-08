package auth

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
)

// SessionCookie is the control-plane session cookie.
const SessionCookie = "pkgreg_session"

// Guard resolves callers and enforces role/ownership policy.
type Guard struct {
	Accounts *Accounts
	Sessions *Sessions
	Config   *config.Store
}

// Actor returns the authenticated session account.
func (g *Guard) Actor(r *http.Request) (control.User, bool) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return control.User{}, false
	}
	username, found := g.Sessions.Resolve(cookie.Value)
	if !found {
		return control.User{}, false
	}
	// The guest session has no stored account to look up, and must not acquire one:
	// resolving it here rather than through Accounts is what keeps "guest" from
	// colliding with a real username. Accounts.validateName reserves the name too.
	if username == GuestUser {
		if !g.guestReadEnabled() {
			return control.User{}, false
		}
		return GuestActor(), true
	}
	return g.Accounts.Get(username)
}

// IsGuest reports whether this request carries a guest session.
func (g *Guard) IsGuest(r *http.Request) bool {
	actor, ok := g.Actor(r)
	return ok && IsGuest(actor)
}

// guestReadEnabled reads the live configuration, so revoking guest access takes effect
// on the next request rather than at the next restart — including for sessions already
// minted.
func (g *Guard) guestReadEnabled() bool {
	return g.Config.Current().Auth.GuestRead
}

// RequireUser requires a signed-in caller holding a real account.
//
// A guest is signed in and is deliberately refused here. Every privileged guard below
// funnels through this one, so that single rejection is what makes "read-only" hold
// for project creation, upstreams, tokens, accounts, maintenance and air-gap transfer
// without each of them having to remember.
func (g *Guard) RequireUser(r *http.Request) (control.User, error) {
	if actor, ok := g.Actor(r); ok {
		if IsGuest(actor) {
			return control.User{}, guestDenied("this action needs an account")
		}
		return actor, nil
	}
	return control.User{}, control.NewError(http.StatusUnauthorized,
		"authentication_required", "authentication required")
}

// RequireAuthed protects instance reads and honors anonymous safe reads.
func (g *Guard) RequireAuthed(r *http.Request) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	if actor, ok := g.Actor(r); ok {
		if IsGuest(actor) && !safeMethod(r.Method) {
			return control.User{}, guestDenied("this request would change something")
		}
		return actor, nil
	}
	if g.Config.Current().Auth.AnonRead && safeMethod(r.Method) {
		return control.User{}, nil
	}
	return g.RequireUser(r)
}

// RequireView requires project visibility.
//
// A guest is refused here rather than granted, because this signature knows only the
// project's owner and a guest's permission is defined by the project's *name*. Callers
// that know the name use RequireViewOf; everything else denies, which is the right way
// round for a check that cannot see what it would be authorizing.
func (g *Guard) RequireView(r *http.Request, owner string) (control.User, error) {
	return g.requireViewIn(r, "", owner)
}

// requireViewIn is RequireView with the project name when the caller knows it, so an
// explicit grant can be honoured. An empty project falls back to ownership alone.
func (g *Guard) requireViewIn(r *http.Request, project, owner string) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	if actor, ok := g.Actor(r); ok {
		if IsGuest(actor) {
			return control.User{}, guestDenied("this view needs an account")
		}
		if g.Accounts.CanViewOn(actor, project, owner) {
			return actor, nil
		}
		return control.User{}, forbidden("not authorized for this project")
	}
	if g.Config.Current().Auth.AnonRead && safeMethod(r.Method) {
		return control.User{}, nil
	}
	return g.RequireUser(r)
}

// RequireViewOf is RequireView for a caller that knows which project it is authorizing.
//
// The distinction exists for exactly one rule: a guest may read the global project and
// nothing else. Encoding that against the project name here, instead of against the
// owner string RequireView receives, is what keeps a guest from reaching a second
// tenant that happens to be unowned.
func (g *Guard) RequireViewOf(
	r *http.Request, project, owner string,
) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	if actor, ok := g.Actor(r); ok && IsGuest(actor) {
		switch {
		case !safeMethod(r.Method):
			return control.User{}, guestDenied("this request would change something")
		case project != config.GlobalProject:
			return control.User{}, guestDenied("project " + project + " is not visible")
		default:
			return actor, nil
		}
	}
	return g.requireViewIn(r, project, owner)
}

// RequireOperate requires ownership of, or an operate grant on, the named project.
//
// The project name is a parameter rather than something this reads back from the owner
// string because a grant is per project: two projects can share an owner and not share
// their access lists, and authorizing against the owner alone could not tell them apart.
func (g *Guard) RequireOperate(r *http.Request, project, owner string) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	actor, err := g.RequireUser(r)
	if err != nil {
		return control.User{}, err
	}
	if !g.Accounts.CanOperateOn(actor, project, owner) {
		return control.User{}, forbidden("not authorized to operate this project")
	}
	return actor, nil
}

// RequireCreate requires admin or superuser role.
func (g *Guard) RequireCreate(r *http.Request) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	actor, err := g.RequireUser(r)
	if err != nil {
		return control.User{}, err
	}
	if actor.Role != "admin" && actor.Role != "superuser" {
		return control.User{}, forbidden("only admins and superusers can create projects")
	}
	return actor, nil
}

// RequireSuperuser requires the highest role.
func (g *Guard) RequireSuperuser(r *http.Request) (control.User, error) {
	if !g.Accounts.Enabled() {
		return control.User{}, nil
	}
	actor, err := g.RequireUser(r)
	if err != nil {
		return control.User{}, err
	}
	if actor.Role != "superuser" {
		return control.User{}, forbidden("superuser only")
	}
	return actor, nil
}

// CheckOrigin rejects cross-origin browser mutations.
func (g *Guard) CheckOrigin(r *http.Request) error {
	if safeMethod(r.Method) || r.Header.Get("Origin") == "" {
		return nil
	}
	origin := authority(r.Header.Get("Origin"))
	if origin == "" {
		return forbidden("cross-origin request refused")
	}
	snapshot := g.Config.Current()
	if snapshot.Auth.PublicOrigin != "" {
		if origin != authority(snapshot.Auth.PublicOrigin) {
			return forbidden("cross-origin request refused")
		}
		return nil
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if snapshot.Server.TrustProxy {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
			scheme = forwarded
		}
	}
	if origin != authority(scheme+"://"+r.Host) {
		return forbidden("cross-origin request refused")
	}
	return nil
}

// ClientIP returns the peer IP, honoring one trusted proxy value when configured.
func (g *Guard) ClientIP(r *http.Request) string {
	if g.Config.Current().Server.TrustProxy {
		value := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if ip := net.ParseIP(value); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func authority(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(parsed.Scheme) + "://" +
		strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".") + ":" + port
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
