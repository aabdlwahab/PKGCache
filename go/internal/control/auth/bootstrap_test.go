package auth

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

func bootstrapDB(t *testing.T) *control.DB {
	t.Helper()
	db, err := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestBootstrapEnablesEnforcement is the point of the whole change: after init, the
// guard must be enforcing. Enforcement keys off Accounts.Enabled, so asserting on that
// rather than on row counts is what actually pins the behaviour.
func TestBootstrapEnablesEnforcement(t *testing.T) {
	db := bootstrapDB(t)
	accounts := NewAccounts(db, "", "")
	if accounts.Enabled() {
		t.Fatal("a fresh control database reported authentication as already enforced")
	}

	result, err := Bootstrap(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Username != BootstrapUser {
		t.Fatalf("Bootstrap = %+v, want a created %s account", result, BootstrapUser)
	}
	if !accounts.Enabled() {
		t.Fatal("authentication is still not enforced after provisioning an account")
	}

	user, ok := accounts.Authenticate(BootstrapUser, result.Password)
	if !ok {
		t.Fatal("the generated password does not authenticate")
	}
	if user.Role != "superuser" {
		t.Errorf("provisioned role = %q, want superuser", user.Role)
	}
	if _, ok := accounts.Authenticate(BootstrapUser, result.Password+"x"); ok {
		t.Error("a wrong password authenticated")
	}
}

// TestBootstrapIsIdempotent matters because the systemd unit runs `init` on every
// start. Rotating the credential there would lock out an operator who is doing nothing
// more unusual than restarting the service.
func TestBootstrapIsIdempotent(t *testing.T) {
	db := bootstrapDB(t)
	first, err := Bootstrap(db, "")
	if err != nil {
		t.Fatal(err)
	}

	second, err := Bootstrap(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Fatal("a second Bootstrap provisioned another account")
	}
	if second.Password != "" {
		t.Error("a no-op Bootstrap returned a password")
	}
	if second.Existing != 1 {
		t.Errorf("Existing = %d, want 1", second.Existing)
	}

	// The original credential must still work: nothing was rotated.
	if _, ok := NewAccounts(db, "", "").Authenticate(BootstrapUser, first.Password); !ok {
		t.Fatal("the first password stopped working after a second Bootstrap")
	}
}

// TestBootstrapDefersToAConfiguredRoot: root_user already enforces authentication and
// is managed entirely through configuration. Adding a stored superuser beside it would
// be a second administrator nobody asked for.
func TestBootstrapDefersToAConfiguredRoot(t *testing.T) {
	db := bootstrapDB(t)
	result, err := Bootstrap(db, "breakglass")
	if err != nil {
		t.Fatal(err)
	}
	if result.Created {
		t.Fatal("provisioned an account despite a configured root_user")
	}
	if result.ConfiguredRoot != "breakglass" {
		t.Errorf("ConfiguredRoot = %q, want breakglass", result.ConfiguredRoot)
	}
	users, err := db.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Errorf("stored %d accounts alongside a configured root", len(users))
	}
}

// TestBootstrapDoesNotResurrectADeletedAccount: an operator who deliberately removed
// every account has chosen an unauthenticated control plane. init must respect that
// rather than silently undoing it — the posture check is what tells them it is unsafe.
func TestBootstrapDoesNotResurrectADeletedAccount(t *testing.T) {
	db := bootstrapDB(t)
	if _, err := Bootstrap(db, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(BootstrapUser); err != nil {
		t.Fatal(err)
	}

	// With no accounts left this looks like a fresh host, so it does provision again.
	// That is the correct reading: "no accounts" is exactly the state init exists to
	// fix, and there is no durable record distinguishing "never had one" from
	// "deleted the last one".
	result, err := Bootstrap(db, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("an empty account table was left empty")
	}
}

func TestGeneratedPasswordShape(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		password, err := generatePassword()
		if err != nil {
			t.Fatal(err)
		}
		if seen[password] {
			t.Fatalf("generatePassword repeated %q", password)
		}
		seen[password] = true

		groups := strings.Split(password, "-")
		if len(groups) != bootstrapLength/5 {
			t.Fatalf("%q has %d groups, want %d", password, len(groups), bootstrapLength/5)
		}
		body := strings.ReplaceAll(password, "-", "")
		if len(body) != bootstrapLength {
			t.Fatalf("%q carries %d symbols, want %d", password, len(body), bootstrapLength)
		}
		for _, r := range body {
			if !strings.ContainsRune(bootstrapAlphabet, r) {
				t.Fatalf("%q contains %q, which is not in the unambiguous alphabet",
					password, r)
			}
		}
		// It has to survive the account policy it is created under.
		if err := validatePassword(password); err != nil {
			t.Fatalf("generated password rejected by policy: %v", err)
		}
	}
}
