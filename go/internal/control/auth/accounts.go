package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/brightskies/pkgreg/internal/control"
)

const minPassword = 8

var (
	accountName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	validRoles  = map[string]bool{"user": true, "admin": true, "superuser": true}
)

// Accounts owns account policy and password verification.
type Accounts struct {
	db           *control.DB
	password     Password
	rootUser     string
	rootPassword string
}

// NewAccounts builds the account service.
func NewAccounts(db *control.DB, rootUser, rootPassword string) *Accounts {
	return &Accounts{db: db, rootUser: rootUser, rootPassword: rootPassword}
}

// Enabled reports whether authentication enforcement is active.
func (a *Accounts) Enabled() bool {
	if a.rootUser != "" {
		return true
	}
	users, err := a.db.ListUsers()
	return err == nil && len(users) > 0
}

// Authenticate verifies a root or stored account.
//
// The guest name can never authenticate by password: a guest session is minted by an
// explicit, credential-free endpoint, and letting the same name through here would
// turn a reserved identity into a guessable login.
func (a *Accounts) Authenticate(username, password string) (control.User, bool) {
	username = strings.TrimSpace(username)
	if username == GuestUser {
		return control.User{}, false
	}
	if a.rootUser != "" && username == a.rootUser {
		ok := subtle.ConstantTimeCompare([]byte(password), []byte(a.rootPassword)) == 1
		return a.root(), ok
	}
	user, err := a.db.User(username)
	if err != nil || !a.password.Verify(password, user.Salt, user.Hash) {
		return control.User{}, false
	}
	return user, true
}

// Get resolves root or stored account.
func (a *Accounts) Get(username string) (control.User, bool) {
	if a.rootUser != "" && username == a.rootUser {
		return a.root(), true
	}
	user, err := a.db.User(username)
	return user, err == nil
}

// List returns only accounts actor is allowed to manage.
func (a *Accounts) List(actor control.User) ([]control.User, error) {
	users, err := a.db.ListUsers()
	if err != nil {
		return nil, err
	}
	switch actor.Role {
	case "superuser":
		if a.rootUser != "" {
			users = append(users, a.root())
		}
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		return users, nil
	case "admin":
		out := []control.User{actor}
		for _, user := range users {
			if user.ReportsTo == actor.Username {
				out = append(out, user)
			}
		}
		return out, nil
	default:
		return []control.User{actor}, nil
	}
}

// Create creates an account according to actor's role.
func (a *Accounts) Create(actor control.User, username, password, role, reportsTo string) (control.User, error) {
	if actor.Role != "admin" && actor.Role != "superuser" {
		return control.User{}, forbidden("only admins and superusers can create accounts")
	}
	if !validRoles[role] {
		return control.User{}, bad("role must be one of user, admin, superuser")
	}
	username, err := a.validateName(username)
	if err != nil {
		return control.User{}, err
	}
	if err := validatePassword(password); err != nil {
		return control.User{}, err
	}
	if _, exists := a.Get(username); exists {
		return control.User{}, conflict("account already exists: %s", username)
	}
	if actor.Role == "admin" {
		if role != "user" {
			return control.User{}, forbidden("admins can only create users")
		}
		reportsTo = actor.Username
	} else if role == "user" {
		if err := a.validateManager(reportsTo, username); err != nil {
			return control.User{}, err
		}
	} else {
		reportsTo = ""
	}
	salt, digest, err := a.password.Hash(password)
	if err != nil {
		return control.User{}, err
	}
	user := control.User{
		Username: username, Role: role, Salt: salt, Hash: digest, ReportsTo: reportsTo,
	}
	if err := a.db.CreateUser(user); err != nil {
		return control.User{}, err
	}
	return user, nil
}

// UserChanges is a partial account update.
type UserChanges struct {
	Role         *string
	ReportsTo    *string
	ReportsToSet bool
	Password     *string
}

