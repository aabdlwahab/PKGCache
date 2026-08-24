package feed

import (
	"path/filepath"
	"strings"
	"testing"
)

// The one property that matters about these two paths: the key is not underneath the
// directory that gets served. A traversal bug in the handler should not be able to reach
// it however bad the bug is.
func TestSigningKeyLivesOutsideTheServedTree(t *testing.T) {
	dataDir := filepath.FromSlash("/var/lib/pkgreg")
	repo, key := RepoDir(dataDir), KeyPath(dataDir)

	relative, err := filepath.Rel(repo, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(relative, "..") {
		t.Errorf("the signing key is inside the served tree:\n  repo: %s\n  key:  %s", repo, key)
	}
}
