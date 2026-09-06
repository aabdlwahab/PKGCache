package local

import (
	"encoding/json"
	"errors"
	"io"
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
	// Home overrides the home directory the persisted settings live in. Empty means the
	// user running the daemon, which is the only correct answer in production and the one
	// a test must never be allowed to reach.
	Home string
}

// Selected reports the stored choice, and whether one was ever made.
func (s *Selection) Selected() (string, bool) {
	return StoredProject(s.DataDir), HasStoredProject(s.DataDir)
}

// Select records the project later commands default to.
func (s *Selection) Select(project string) error {
	return SetCurrentProject(s.DataDir, project)
}

// Persisted reports what `pkgcache persist` installed for this user.
func (s *Selection) Persisted() (controlapi.PersistedSettings, bool) {
	record, found := ReadPersisted(s.DataDir)
	if !found {
		return controlapi.PersistedSettings{}, false
	}
	return controlapi.PersistedSettings{
		Project: record.Project, Files: len(record.Files),
	}, true
}

// Repoint rewrites the persisted tool settings to name a different project.
//
// The address comes from the record rather than from the running daemon, because that is
// what the files themselves say and re-pointing is meant to change one thing. Rewriting
// the port at the same time would quietly repair — or quietly break — an installation
// somebody had made against a different configuration.
func (s *Selection) Repoint(project string) error {
	record, found := ReadPersisted(s.DataDir)
	if !found {
		return errors.New("local: nothing to re-point; `pkgcache persist` has not run here")
	}
	return ApplyPersist(PersistOptions{
		BaseURL: record.BaseURL,
		Project: project,
		DataDir: s.DataDir,
		// Recovered from the file rather than remembered, so an installation made before
		// this existed re-points with the same hosts it was installed with. An empty
		// result means git was left alone, which is the same choice made again.
		GitHosts: PersistedGitHosts(record),
		// Availability was settled when these files were installed and is not in
		// question here: this rewrites URLs in files that already exist, and refusing
		// over a question already answered would leave them naming the wrong project.
		Available: AvailabilityAccepted,
		Home:      s.Home,
		Out:       io.Discard,
	})
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