// Update applies an authorized account change.
func (a *Accounts) Update(actor control.User, username string, change UserChanges) (control.User, error) {
	if username == a.rootUser && a.rootUser != "" {
		return control.User{}, forbidden("the root superuser is managed through configuration")
	}
	user, err := a.db.User(username)
	if errors.Is(err, control.ErrNotFound) {
		return control.User{}, notFound("no such account: %s", username)
	}
	if err != nil {
		return control.User{}, err
	}
	if change.Password != nil {
		allowed := actor.Role == "superuser" || actor.Username == username ||
			(actor.Role == "admin" && user.ReportsTo == actor.Username)
		if !allowed {
			return control.User{}, forbidden("you may only change your own password or your reports'")
		}
		if err := validatePassword(*change.Password); err != nil {
			return control.User{}, err
		}
		user.Salt, user.Hash, err = a.password.Hash(*change.Password)
		if err != nil {
			return control.User{}, err
		}
	}
	roleChange := change.Role != nil && *change.Role != user.Role
	if roleChange || change.ReportsToSet {
		if actor.Role != "superuser" {
			return control.User{}, forbidden("only a superuser can change roles or reporting")
		}
		if roleChange && actor.Username == username {
			return control.User{}, forbidden("you cannot change your own role")
		}
	}
	finalRole := user.Role
	if roleChange {
		if !validRoles[*change.Role] {
			return control.User{}, bad("role must be one of user, admin, superuser")
		}
		finalRole = *change.Role
	}
	finalReports := user.ReportsTo
	if change.ReportsToSet {
		finalReports = ""
		if change.ReportsTo != nil {
			finalReports = *change.ReportsTo
		}
	}
	if finalRole == "user" {
		if user.Role != "user" {
			hasReports, err := a.hasReports(username)
			if err != nil {
				return control.User{}, err
			}
			if hasReports {
				return control.User{}, forbidden("reassign this account's reports before demoting it")
			}
			owns, err := a.ownsProjects(username)
			if err != nil {
				return control.User{}, err
			}
			if owns {
				return control.User{}, forbidden("reassign this account's projects before demoting it")
			}
		}
		if err := a.validateManager(finalReports, username); err != nil {
			return control.User{}, err
		}
	} else {
		finalReports = ""
	}
	user.Role, user.ReportsTo = finalRole, finalReports
	if err := a.db.UpdateUser(user); err != nil {
		return control.User{}, err
	}
	return user, nil
}

// Delete removes an authorized account.
func (a *Accounts) Delete(actor control.User, username string) error {
	if username == a.rootUser && a.rootUser != "" {
		return forbidden("the root superuser is managed through configuration")
	}
	if username == actor.Username {
		return forbidden("you cannot delete your own account")
	}
	user, err := a.db.User(username)
	if errors.Is(err, control.ErrNotFound) {
		return notFound("no such account: %s", username)
	}
	if err != nil {
		return err
	}
	switch actor.Role {
	case "superuser":
		hasReports, err := a.hasReports(username)
		if err != nil {
			return err
		}
		if hasReports {
			return forbidden("reassign this account's reports before deleting it")
		}
	case "admin":
		if user.Role != "user" || user.ReportsTo != actor.Username {
			return forbidden("admins may only delete their own users")
		}
	default:
		return forbidden("users cannot delete accounts")
	}
	owns, err := a.ownsProjects(username)
	if err != nil {
		return err
	}
	if owns {
		return forbidden("reassign this account's projects before deleting it")
	}
	return a.db.DeleteUser(username)
}

// CanOperate reports owner-level access from ownership alone.
//
// Callers that know which project they are authorizing should use CanOperateOn, which
// also honours grants. This form stays because a caller that does not know the project
// cannot evaluate a per-project grant, and answering "no" there is the safe direction.
func (a *Accounts) CanOperate(actor control.User, owner string) bool {
	return actor.Role == "superuser" || (owner != "" && actor.Username == owner)
}

// CanView reports owner/report read access from ownership alone. See CanOperate.
func (a *Accounts) CanView(actor control.User, owner string) bool {
	return a.CanOperate(actor, owner) || (owner != "" && actor.ReportsTo == owner)
}

// CanOperateOn reports write access to a named project: ownership, superuser, or an
// explicit operate grant.
func (a *Accounts) CanOperateOn(actor control.User, project, owner string) bool {
	if a.CanOperate(actor, owner) {
		return true
	}
	return a.grantLevel(actor, project) == GrantOperate
}

