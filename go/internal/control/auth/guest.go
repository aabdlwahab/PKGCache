package auth

import (
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
)

// The guest identity: a real session with no account behind it.
//
// Modelled as a session rather than as anonymous access on purpose. The console
// already has session plumbing — a cookie, a TTL, a sign-out button, an actor on every
// audit row — and a guest that flows through all of it needs no parallel code path.
// Anonymous access would have meant every one of those places growing a second case,
// which is exactly how a "read-only" mode acquires a write.
//
// What stops a guest doing more than read is not this file. It is the route allowlist
// in internal/control/api: a guest may reach an explicit set of safe routes and is
// refused everything else, so an endpoint added tomorrow is closed to guests by
// default. The rules here are the second layer, which matters because the first is a
// list somebody could forget to think about.

const (
	// GuestUser is the reserved username a guest session carries.
	GuestUser = "guest"
	// RoleGuest is the reserved role. It is deliberately not in validRoles, so no
	// stored account can ever hold it.
	RoleGuest = "guest"
)

// GuestActor is the identity a guest session resolves to.
func GuestActor() control.User {
	return control.User{Username: GuestUser, Role: RoleGuest, Builtin: true}
}

// IsGuest reports whether a resolved actor is the guest identity.
func IsGuest(user control.User) bool {
	return user.Role == RoleGuest
}

// CreateGuest mints a guest session.
//
// It takes no credentials because there are none to take: the point is a view that
// costs a newcomer nothing. Enforcement of whether guest access is offered at all
// lives with the caller, which reads configuration; this function only mints.
func (s *Sessions) CreateGuest() (string, error) {
	return s.Create(GuestUser)
}

// guestDenied is the refusal a guest gets. It names the way forward, because the
// reader is a person who clicked "browse as guest" and then found an edge.
func guestDenied(what string) error {
	return control.NewError(http.StatusForbidden, "guest_read_only",
		"guest sessions are read-only and limited to the %s project: %s. "+
			"Sign in with an account to continue.", config.GlobalProject, what)
}
