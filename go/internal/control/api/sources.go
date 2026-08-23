package api

import (
	"context"
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// Where a project's misses go, as a control-plane surface.
//
// On a server this is upstreams — rows in a table, edited by an operator who already has
// the CA in a file. On a laptop it is one question with a trust decision inside it: point
// this project at the team's cache, verifying it against a fingerprint somebody read to me
// over a desk. That whole operation — fetch the CA, refuse it unless it matches, write it
// into the bundle the outbound pool trusts, rewrite the project's chain, reload the pool —
// lives in the package that owns the local profile, above this one.
//
// So it arrives as an interface rather than as code here. The routes exist only when it is
// supplied, which means a server answers 404 for them rather than 501: the surface is
// absent, not broken.

// LocalSources is the per-project upstream configuration of a cache that keeps its own.
type LocalSources interface {
	// Sources reports every project's configuration, including inherited ones.
	Sources(ctx context.Context) ([]SourceState, error)
	// Configure points one project at a cache, verifying it first.
	Configure(ctx context.Context, project string, spec SourceSpec) (SourceState, error)
	// Forget removes one project's own configuration. It may then inherit another's.
	Forget(ctx context.Context, project string) error
	// Adopt gives a project that has just been created whatever configuration it
	// inherits.
	//
	// A project is born pointing nowhere: its chain is written by whoever configures a
	// source, and a project created afterwards never had one written for it. On a server
	// that is correct — an operator adds upstreams deliberately. On a laptop it is a
	// silent bypass: the new project resolves straight to the public internet while the
	// cache it was supposed to use sits configured and idle, and nothing anywhere says
	// so, because every request still succeeds.
	Adopt(ctx context.Context, project string) error
}

// SourceSpec is a request to point one project somewhere.
type SourceSpec struct {
	// Server is the origin, for example https://cache.internal:8443.
	Server string `json:"server"`
	// Fingerprint is the CA's SHA-256, and it is required. A browser cannot be trusted to
	// have obtained it out of band, but the person typing into one can — and without it
	// this would be a click that trusts whatever answers the address.
	Fingerprint string `json:"ca_sha256"`
	// TeamProject is the project name on the far side. Empty means their global project.
	TeamProject string `json:"team_project"`
	// Direct keeps the public registry in the chain behind the team cache.
	Direct bool `json:"direct"`
}

// SourceState is what one project resolves through.
type SourceState struct {
	Project     string `json:"project"`
	Server      string `json:"server,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	TeamProject string `json:"team_project,omitempty"`
	Direct      bool   `json:"direct"`
	// Inherited reports that this project is following another's configuration rather
	// than one of its own, which is the difference between "remove" and "override".
	Inherited bool `json:"inherited"`
	// Reachable is nil when it was not probed. A pointer because "not asked" and "down"
	// are different answers and a bool cannot hold both.
	Reachable *bool `json:"reachable,omitempty"`
}

// requireSources refuses where nothing implements the surface, which is every server.
//
// 404 rather than 501: a server does not have a half-working version of this, it has no
// such thing. The code says which, so a client can tell "this instance is not a local
// cache" from "this request was wrong".
func (a *API) requireSources() error {
	if a.Sources == nil {
		return control.NewError(http.StatusNotFound, "no_local_sources",
			"this instance does not keep per-project upstreams of its own")
	}
	return nil
}

func (a *API) listSources(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSources(); err != nil {
		return err
	}
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	sources, err := a.Sources.Sources(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list(sources)})
	return nil
}

func (a *API) putSource(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSources(); err != nil {
		return err
	}
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var spec SourceSpec
	if err := a.decode(r, &spec); err != nil {
		return err
	}
	state, err := a.Sources.Configure(r.Context(), name, spec)
	if err != nil {
		return err
	}
	// The fingerprint is audited, not the CA: it is the thing a person can compare against
	// what they were told, and it is what identifies which cache was trusted.
	a.audit(r, actor, "source.configure", name,
		map[string]any{"server": state.Server, "fingerprint": state.Fingerprint})
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (a *API) deleteSource(w http.ResponseWriter, r *http.Request) error {
	if err := a.requireSources(); err != nil {
		return err
	}
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	if err := a.Sources.Forget(r.Context(), name); err != nil {
		return err
	}
	a.audit(r, actor, "source.forget", name, nil)
	writeJSON(w, http.StatusOK, map[string]any{"forgotten": name})
	return nil
}
