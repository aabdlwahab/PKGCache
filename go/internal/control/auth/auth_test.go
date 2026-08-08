package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/control"
)

func authDB(t *testing.T) *control.DB {
	t.Helper()
	db, err := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestExistingPythonScryptHashVerifiesUnchanged(t *testing.T) {
	const (
		salt   = "000102030405060708090a0b0c0d0e0f"
		digest = "25b376840366f4d3b0e21e414476676e3cd0e89af4430356a234ce0b65a021b5"
	)
	password := Password{}
	if !password.Verify("correct horse", salt, digest) {
		t.Fatal("Python hashlib.scrypt vector did not verify")
	}
	if password.Verify("wrong", salt, digest) {
		t.Fatal("wrong password verified")
	}
}

func TestLegacyUsersJSONImportsHashWithoutRehashing(t *testing.T) {
	db := authDB(t)
	accounts := NewAccounts(db, "", "")
	path := filepath.Join(t.TempDir(), "users.json")
	const digest = "25b376840366f4d3b0e21e414476676e3cd0e89af4430356a234ce0b65a021b5"
	data := `{"users":{"alice":{"role":"admin",` +
		`"salt":"000102030405060708090a0b0c0d0e0f","hash":"` + digest + `"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ImportLegacy(path); err != nil {
		t.Fatal(err)
	}
	user, ok := accounts.Authenticate("alice", "correct horse")
	if !ok || user.Role != "admin" {
		t.Fatalf("imported account = %+v, authenticated=%v", user, ok)
	}
	stored, err := db.User("alice")
	if err != nil || stored.Hash != digest {
		t.Fatalf("stored hash changed: %q, %v", stored.Hash, err)
	}
	// Startup re-runs the importer; it must remain a no-op.
	if err := accounts.ImportLegacy(path); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRoleReportingAndOwnershipPolicy(t *testing.T) {
	db := authDB(t)
	accounts := NewAccounts(db, "root", "rootpass12")
	root, ok := accounts.Authenticate("root", "rootpass12")
	if !ok || !root.Builtin {
		t.Fatal("configured root did not authenticate")
	}
	alice, err := accounts.Create(root, "alice", "adminpass1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := accounts.Create(alice, "bob", "bobsecret1", "user", "")
	if err != nil || bob.ReportsTo != "alice" {
		t.Fatalf("admin create = %+v, %v", bob, err)
	}
	if _, err := accounts.Create(bob, "eve", "evesecret1", "user", ""); err == nil {
		t.Fatal("user created an account")
	}
	if err := db.CreateProject(control.Project{
		Name: "team-a", Owner: "alice", DataPlaneAuth: "public",
	}); err != nil {
		t.Fatal(err)
	}
	role := "user"
	manager := "root"
	if _, err := accounts.Update(root, "alice", UserChanges{
		Role: &role, ReportsTo: &manager, ReportsToSet: true,
	}); err == nil {
		t.Fatal("project owner was demoted")
	}
	if !accounts.CanView(bob, "alice") || accounts.CanOperate(bob, "alice") {
		t.Fatal("report policy does not grant view-only access")
	}
}

func TestSessionsExpireThrottleAndRestartRevokes(t *testing.T) {
	sessions := NewSessions(time.Minute)
	token, err := sessions.Create("alice")
	if err != nil {
		t.Fatal(err)
	}
	if username, ok := sessions.Resolve(token); !ok || username != "alice" {
		t.Fatal("fresh session did not resolve")
	}
	for range 4 {
		sessions.RecordFailure("192.0.2.1")
	}
	if sessions.Blocked("192.0.2.1") {
		t.Fatal("locked before fifth failure")
	}
	sessions.RecordFailure("192.0.2.1")
	if !sessions.Blocked("192.0.2.1") {
		t.Fatal("fifth failure did not lock the IP")
	}
	restarted := NewSessions(time.Minute)
	if _, ok := restarted.Resolve(token); ok {
		t.Fatal("session survived process-local store restart")
	}
}

// A served lockout must restore the full allowance. Leaving the counter at the
// threshold meant one later typo re-locked the address immediately and indefinitely,
// which behind a shared NAT address is an outage rather than a defence.
func TestLockoutExpiryRestoresFullAllowance(t *testing.T) {
	sessions := NewSessions(time.Minute)
	clock := time.Now()
	sessions.now = func() time.Time { return clock }
	const ip = "192.0.2.7"

	for range 5 {
		sessions.RecordFailure(ip)
	}
	if !sessions.Blocked(ip) {
		t.Fatal("five failures did not lock the address")
	}

	clock = clock.Add(6 * time.Minute) // past the 5-minute lockout
	if sessions.Blocked(ip) {
		t.Fatal("the address is still locked after the lockout elapsed")
	}
	sessions.RecordFailure(ip)
	if sessions.Blocked(ip) {
		t.Fatal("a single failure after an expired lockout re-locked the address")
	}
	for range 3 {
		sessions.RecordFailure(ip)
	}
	if sessions.Blocked(ip) {
		t.Fatal("locked again after only four failures in the new window")
	}
	sessions.RecordFailure(ip)
	if !sessions.Blocked(ip) {
		t.Fatal("a full five failures in the new window did not lock the address")
	}
}

// Failures spread wider than the accumulation window are not one guessing burst.
func TestSlowFailuresDoNotAccumulateIntoALockout(t *testing.T) {
	sessions := NewSessions(time.Minute)
	clock := time.Now()
	sessions.now = func() time.Time { return clock }
	const ip = "192.0.2.8"
	for range 10 {
		sessions.RecordFailure(ip)
		if sessions.Blocked(ip) {
			t.Fatal("occasional failures hours apart produced a lockout")
		}
		clock = clock.Add(time.Hour)
	}
}

// The throttle table must not grow without bound as source addresses rotate.
func TestThrottleTableIsBounded(t *testing.T) {
	sessions := NewSessions(time.Minute)
	clock := time.Now()
	sessions.now = func() time.Time { return clock }
	for i := range maxThrottleEntries + 500 {
		sessions.RecordFailure(fmt.Sprintf("2001:db8::%x", i))
		if i%256 == 0 {
			clock = clock.Add(time.Second)
		}
	}
	sessions.mu.Lock()
	size := len(sessions.failures)
	sessions.mu.Unlock()
	if size > maxThrottleEntries {
		t.Fatalf("throttle table holds %d entries, cap is %d", size, maxThrottleEntries)
	}
}

func TestScopedTokenIssueVerifyLastUsedAndRevoke(t *testing.T) {
	db := authDB(t)
	tokens := NewTokens(db)
	record, secret, err := tokens.Issue("team-a", "files", "write", "ci", "root", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tokens.VerifyToken("team-a", "files", "write", secret) {
		t.Fatal("correct scope did not verify")
	}
	if tokens.VerifyToken("team-b", "files", "write", secret) ||
		tokens.VerifyToken("team-a", "npm", "write", secret) {
		t.Fatal("token escaped its exact project/ecosystem")
	}
	// A write credential must also be able to read. The data plane demands the "read"
	// scope for every GET, so without this a CI token could publish to a token-gated
	// project but not install from it.
	if !tokens.VerifyToken("team-a", "files", "read", secret) {
		t.Fatal("a write token could not read")
	}
	if tokens.VerifyToken("team-a", "files", "admin", secret) {
		t.Fatal("a write token was accepted for admin")
	}
	stored, err := tokens.Get(record.ID)
	if err != nil || stored.LastUsed == nil {
		t.Fatalf("last_used was not recorded: %+v, %v", stored, err)
	}
	if err := tokens.Revoke(record.ID); err != nil {
		t.Fatal(err)
	}
	if tokens.VerifyToken("team-a", "files", "write", secret) {
		t.Fatal("revoked token still verifies")
	}
}

func TestTokenScopeLadderAndPeerIsolation(t *testing.T) {
	db := authDB(t)
	tokens := NewTokens(db)
	issue := func(scope string) string {
		t.Helper()
		_, secret, err := tokens.Issue("team-a", "files", scope, scope, "root", 0)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	read, write, admin, peer := issue("read"), issue("write"), issue("admin"), issue("peer")

	for _, c := range []struct {
		held, required string
		want           bool
	}{
		{"read", "read", true},
		{"read", "write", false},
		{"read", "admin", false},
		{"write", "read", true},
		{"write", "write", true},
		{"write", "admin", false},
		{"admin", "read", true},
		{"admin", "write", true},
		{"admin", "admin", true},
		// peer is orthogonal: it neither grants user access nor is granted by it.
		{"peer", "peer", true},
		{"peer", "read", false},
		{"peer", "write", false},
		{"admin", "peer", false},
		{"read", "peer", false},
	} {
		secret := map[string]string{
			"read": read, "write": write, "admin": admin, "peer": peer,
		}[c.held]
		if got := tokens.VerifyToken("team-a", "files", c.required, secret); got != c.want {
			t.Errorf("%s token for %s scope = %v, want %v", c.held, c.required, got, c.want)
		}
		if got := tokens.HasToken("team-a", "files", c.required); c.want && !got {
			t.Errorf("HasToken(%s) = false while a %s token exists", c.required, c.held)
		}
	}
}
