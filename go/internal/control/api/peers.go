package api

import (
	"context"
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Another machine's cache, as a source for this one.
//
// Local-only in the same sense as LocalSources: a pkgreg is a server and its operator
// does not point it at somebody's laptop, so it supplies nothing here and answers 404.
//
// The surface exists because the rows are not the fact. One sibling is six or more
// upstream rows — one per ecosystem it fronts, plus the digest rows — and a console that
// wrote them individually would be reimplementing a decision that belongs in one place.

// LocalPeers is the sibling caches a local cache borrows from.
type LocalPeers interface {
	// Peers reports the siblings one project borrows from.
	Peers(ctx context.Context, project string) ([]PeerState, error)
	// AddPeer points a project at a sibling, replacing whatever it had for that one.
	AddPeer(ctx context.Context, project string, spec PeerSpec) (PeerState, error)
	// ForgetPeer removes every row a sibling was added as.
	ForgetPeer(ctx context.Context, project, name string) error
}

// PeerSpec is a request to borrow from one machine.
type PeerSpec struct {
	// Address is the sibling's base URL. A bare host is given the default port.
	Address string `json:"address"`
	// TheirProject is the project on the far side. Empty means their global project.
	//
	// It is theirs and not ours: two machines number their projects independently, and
	// assuming a name exists on somebody else's laptop is not this program's call — the
	// same reasoning as -team-project on a pkgreg.
	TheirProject string `json:"their_project"`
	// Name is what to call it here. Empty takes the host.
	Name string `json:"name"`
	// Token authenticates the digest half. Empty asks the sibling for one, which works
	// where it has no accounts; where that fails the chain half is still written.
	Token string `json:"token"`
}

// PeerState is one sibling, as this cache has it configured.
type PeerState struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// TheirProject is the project this cache fetches from on the far side.
	TheirProject string `json:"their_project"`
	// Through are the ecosystems fetched through it, as a team cache would be.
	Through []string `json:"through"`
	// Offline are the ones it can also answer by digest with this project offline.
	Offline []string `json:"offline"`
}

// requireLocalPeers refuses where nothing implements the surface, which is every server.
func (a *API) requireLocalPeers() error {
	if a.Peers == nil {
		return control.NewError(http.StatusNotFound, "no_local_peers",
			"this instance does not borrow from other machines' caches")
	}
	return nil
}

func (a *API) listLocalPeers(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireLocalPeers(); err != nil {
		return err
	}
	name := projectName(r)
	if _, _, err := a.requireView(r, name); err != nil {
		return err
	}
	peers, err := a.Peers.Peers(r.Context(), name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": list(peers)})
	return nil
}

func (a *API) addLocalPeer(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireLocalPeers(); err != nil {
		return err
	}
	name := projectName(r)
	project, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var spec PeerSpec
	if err := a.decode(r, &spec); err != nil {
		return err
	}
	state, err := a.Peers.AddPeer(r.Context(), project.Name, spec)
	if err != nil {
		return err
	}
	// The address and the far-side project are audited, not the token: those are what
	// identify where this project's misses now go, and the token is a secret.
	a.audit(r, actor, "peer.add", project.Name,
		map[string]any{"url": state.URL, "their_project": state.TheirProject})
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (a *API) forgetLocalPeer(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireLocalPeers(); err != nil {
		return err
	}
	name := projectName(r)
	project, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	peer := r.PathValue("peer")
	if err := a.Peers.ForgetPeer(r.Context(), project.Name, peer); err != nil {
		return err
	}
	a.audit(r, actor, "peer.forget", project.Name, map[string]any{"peer": peer})
	writeJSON(w, http.StatusOK, map[string]any{"forgotten": peer})
	return nil
}
