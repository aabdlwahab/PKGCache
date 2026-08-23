package auth

import (
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// grantFixture builds the shape the feature exists for: two admins who own nothing of
// each other's, one plain user reporting to the first admin, and one project.
func grantFixture(t *testing.T) (*Accounts, control.User, control.User, control.User, control.User) {
	t.Helper()
	db := authDB(t)
	accounts := NewAccounts(db, "root", "rootpass12")
	root, ok := accounts.Authenticate("root", "rootpass12")
	if !ok {
		t.Fatal("configured root did not authenticate")
	}
	alice, err := accounts.Create(root, "alice", "alicepass1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := accounts.Create(root, "bob", "bobsecret1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := accounts.Create(alice, "carol", "carolpass1", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(control.Project{
		Name: "team-a", Owner: "alice", DataPlaneAuth: "public",
	}); err != nil {
		t.Fatal(err)
	}
	return accounts, root, alice, bob, carol
}

func TestOwnerGrantsAnotherAdminOperateAccess(t *testing.T) {
	accounts, _, alice, bob, _ := grantFixture(t)

	if accounts.CanViewOn(bob, "team-a", alice.Username) {
		t.Fatal("a second admin reached the project before any grant")
	}
	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if !accounts.CanOperateOn(bob, "team-a", alice.Username) {
		t.Fatal("granted admin cannot operate the project")
	}
	if !accounts.CanViewOn(bob, "team-a", alice.Username) {
		t.Fatal("operate did not imply view")
	}
	// The grant is scoped to the one project it names.
	if accounts.CanOperateOn(bob, "team-b", alice.Username) {
		t.Fatal("a grant on one project authorized another")
	}
}

func TestViewGrantDoesNotCarryWriteAccess(t *testing.T) {
	accounts, _, alice, bob, _ := grantFixture(t)

	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantView); err != nil {
		t.Fatal(err)
	}
	if !accounts.CanViewOn(bob, "team-a", alice.Username) {
		t.Fatal("view grant did not grant view")
	}
	if accounts.CanOperateOn(bob, "team-a", alice.Username) {
		t.Fatal("view grant granted write access")
	}
}

// A grantee working on a project must not be able to widen the circle: that would turn
// one owner's decision into a chain they cannot see or stop.
func TestGranteeCannotRegrantOrRevoke(t *testing.T) {
	accounts, _, alice, bob, carol := grantFixture(t)

	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SetGrant(bob, "team-a", alice.Username, "carol", GrantOperate); err == nil {
		t.Fatal("an operate grantee granted access to someone else")
	}
	if err := accounts.RevokeGrant(bob, "team-a", alice.Username, "bob"); err == nil {
		t.Fatal("an operate grantee changed the access list")
	}
	if _, err := accounts.SetGrant(carol, "team-a", alice.Username, "carol", GrantOperate); err == nil {
		t.Fatal("an unrelated user granted themselves access")
	}
}

func TestSuperuserGrantsAndRevokesAnyProject(t *testing.T) {
	accounts, root, alice, bob, _ := grantFixture(t)

	if _, err := accounts.SetGrant(root, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if err := accounts.RevokeGrant(root, "team-a", alice.Username, "bob"); err != nil {
		t.Fatal(err)
	}
	if accounts.CanViewOn(bob, "team-a", alice.Username) {
		t.Fatal("revoked admin still reaches the project")
	}
	if err := accounts.RevokeGrant(root, "team-a", alice.Username, "bob"); err == nil {
		t.Fatal("revoking a grant that does not exist reported success")
	}
}

func TestGrantRejectsMeaninglessAndUnknownSubjects(t *testing.T) {
	accounts, root, alice, _, _ := grantFixture(t)

	for name, run := range map[string]func() error{
		"unknown account": func() error {
			_, err := accounts.SetGrant(alice, "team-a", alice.Username, "nobody", GrantOperate)
			return err
		},
		"the owner": func() error {
			_, err := accounts.SetGrant(alice, "team-a", alice.Username, "alice", GrantOperate)
			return err
		},
		"a superuser": func() error {
			_, err := accounts.SetGrant(root, "team-a", alice.Username, "root", GrantOperate)
			return err
		},
		"an invalid level": func() error {
			_, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", "admin")
			return err
		},
	} {
		if err := run(); err == nil {
			t.Errorf("granting %s was accepted", name)
		}
	}
}

// Re-granting is how the level is changed, so it must overwrite rather than conflict.
func TestRegrantChangesTheLevelInPlace(t *testing.T) {
	accounts, _, alice, _, _ := grantFixture(t)

	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantView); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	grants, err := accounts.Grants(alice, "team-a", alice.Username)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Level != GrantOperate || grants[0].GrantedBy != "alice" {
		t.Fatalf("grants after re-grant = %+v", grants)
	}
}

// A deleted account must not leave a row behind that a same-named account would inherit.
func TestDeletingAnAccountRemovesItsGrants(t *testing.T) {
	accounts, root, alice, _, _ := grantFixture(t)

	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Delete(root, "bob"); err != nil {
		t.Fatal(err)
	}
	remade, err := accounts.Create(root, "bob", "bobsecret2", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if accounts.CanViewOn(remade, "team-a", alice.Username) {
		t.Fatal("a recreated account inherited the deleted account's grant")
	}
}

// The access list is an operator surface: a plain user with no relationship to the
// project must not learn who works on it.
func TestAccessListIsReadableOnlyByOperators(t *testing.T) {
	accounts, _, alice, bob, carol := grantFixture(t)

	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Grants(bob, "team-a", alice.Username); err != nil {
		t.Fatalf("an operate grantee could not read the access list: %v", err)
	}
	if _, err := accounts.Grants(carol, "team-a", alice.Username); err == nil {
		t.Fatal("an unrelated user read the access list")
	}
}

func TestGrantsForReportsEveryProjectAnAccountReaches(t *testing.T) {
	accounts, root, alice, bob, _ := grantFixture(t)

	if err := accounts.db.CreateProject(control.Project{
		Name: "team-b", Owner: "alice", DataPlaneAuth: "public",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SetGrant(alice, "team-a", alice.Username, "bob", GrantOperate); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.SetGrant(root, "team-b", alice.Username, "bob", GrantView); err != nil {
		t.Fatal(err)
	}
	held, err := accounts.GrantsFor(bob.Username)
	if err != nil {
		t.Fatal(err)
	}
	if held["team-a"] != GrantOperate || held["team-b"] != GrantView || len(held) != 2 {
		t.Fatalf("grants for bob = %+v", held)
	}
}