// CanViewOn reports read access to a named project. An operate grant implies view, so
// only the absence of any grant denies here.
func (a *Accounts) CanViewOn(actor control.User, project, owner string) bool {
	if a.CanView(actor, owner) {
		return true
	}
	return a.grantLevel(actor, project) != ""
}

// grantLevel resolves what actor holds on project, or "" for nothing.
//
// A guest never resolves to a grant: the guest name is reserved and cannot be an
// account, but this is the authorization path, and relying on a validation rule
// enforced somewhere else is how a reserved name becomes a real one.
func (a *Accounts) grantLevel(actor control.User, project string) string {
	if project == "" || actor.Username == "" || IsGuest(actor) {
		return ""
	}
	level, err := a.db.GrantLevel(project, actor.Username)
	if err != nil {
		return ""
	}
	return level
}

// ImportLegacy imports existing users.json hashes unchanged when control.db has no
// stored users. It is safe and idempotent at every startup.
func (a *Accounts) ImportLegacy(path string) error {
	users, err := a.db.ListUsers()
	if err != nil || len(users) != 0 {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy struct {
		Users map[string]struct {
			Role      string `json:"role"`
			Salt      string `json:"salt"`
			Hash      string `json:"hash"`
			ReportsTo string `json:"reports_to"`
		} `json:"users"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return control.NewError(http.StatusBadRequest, "invalid_legacy_users",
			"read legacy users.json: %v", err)
	}
	for username, record := range legacy.Users {
		if _, err := a.validateName(username); err != nil || !validRoles[record.Role] {
			return bad("invalid account in legacy users.json: %s", username)
		}
		if err := a.db.CreateUser(control.User{
			Username: username, Role: record.Role, Salt: record.Salt,
			Hash: record.Hash, ReportsTo: record.ReportsTo,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *Accounts) validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 40 || !accountName.MatchString(name) {
		return "", bad("username must be 1–40 lowercase letters/digits separated by '.', '_' or '-'")
	}
	// Reserved because Guard.Actor resolves this name to the guest identity without
	// consulting the database. An account holding it would be unreachable at best,
	// and at worst would let someone with a real password inherit whatever the guest
	// path is allowed to do.
	if name == GuestUser {
		return "", bad("%q is reserved for read-only guest sessions", name)
	}
	if a.rootUser != "" && name == a.rootUser {
		return "", bad("%q is reserved", name)
	}
	return name, nil
}

func validatePassword(password string) error {
	if len(password) < minPassword {
		return bad("password must be at least %d characters", minPassword)
	}
	return nil
}

func (a *Accounts) validateManager(manager, subject string) error {
	if manager == "" {
		return bad("a user must report to an admin or superuser")
	}
	if manager == subject {
		return bad("an account cannot report to itself")
	}
	target, found := a.Get(manager)
	if !found || (target.Role != "admin" && target.Role != "superuser") {
		return bad("reports_to must be an existing admin or superuser: %s", manager)
	}
	return nil
}

func (a *Accounts) hasReports(username string) (bool, error) {
	users, err := a.db.ListUsers()
	if err != nil {
		return false, err
	}
	for _, user := range users {
		if user.ReportsTo == username {
			return true, nil
		}
	}
	return false, nil
}

func (a *Accounts) ownsProjects(username string) (bool, error) {
	projects, err := a.db.ListProjects()
	if err != nil {
		return false, err
	}
	for _, project := range projects {
		if project.Owner == username {
			return true, nil
		}
	}
	return false, nil
}

func (a *Accounts) root() control.User {
	return control.User{Username: a.rootUser, Role: "superuser", Builtin: true}
}

func bad(format string, args ...any) error {
	return control.NewError(http.StatusBadRequest, "invalid_request", format, args...)
}
func conflict(format string, args ...any) error {
	return control.NewError(http.StatusConflict, "conflict", format, args...)
}
func forbidden(format string, args ...any) error {
	return control.NewError(http.StatusForbidden, "forbidden", format, args...)
}
func notFound(format string, args ...any) error {
	return control.NewError(http.StatusNotFound, "not_found", format, args...)
}
