package auth

import (
	"errors"
	"sort"

	"github.com/brightskies/pkgreg/internal/control"
)

// Grant levels. An operate grant implies view; there is no third level, because the
// only thing between "can read this project" and "can change it" would be a list of
// per-route exceptions, and a permission model people cannot hold in their heads is a
// permission model that gets handed a superuser account instead.
const (
	GrantView    = "view"
	GrantOperate = "operate"
)

var grantLevels = map[string]bool{GrantView: true, GrantOperate: true}

// Grants lists who else may reach a project.
//
// Readable by anyone who can operate the project, which includes an admin holding an
// operate grant: someone trusted to change the project should be able to see who else
// can, if only to know whose change they are looking at in the audit log.
func (a *Accounts) Grants(actor control.User, project, owner string) ([]control.Grant, error) {
	if !a.Enabled() {
		return nil, nil
	}
	if !a.CanOperateOn(actor, project, owner) {
		return nil, forbidden("not authorized to read this project's access list")
	}
	grants, err := a.db.ListGrants(project)
	if err != nil {
		return nil, err
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Username < grants[j].Username })
	return grants, nil
}

// SetGrant gives an account access to a project, or changes the level it already has.
//
// Only a superuser or the project's own owner may do this — deliberately not a holder
// of an operate grant. Letting a grantee re-grant turns one deliberate decision into a
// chain nobody is tracking, and the owner who made the first grant would have no way to
// see, let alone stop, the tenth.
func (a *Accounts) SetGrant(
	actor control.User, project, owner, username, level string,
) (control.Grant, error) {
	if err := a.canManageGrants(actor, owner); err != nil {
		return control.Grant{}, err
	}
	if !grantLevels[level] {
		return control.Grant{}, bad("level must be one of view, operate")
	}
	username, err := a.validateName(username)
	if err != nil {
		return control.Grant{}, err
	}
	subject, found := a.Get(username)
	if !found {
		return control.Grant{}, notFound("no such account: %s", username)
	}
	// Both of these would be grants that change nothing, and a permission list whose
	// rows are sometimes decorative is one no one can read for what it means.
	if username == owner {
		return control.Grant{}, bad("%s already owns this project", username)
	}
	if subject.Role == "superuser" {
		return control.Grant{}, bad("%s is a superuser and already has access to every project", username)
	}
	grant := control.Grant{
		Project: project, Username: username, Level: level, GrantedBy: actor.Username,
	}
	if err := a.db.PutGrant(grant); err != nil {
		return control.Grant{}, err
	}
	stored, err := a.db.ListGrants(project)
	if err != nil {
		return grant, nil // the write succeeded; the read-back is only for created_at
	}
	for _, row := range stored {
		if row.Username == username {
			return row, nil
		}
	}
	return grant, nil
}

// RevokeGrant removes an account's access to a project.
func (a *Accounts) RevokeGrant(actor control.User, project, owner, username string) error {
	if err := a.canManageGrants(actor, owner); err != nil {
		return err
	}
	err := a.db.DeleteGrant(project, username)
	if errors.Is(err, control.ErrNotFound) {
		return notFound("%s has no grant on this project", username)
	}
	return err
}

// GrantsFor reports every project one account reaches by grant.
//
// The console asks this for the signed-in actor so it can decide which controls to
// offer without probing each project with a request that would 403.
func (a *Accounts) GrantsFor(username string) (map[string]string, error) {
	if username == "" {
		return nil, nil
	}
	grants, err := a.db.GrantsFor(username)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(grants))
	for _, grant := range grants {
		out[grant.Project] = grant.Level
	}
	return out, nil
}

func (a *Accounts) canManageGrants(actor control.User, owner string) error {
	if actor.Role == "superuser" {
		return nil
	}
	if owner != "" && actor.Username == owner {
		return nil
	}
	return forbidden("only a superuser or this project's owner can change who may reach it")
}
