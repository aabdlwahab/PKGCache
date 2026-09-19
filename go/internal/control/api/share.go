package api

import (
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Opening a local cache's console to the network, from the window.
//
// pkgcache binds loopback and nothing else, and that is not relaxed here: the socket every
// client on this machine is pointed at stays where it is. Sharing adds a second socket, on
// every interface, that serves the console and the control API behind a password and
// refuses everything else. The password is the only thing standing between that socket and
// an unauthenticated control plane, so there is no way to turn sharing on without one.
//
// Local-only, in the same sense as LocalMigration: a server answers 404, because a server
// already decides who reaches its console through accounts, not through a switch.

// LocalShare opens and closes a local cache's console to other machines.
type LocalShare interface {
	// State says whether the console is shared, and where other machines reach it.
	State() ShareState
	// Enable shares the console behind password, or changes the password of a console
	// that is already shared. Either way every session signed in with the old one ends.
	Enable(password string, port int) (ShareState, error)
	// Disable closes the shared socket and forgets the password.
	Disable() (ShareState, error)
}

// ShareState is whether the console is reachable from other machines, and at what.
type ShareState struct {
	Enabled bool `json:"enabled"`
	// Port is the shared socket's, which is never the loopback one: see local.SharePort.
	Port int `json:"port"`
	// URLs are the addresses another machine can type, one per interface this machine
	// has an address on. Empty while not shared.
	URLs []string `json:"urls"`
	// Error is why a console that is meant to be shared is not being, in a sentence:
	// most often, another program holds the port.
	Error string `json:"error,omitempty"`
}

// shareRequest turns sharing on, or changes its password.
type shareRequest struct {
	Password string `json:"password"`
	// Port zero keeps the one already chosen, or the default.
	Port int `json:"port"`
}

// requireShare refuses where nothing implements the surface, which is every server.
func (a *API) requireShare() error {
	if a.Share == nil {
		return control.NewError(http.StatusNotFound, "no_local_share",
			"this instance does not share a console of its own")
	}
	return nil
}

func (a *API) getShare(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireShare(); err != nil {
		return err
	}
	if _, _, err := a.requireOperate(r, projectName(r)); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a.Share.State())
	return nil
}

func (a *API) putShare(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireShare(); err != nil {
		return err
	}
	_, actor, err := a.requireOperate(r, projectName(r))
	if err != nil {
		return err
	}
	var request shareRequest
	if err := a.decode(r, &request); err != nil {
		return err
	}
	state, err := a.Share.Enable(request.Password, request.Port)
	if err != nil {
		return err
	}
	a.audit(r, actor, "console.share", "", map[string]any{"port": state.Port})
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (a *API) deleteShare(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireShare(); err != nil {
		return err
	}
	_, actor, err := a.requireOperate(r, projectName(r))
	if err != nil {
		return err
	}
	state, err := a.Share.Disable()
	if err != nil {
		return err
	}
	a.audit(r, actor, "console.unshare", "", nil)
	writeJSON(w, http.StatusOK, state)
	return nil
}
