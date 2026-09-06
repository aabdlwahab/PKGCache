package api

import (
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Which project this machine works in, as a control-plane surface.
//
// A server has no such thing and cannot: "the project" there is whichever one the reader
// is looking at, and two people are looking at two different ones through the same
// process. On one person's laptop it is a single machine-wide fact — the project
// `pkgcache run`, `pkgcache build` and pkgcache-docker put their work in when nothing
// names one — and it is kept in a file beside the cache so it outlives every daemon.
//
// The window has to be able to write that file, and this is the route by which it does.
// Without it the app's project switcher moved a value in the browser and nothing else:
// the window said "work" while every build on that machine went on filling `global`,
// with no error anywhere to suggest the two had ever disagreed.
//
// An interface for the same reason as LocalSources: the file's shape belongs to the
// package that owns the local profile, which sits above this one and cannot be imported
// from it.

// LocalSelection is the working project of a cache that belongs to one machine.
type LocalSelection interface {
	// Selected reports the stored choice, or the global project when none was made.
	Selected() (string, bool)
	// Select records the project later commands default to.
	Select(project string) error
	// Persisted reports the settings `pkgcache persist` installed, if any.
	Persisted() (PersistedSettings, bool)
	// Repoint rewrites those settings to name a different project.
	Repoint(project string) error
}

// PersistedSettings is what `pkgcache persist` wrote into this user's tool configuration.
//
// It names one project literally — `registry=http://…/global/npm/` — and deliberately
// does not follow a later switch, so that an editor nobody has reopened is not silently
// redirected mid-session. That decision is right and it is also invisible: the window
// said "work" while every `npm install` on the machine went to global, and no surface
// anywhere put those two facts next to each other. This is how it does.
type PersistedSettings struct {
	Project string `json:"project"`
	Files   int    `json:"files"`
}

// requireSelection refuses where nothing implements the surface, which is every server.
func (a *API) requireSelection() error {
	if a.Selection == nil {
		return control.NewError(http.StatusNotFound, "no_local_selection",
			"this instance does not belong to one machine, so it has no working project")
	}
	return nil
}

// getSelection reports the project this machine's commands default to.
//
// `chosen` is the difference between a machine that was asked and one that never was.
// Both work in the global project; only one of them has a file saying so, and `status`
// and the window both draw that distinction.
func (a *API) getSelection(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSelection(); err != nil {
		return err
	}
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a.selectionState())
	return nil
}

// selectionState is the whole answer: what the machine works in, and what the persisted
// tool settings name. Both, always, because the interesting case is them disagreeing and
// a client that had to ask twice could render the disagreement as a flicker.
func (a *API) selectionState() map[string]any {
	project, chosen := a.Selection.Selected()
	state := map[string]any{"project": project, "chosen": chosen}
	if settings, found := a.Selection.Persisted(); found {
		state["persisted"] = settings
	}
	return state
}

// repointSettings rewrites the persisted tool configuration to the machine's project.
//
// Its own route, and not a side effect of selecting: rewriting files in somebody's home
// directory is a second decision, and the whole reason the settings do not follow a
// switch on their own is that doing it unasked redirects a running editor. Asked for, it
// is exactly what somebody wants.
func (a *API) repointSettings(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSelection(); err != nil {
		return err
	}
	selected, _ := a.Selection.Selected()
	project, actor, err := a.requireOperate(r, selected)
	if err != nil {
		return err
	}
	if err := a.Selection.Repoint(project.Name); err != nil {
		return err
	}
	a.audit(r, actor, "project.repoint", project.Name, nil)
	writeJSON(w, http.StatusOK, a.selectionState())
	return nil
}

// putSelection records the project this machine's commands default to.
func (a *API) putSelection(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSelection(); err != nil {
		return err
	}
	var body struct {
		Project string `json:"project"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	// Validated against the registry before it is stored, for the same reason
	// `pkgcache project use` validates it: a default that names nothing fails at the
	// next `npm ci`, as a 404 from the router that says nothing about where it came
	// from. requireOperate also settles who may do this — everyone, on a cache with no
	// accounts, which is the only kind that has this surface.
	project, actor, err := a.requireOperate(r, body.Project)
	if err != nil {
		return err
	}
	if err := a.Selection.Select(project.Name); err != nil {
		return err
	}
	a.audit(r, actor, "project.select", project.Name, nil)
	writeJSON(w, http.StatusOK, a.selectionState())
	return nil
}
