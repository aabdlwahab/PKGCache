package local

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
)

// Selection implements the control plane's working-project surface for a local cache.
//
// The window's project switcher and `pkgcache project use` now write the same file, which
// is the whole point of it existing: they are two ways of saying one thing, and while only
// the command line could say it, a person who switched project in the app got a window
// that agreed with them and a machine that did not.
type Selection struct {
	// DataDir is where project.json lives.
	DataDir string
}

// Selected reports the stored choice, and whether one was ever made.
func (s *Selection) Selected() (string, bool) {
	return StoredProject(s.DataDir), HasStoredProject(s.DataDir)
}

// Select records the project later commands default to.
func (s *Selection) Select(project string) error {
	return SetCurrentProject(s.DataDir, project)
}

// StoredProject is CurrentProject without the environment override.
//
// The difference matters in exactly one place, and it is this surface. PKGCACHE_PROJECT
// belongs to one command's environment — a CI step that must not depend on what a
// previous one selected — and the daemon's environment is not the environment of the
// shell somebody runs `pkgcache build` in. Answering the window with the daemon's copy
// would show a project no command on that machine was necessarily using, and writing the
// file would then look like it had done nothing.
func StoredProject(dataDir string) string {
	data, err := os.ReadFile(projectPath(dataDir))
	if err != nil {
		return config.GlobalProject
	}
	var stored currentProject
	if err := json.Unmarshal(data, &stored); err != nil {
		return config.GlobalProject
	}
	if name := strings.TrimSpace(stored.Current); name != "" {
		return name
	}
	return config.GlobalProject
}

// HasStoredProject reports whether a choice was written, ignoring the environment for the
// same reason StoredProject does.
func HasStoredProject(dataDir string) bool {
	_, err := os.Stat(projectPath(dataDir))
	return err == nil
}

// Selection is what it claims to be.
var _ controlapi.LocalSelection = (*Selection)(nil)
