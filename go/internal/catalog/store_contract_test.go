package catalog_test

import (
	"path/filepath"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/catalog/storetest"
)

// The SQLite store is the only implementation today, so this is what makes the
// interface a contract rather than a description of it.
func TestSQLiteSatisfiesStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) catalog.Store {
		t.Helper()
		db, err := catalog.Open(catalog.Options{
			Path: filepath.Join(t.TempDir(), "catalog.db"),
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	})
}
