package auth

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/brightskies/pkgreg/internal/control"
)

// BootstrapUser is the account `pkgreg init` provisions on a host that has none.
const BootstrapUser = "admin"

// bootstrapAlphabet omits the characters people misread when copying a password off a
// terminal onto another machine: 0/O, 1/l/I. 32 symbols, so each character carries
// exactly 5 bits and the arithmetic below needs no rejection sampling bias argument.
const bootstrapAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// bootstrapLength is 20 symbols over a 32-symbol alphabet: 100 bits of entropy, which
// is far past anything the login throttle would ever let an attacker reach.
const bootstrapLength = 20

// BootstrapResult reports what Bootstrap did, so the caller can print the one thing a
// generated credential requires: showing it exactly once.
type BootstrapResult struct {
	// Created is true only when this call provisioned a new account.
	Created bool
	// Username is the account that now exists, whether or not this call made it.
	Username string
	// Password is non-empty only when Created is true. It is never persisted in
	// recoverable form and never logged by this package.
	Password string
	// Existing counts accounts already stored, for a caller reporting the no-op case.
	Existing int
	// ConfiguredRoot is set when enforcement comes from auth.root_user instead of a
	// stored account, in which case this function deliberately does nothing.
	ConfiguredRoot string
}

// Bootstrap makes authentication enforced on a fresh host.
//
// Enforcement turns on the moment any account exists (see Accounts.Enabled), so
// provisioning one superuser here is what closes the default-open control plane. The
// previous design left `root_user` commented out in the generated configuration, which
// meant the documented install produced an instance where an anonymous caller could
// create projects, mint tokens and read the audit log — and `doctor` reported it
// healthy.
//
// A generated password rather than a fixed one because a well-known default is not a
// credential, and a prompt because an unattended `ExecStartPre=pkgreg init` has no
// terminal to prompt from. The secret is returned rather than written anywhere: the
// caller prints it once, and from then on only its scrypt digest exists.
//
// Idempotent by construction. It provisions only when the host has no accounts at all,
// so re-running init — which systemd does on every start — never rotates a working
// credential or resurrects a deliberately deleted one.
func Bootstrap(db *control.DB, configuredRoot string) (BootstrapResult, error) {
	// A configured root_user already enforces authentication and is managed entirely
	// through configuration; adding a stored account beside it would be a second
	// superuser nobody asked for.
	if strings.TrimSpace(configuredRoot) != "" {
		return BootstrapResult{ConfiguredRoot: configuredRoot, Username: configuredRoot}, nil
	}

	users, err := db.ListUsers()
	if err != nil {
		return BootstrapResult{}, err
	}
	if len(users) > 0 {
		return BootstrapResult{Existing: len(users), Username: users[0].Username}, nil
	}

	password, err := generatePassword()
	if err != nil {
		return BootstrapResult{}, err
	}
	salt, digest, err := Password{}.Hash(password)
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := db.CreateUser(control.User{
		Username: BootstrapUser, Role: "superuser", Salt: salt, Hash: digest,
	}); err != nil {
		return BootstrapResult{}, fmt.Errorf("provision the %s account: %w", BootstrapUser, err)
	}
	return BootstrapResult{Created: true, Username: BootstrapUser, Password: password}, nil
}

// generatePassword returns a grouped, unambiguous secret.
//
// Grouping is not decoration: this string gets read off one terminal and typed into
// another, or into a browser login form, and four short runs are markedly easier to
// transcribe correctly than one run of twenty.
func generatePassword() (string, error) {
	raw := make([]byte, bootstrapLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate a password: %w", err)
	}
	var out strings.Builder
	for i, b := range raw {
		if i > 0 && i%5 == 0 {
			out.WriteByte('-')
		}
		// len(bootstrapAlphabet) is 32 and 256 is a whole multiple of it, so the
		// modulo is uniform over the alphabet with no modulo bias.
		out.WriteByte(bootstrapAlphabet[int(b)%len(bootstrapAlphabet)])
	}
	return out.String(), nil
}
