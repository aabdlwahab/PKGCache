package api

import (
	"context"
	"net/http"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Moving a cache to another disk, from the window.
//
// `pkgcache migrate` does this from a terminal, and the window cannot simply call the same
// code: the page is served by the daemon, and the daemon holds the lock on the very
// directory being moved. So this surface splits the job the way the processes have to.
// The daemon answers the questions that need no lock — where the cache is, whether it can
// go where somebody pointed, how much there is — and then hands the move to a process of
// its own, which stops the daemon, moves the cache and starts it again. The window loses
// its connection in between and reads the outcome from whichever daemon answers after.
//
// Local-only, in the same sense as LocalFiles: a server answers 404, because it has no
// disk the reader is standing at.

// LocalMigration moves a local cache to another directory.
type LocalMigration interface {
	// Location says where the cache is, whether it can be moved from here, and how the
	// last move started from a window ended.
	Location(ctx context.Context) (MigrationLocation, error)
	// Plan answers whether the cache can go to the directory somebody chose, and changes
	// nothing.
	Plan(ctx context.Context, chosen string) (MigrationPlan, error)
	// Start hands the move off and returns. The move itself stops this daemon.
	Start(ctx context.Context, chosen string) (MigrationPlan, error)
	// Acknowledge forgets the last move's outcome, once a window has shown it.
	Acknowledge() error
}

// MigrationLocation is where a local cache is, and whether a window may move it.
type MigrationLocation struct {
	DataDir string `json:"data_dir"`
	// MovedFrom is the default location, when a signpost there sends everything here.
	MovedFrom string `json:"moved_from,omitempty"`
	Movable   bool   `json:"movable"`
	// Reason says why not, in a sentence that names what to do instead.
	Reason string           `json:"reason,omitempty"`
	Last   *MigrationRecord `json:"last,omitempty"`
}

// MigrationPlan is the answer to "can the cache go there".
//
// Problems are sentences rather than an error because the reader is a dialog: a disk that
// cannot hardlink is something to show beside a disabled button, not a failed request.
type MigrationPlan struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
	// FreeBytes is what the destination's filesystem has available, -1 when it would not
	// say.
	FreeBytes int64 `json:"free_bytes"`
	// SameDisk means the move is a rename and frees nothing where the cache is now —
	// allowed, and worth saying before somebody does it to reclaim space.
	SameDisk bool     `json:"same_disk"`
	Problems []string `json:"problems"`
}

// Migration states, as a window reads them.
const (
	MigrationMoving = "moving"
	MigrationDone   = "done"
	MigrationFailed = "failed"
)

// MigrationRecord is how a move a window started went.
type MigrationRecord struct {
	State    string     `json:"state"`
	From     string     `json:"from"`
	To       string     `json:"to"`
	Bytes    int64      `json:"bytes,omitempty"`
	Files    int        `json:"files,omitempty"`
	Renamed  bool       `json:"renamed,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
}

// migrationRequest names the directory somebody chose in the picker.
type migrationRequest struct {
	Path string `json:"path"`
}

// requireMigration refuses where nothing implements the surface, which is every server.
func (a *API) requireMigration() error {
	if a.Migration == nil {
		return control.NewError(http.StatusNotFound, "no_local_migration",
			"this instance does not move a cache of its own")
	}
	return nil
}

func (a *API) getMigration(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireMigration(); err != nil {
		return err
	}
	// Operate, as for the file browser: the answer is a path on this machine's disk.
	if _, _, err := a.requireOperate(r, projectName(r)); err != nil {
		return err
	}
	location, err := a.Migration.Location(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, location)
	return nil
}

func (a *API) planMigration(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireMigration(); err != nil {
		return err
	}
	if _, _, err := a.requireOperate(r, projectName(r)); err != nil {
		return err
	}
	var request migrationRequest
	if err := a.decode(r, &request); err != nil {
		return err
	}
	plan, err := a.Migration.Plan(r.Context(), request.Path)
	if err != nil {
		return err
	}
	if plan.Problems == nil {
		plan.Problems = []string{}
	}
	writeJSON(w, http.StatusOK, plan)
	return nil
}

func (a *API) startMigration(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireMigration(); err != nil {
		return err
	}
	_, actor, err := a.requireOperate(r, projectName(r))
	if err != nil {
		return err
	}
	var request migrationRequest
	if err := a.decode(r, &request); err != nil {
		return err
	}
	plan, err := a.Migration.Start(r.Context(), request.Path)
	if err != nil {
		return err
	}
	a.audit(r, actor, "cache.migrate", plan.To,
		map[string]any{"from": plan.From, "to": plan.To, "bytes": plan.Bytes})
	if plan.Problems == nil {
		plan.Problems = []string{}
	}
	// Accepted, not OK: what has happened is that a process was started, and the move it
	// performs is going to take this daemon down before it is done.
	writeJSON(w, http.StatusAccepted, plan)
	return nil
}

func (a *API) acknowledgeMigration(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireMigration(); err != nil {
		return err
	}
	if _, _, err := a.requireOperate(r, projectName(r)); err != nil {
		return err
	}
	if err := a.Migration.Acknowledge(); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
