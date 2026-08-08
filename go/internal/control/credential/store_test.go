package credential

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/brightskies/pkgreg/internal/control"
)

func TestCredentialIsSealedAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	db, err := control.Open(filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyPath := filepath.Join(root, "host.key")
	store, err := Open(db, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(Plain{
		Label: "private", Kind: "basic", Username: "alice", Password: "supersecret",
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := db.Credential(id)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(record.Sealed, []byte("supersecret")) ||
		bytes.Contains(record.Sealed, []byte("alice")) {
		t.Fatal("credential plaintext is visible in control.db")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode = %o, want 600", info.Mode().Perm())
	}
	restarted, err := Open(db, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := restarted.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Username != "alice" || plain.Password != "supersecret" ||
		plain.Kind != "basic" {
		t.Fatalf("unsealed credential = %+v", plain)
	}
}
