package api

import (
	"errors"
	"net/http"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/maintenance"
)

// cancelFetch stops one download in flight.
//
// Operate, not view: every client reading that download sees its response end, which on a
// shared server is somebody else's build.
func (a *API) cancelFetch(w http.ResponseWriter, r *http.Request) error {
	project, actor, err := a.requireOperate(r, projectName(r))
	if err != nil {
		return err
	}
	if a.Engine == nil {
		return control.NewError(http.StatusNotImplemented, "no_engine",
			"this instance has no downloads to stop")
	}
	var body struct {
		Eco string `json:"eco"`
		Key string `json:"key"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	if body.Eco == "" || body.Key == "" {
		return control.NewError(http.StatusBadRequest, "invalid_fetch",
			"name the download to stop by its ecosystem and key")
	}
	if !a.Engine.CancelFetch(project.Name, body.Eco, body.Key) {
		// Most often it finished between the row being drawn and the click. Said as that,
		// because "not found" reads as the click having failed.
		return control.NewError(http.StatusNotFound, "not_in_flight",
			"%s is no longer downloading; it may have just finished", body.Key)
	}
	a.audit(r, actor, "fetch.cancel", project.Name,
		map[string]any{"eco": body.Eco, "key": body.Key})
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": body.Key})
	return nil
}

// transferArtifacts copies or moves named packages into another project.
//
// Operate on both ends: a copy writes into the destination, and a move also takes from
// the source, and permission to work on one project says nothing about the other.
func (a *API) transferArtifacts(w http.ResponseWriter, r *http.Request) error {
	source, actor, err := a.requireOperate(r, projectName(r))
	if err != nil {
		return err
	}
	if a.Maintenance == nil {
		return control.NewError(http.StatusNotImplemented, "no_maintenance",
			"this instance cannot move content between projects")
	}
	var body struct {
		To      string   `json:"to"`
		Digests []string `json:"digests"`
		Move    bool     `json:"move"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	if body.To == "" {
		return control.NewError(http.StatusBadRequest, "invalid_project",
			"choose the project to put these packages in")
	}
	destination, _, err := a.requireOperate(r, body.To)
	if err != nil {
		return err
	}
	if destination.Name == source.Name {
		return control.NewError(http.StatusBadRequest, "same_project",
			"%s is already where these packages are", source.Name)
	}
	digests := make(map[blob.Digest]struct{}, len(body.Digests))
	for _, raw := range body.Digests {
		digest, err := blob.ParseDigest(raw)
		if err != nil {
			return control.NewError(http.StatusBadRequest, "invalid_digest",
				"%q is not a sha256 digest", raw)
		}
		digests[digest] = struct{}{}
	}

	result, err := a.Maintenance.Transfer(r.Context(), maintenance.TransferOptions{
		From: source.Name, To: destination.Name, Digests: digests, Move: body.Move,
		Companions: a.companions,
	})
	if errors.Is(err, catalog.ErrQuota) {
		return control.NewError(http.StatusInsufficientStorage, "quota_exceeded",
			"%s has no room for these under its quota: %v", destination.Name, err)
	}
	if err != nil {
		return err
	}
	action := "artifact.copy"
	if body.Move {
		action = "artifact.move"
	}
	if result.Copied > 0 || result.Removed > 0 {
		a.audit(r, actor, action, source.Name, map[string]any{
			"to": destination.Name, "packages": result.Packages, "entries": result.Copied,
		})
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

// companions asks the ecosystem that owns a key what must travel with it, which is the
// whole of what this layer knows about the question.
func (a *API) companions(ecoID, key string, read func() ([]byte, error)) ([]string, error) {
	if a.Ecos == nil {
		return nil, nil
	}
	ecosystem, ok := a.Ecos.Get(ecoID)
	if !ok {
		return nil, nil
	}
	companions := ecosystem.Descriptor().Companions
	if companions == nil {
		return nil, nil
	}
	return companions(key, read)
}
