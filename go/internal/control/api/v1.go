package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/auth"
	"github.com/aabdlwahab/PKGCache/internal/control/credential"
	controlproject "github.com/aabdlwahab/PKGCache/internal/control/project"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/maintenance"
)

func (a *API) v1Routes() {
	a.route("GET /api/v1/ecosystems", a.getEcosystems)
	a.route("GET /api/v1/projects", a.listProjects)
	a.route("POST /api/v1/projects", a.createProject)
	a.route("GET /api/v1/projects/{project}", a.getProject)
	a.route("PATCH /api/v1/projects/{project}", a.patchProject)
	a.route("DELETE /api/v1/projects/{project}", a.deleteProject)
	a.route("GET /api/v1/projects/{project}/grants", a.listGrants)
	a.route("PUT /api/v1/projects/{project}/grants/{name}", a.putGrant)
	a.route("DELETE /api/v1/projects/{project}/grants/{name}", a.deleteGrant)
	a.route("GET /api/v1/projects/{project}/artifacts", a.getArtifacts)
	a.route("GET /api/v1/projects/{project}/endpoints", a.getEndpoints)
	a.route("GET /api/v1/projects/{project}/setup.sh", a.getSetupShell)
	a.route("GET /api/v1/projects/{project}/setup.ps1", a.getSetupPowerShell)
	a.route("GET /api/v1/projects/{project}/upstreams", a.listUpstreams)
	a.route("POST /api/v1/projects/{project}/upstreams", a.createUpstream)
	a.route("PATCH /api/v1/projects/{project}/upstreams/{id}", a.patchUpstream)
	a.route("DELETE /api/v1/projects/{project}/upstreams/{id}", a.deleteUpstream)
	a.route("GET /api/v1/projects/{project}/snapshots", a.listSnapshots)
	a.route("POST /api/v1/projects/{project}/snapshots", a.createSnapshotJob)
	a.route("POST /api/v1/projects/{project}/snapshots/{id}/rollback", a.rollbackJob)
	a.route("POST /api/v1/projects/{project}/export", a.exportJob)
	a.route("POST /api/v1/projects/{project}/import", a.importJob)
	a.route("POST /api/v1/projects/{project}/lockwarm", a.lockwarmJob)
	a.route("GET /api/v1/stats", a.getStats)
	a.route("GET /api/v1/jobs", a.listJobs)
	a.route("GET /api/v1/jobs/{id}", a.getJob)
	a.route("DELETE /api/v1/jobs/{id}", a.cancelJob)
	a.route("GET /api/v1/tokens", a.listTokens)
	a.route("POST /api/v1/tokens", a.createToken)
	a.route("DELETE /api/v1/tokens/{id}", a.deleteToken)
	a.route("GET /api/v1/users", a.listUsers)
	a.route("POST /api/v1/users", a.createUser)
	a.route("PATCH /api/v1/users/{name}", a.patchUser)
	a.route("DELETE /api/v1/users/{name}", a.deleteUser)
	a.route("POST /api/v1/login", a.login)
	a.route("POST /api/v1/login/guest", a.loginGuest)
	a.route("POST /api/v1/logout", a.logout)
	a.route("GET /api/v1/me", a.me)
	a.route("GET /api/v1/audit", a.listAudit)
	a.route("GET /api/v1/events", a.events)
	a.route("POST /api/v1/maintenance/gc", a.gcJob)
	a.route("POST /api/v1/projects/{project}/maintenance/evict", a.evictJob)
	a.route("POST /api/v1/projects/{project}/maintenance/remove", a.removeArtifacts)
	// Registered unconditionally, and each one refuses when nothing supplies the surface.
	// The routes cannot be made conditional: this runs at construction, and the only thing
	// that can implement them is built from the instance this is part of — so the hook is
	// always set after the table exists. See LocalSources and requireSources.
	a.route("GET /api/v1/local/sources", a.listSources)
	a.route("PUT /api/v1/local/sources/{project}", a.putSource)
	a.route("DELETE /api/v1/local/sources/{project}", a.deleteSource)
}

func (a *API) gcJob(w http.ResponseWriter, r *http.Request) error {
	actor, err := a.guard.RequireSuperuser(r)
	if err != nil {
		return err
	}
	var body map[string]any
	if r.ContentLength != 0 {
		if err := a.decode(r, &body); err != nil {
			return err
		}
	}
	return a.submitJob(w, r, actor, "", "gc", body)
}

// removeArtifacts drops named content from one project.
//
// Synchronous, unlike gc and evict. Those sweep a whole store and report progress worth
// watching; this is the handful of digests somebody ticked in a list, and a queue plus a
// poll between the click and the answer would be machinery around nothing.
func (a *API) removeArtifacts(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	if a.Maintenance == nil {
		return control.NewError(http.StatusNotImplemented, "no_maintenance",
			"this instance cannot remove content")
	}
	var body struct {
		Digests []string `json:"digests"`
		DryRun  bool     `json:"dry_run"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
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
	result, err := a.Maintenance.Remove(r.Context(), maintenance.RemoveOptions{
		Project: name, Digests: digests, DryRun: body.DryRun,
	})
	if err != nil {
		return err
	}
	if !body.DryRun && result.Entries > 0 {
		a.audit(r, actor, "artifact.remove", name, map[string]any{
			"entries": result.Entries, "reclaimed_bytes": result.ReclaimedBytes,
		})
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (a *API) evictJob(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body map[string]any
	if r.ContentLength != 0 {
		if err := a.decode(r, &body); err != nil {
			return err
		}
	}
	return a.submitJob(w, r, actor, name, "evict", body)
}

func (a *API) getEcosystems(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	out := make([]map[string]any, 0, a.Ecos.Len())
	for _, descriptor := range a.Ecos.Descriptors() {
		defaults := descriptor.DefaultUpstreams
		if defaults == nil {
			defaults = map[string]string{}
		}
		out = append(out, map[string]any{
			"id": descriptor.ID, "display": descriptor.Display,
			"summary": descriptor.Summary, "storage": descriptor.Storage,
			"listener": descriptor.Listener, "upstream_shape": descriptor.Upstreams,
			"default_upstreams": defaults,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ecosystems": list(out)})
	return nil
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) error {
	projects, err := a.Projects.List()
	if err != nil {
		return err
	}
	if a.Accounts.Enabled() {
		actor, found := a.guard.Actor(r)
		switch {
		case !found:
			if !a.Config.Current().Auth.AnonRead {
				return control.NewError(http.StatusUnauthorized,
					"authentication_required", "authentication required")
			}
		case auth.IsGuest(actor):
			// A guest sees one project and must not learn that the others exist:
			// tenant names alone are information, and the project switcher would
			// otherwise offer a list of doors that all answer 403.
			kept := projects[:0]
			for _, project := range projects {
				if project.Name == config.GlobalProject {
					kept = append(kept, project)
				}
			}
			projects = kept
		case actor.Role != "superuser":
			projects = a.Projects.Visible(projects, func(project control.Project) bool {
				return a.Accounts.CanViewOn(actor, project.Name, project.Owner)
			})
		}
	}
	out := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		out = append(out, a.projectJSON(project))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list(out)})
	return nil
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) error {
	actor, err := a.guard.RequireCreate(r)
	if err != nil {
		return err
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	project, err := a.Projects.Create(body.Name, actor.Username)
	if err != nil {
		return err
	}
	// A cache that keeps its own sources gives the new project the one it inherits, here
	// rather than in each caller. `pkgcache project create` did this for itself, so a
	// project made from the widget or over the API got no chain at all and quietly went
	// direct — the failure this whole surface exists to prevent.
	//
	// Reported rather than swallowed. The project does exist by now, so this cannot be
	// undone by failing; what it can do is say plainly that the project is not pointed
	// anywhere yet, instead of leaving that to be discovered by a slow build.
	if a.Sources != nil {
		if err := a.Sources.Adopt(r.Context(), project.Name); err != nil {
			return fmt.Errorf(
				"project %s was created but could not be pointed at a cache: %w",
				project.Name, err)
		}
	}
	a.audit(r, actor, "project.create", project.Name, nil)
	writeJSON(w, http.StatusCreated, a.projectJSON(project))
	return nil
}

func (a *API) getProject(w http.ResponseWriter, r *http.Request) error {
	project, _, err := a.requireView(r, projectName(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a.projectJSON(project))
	return nil
}

func (a *API) patchProject(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	project, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body struct {
		Mode           *string `json:"mode"`
		Offline        *bool   `json:"offline"`
		QuotaBytes     *int64  `json:"quota_bytes"`
		QuotaArtifacts *int64  `json:"quota_artifacts"`
		Owner          *string `json:"owner"`
		Auth           *string `json:"auth"`
		DataPlaneAuth  *string `json:"data_plane_auth"`
		RateLimit      *int    `json:"rate_limit"`
		RateBurst      *int    `json:"rate_burst"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	patch := controlproject.Patch{
		Offline: body.Offline, QuotaBytes: body.QuotaBytes,
		QuotaArtifacts: body.QuotaArtifacts, DataPlaneAuth: body.DataPlaneAuth,
		RateLimit: body.RateLimit, RateBurst: body.RateBurst,
	}
	if body.Mode != nil {
		value := *body.Mode == "offline"
		if *body.Mode != "online" && *body.Mode != "offline" {
			return control.NewError(http.StatusBadRequest, "invalid_mode",
				"mode must be online or offline")
		}
		patch.Offline = &value
	}
	if body.Auth != nil {
		patch.DataPlaneAuth = body.Auth
	}
	if body.Owner != nil && *body.Owner != project.Owner {
		actor, err = a.guard.RequireSuperuser(r)
		if err != nil {
			return err
		}
		owner, found := a.Accounts.Get(*body.Owner)
		if !found || (owner.Role != "admin" && owner.Role != "superuser") {
			return control.NewError(http.StatusBadRequest, "invalid_owner",
				"owner must be an existing admin or superuser")
		}
		patch.Owner = body.Owner
	}
	updated, err := a.Projects.Update(name, patch)
	if err != nil {
		return err
	}
	a.audit(r, actor, "project.update", name, map[string]any{"project": updated})
	writeJSON(w, http.StatusOK, a.projectJSON(updated))
	return nil
}

func (a *API) deleteProject(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOwner(r, name)
	if err != nil {
		return err
	}
	if name == config.GlobalProject {
		return a.Projects.Delete(name)
	}
	// Remove catalog roots before unregistering the tenant. The blobs themselves
	// remain shared and are reclaimed by online GC only when no other project or
	// retained snapshot references them.
	if _, err := a.Catalog.DeleteProject(name); err != nil {
		return err
	}
	if err := a.Projects.Delete(name); err != nil {
		return err
	}
	a.audit(r, actor, "project.delete", name, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
	return nil
}

// listGrants reports who else may reach this project.
//
// Not folded into the project object: the access list is an admin concern that most
// readers of a project never need, and GET /projects is on the console's hot path for
// every signed-in session.
func (a *API) listGrants(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	project, err := a.project(r, name)
	if err != nil {
		return err
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return err
	}
	grants, err := a.Accounts.Grants(actor, project.Name, project.Owner)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project.Name, "owner": project.Owner, "grants": list(grants),
	})
	return nil
}

// putGrant is idempotent on purpose: granting operate to an account that already has
// view is the same request as granting it the first time, and an API that made the
// caller know which one it was would push that state into every client.
func (a *API) putGrant(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	project, actor, err := a.requireOwner(r, name)
	if err != nil {
		return err
	}
	var body struct {
		Level string `json:"level"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	grant, err := a.Accounts.SetGrant(
		actor, project.Name, project.Owner, r.PathValue("name"), body.Level)
	if err != nil {
		return err
	}
	a.audit(r, actor, "project.grant", project.Name, map[string]any{
		"username": grant.Username, "level": grant.Level,
	})
	writeJSON(w, http.StatusOK, grant)
	return nil
}

func (a *API) deleteGrant(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	project, actor, err := a.requireOwner(r, name)
	if err != nil {
		return err
	}
	username := r.PathValue("name")
	if err := a.Accounts.RevokeGrant(actor, project.Name, project.Owner, username); err != nil {
		return err
	}
	a.audit(r, actor, "project.revoke", project.Name, map[string]any{"username": username})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": username, "project": project.Name})
	return nil
}

func (a *API) getArtifacts(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	if _, _, err := a.requireView(r, name); err != nil {
		return err
	}
	query := r.URL.Query()
	rows, total, err := a.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: name, Eco: query.Get("eco"), Search: query.Get("q"),
		Sort: query.Get("sort"), Page: parseInt(query.Get("page"), 1), PageSize: 100,
	})
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, artifact := range rows {
		out = append(out, artifactJSON(artifact))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts": out, "total": total, "page": parseInt(query.Get("page"), 1),
	})
	return nil
}

func (a *API) getEndpoints(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	if _, _, err := a.requireView(r, name); err != nil {
		return err
	}
	host, unifiedPort, proxyPort, err := a.clientCoordinates(r)
	if err != nil {
		return err
	}
	onboarding, err := a.onboardingJSON(name)
	if err != nil {
		var clientErr *control.Error
		if !errors.As(err, &clientErr) || clientErr.Status != http.StatusNotFound {
			return err
		}
		onboarding = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":    name,
		"endpoints":  a.endpointMap(name, host, unifiedPort, proxyPort),
		"onboarding": onboarding,
	})
	return nil
}

func (a *API) listUpstreams(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	if _, _, err := a.requireView(r, name); err != nil {
		return err
	}
	rows, err := a.Projects.Upstreams(name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"upstreams": list(rows)})
	return nil
}

type upstreamBody struct {
	Eco          *string `json:"eco"`
	Name         *string `json:"name"`
	URL          *string `json:"url"`
	Kind         *string `json:"kind"`
	Priority     *int    `json:"priority"`
	Enabled      *bool   `json:"enabled"`
	CredentialID *int64  `json:"credential_id"`
	Credential   *struct {
		Label    string `json:"label"`
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	} `json:"credential"`
}

func (body upstreamBody) value(base control.Upstream) control.Upstream {
	value := base
	if body.Eco != nil {
		value.Eco = *body.Eco
	}
	if body.Name != nil {
		value.Name = *body.Name
	}
	if body.URL != nil {
		value.URL = *body.URL
	}
	if body.Kind != nil {
		value.Kind = *body.Kind
	}
	if body.Priority != nil {
		value.Priority = *body.Priority
	}
	if body.Enabled != nil {
		value.Enabled = *body.Enabled
	}
	if body.CredentialID != nil {
		value.CredentialID = body.CredentialID
	}
	return value
}

func (a *API) createUpstream(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body upstreamBody
	if err := a.decode(r, &body); err != nil {
		return err
	}
	value := body.value(control.Upstream{Project: name, Enabled: true})
	var createdCredential int64
	if body.Credential != nil {
		createdCredential, err = a.Credentials.Create(credential.Plain{
			Label: body.Credential.Label, Kind: body.Credential.Kind,
			Username: body.Credential.Username, Password: body.Credential.Password,
			Token: body.Credential.Token,
		})
		if err != nil {
			return control.NewError(http.StatusBadRequest, "invalid_credential",
				"%s", err.Error())
		}
		value.CredentialID = &createdCredential
	}
	upstream, err := a.Projects.AddUpstream(name, value)
	if err != nil {
		if createdCredential != 0 {
			_ = a.Credentials.Delete(createdCredential)
		}
		return err
	}
	a.audit(r, actor, "upstream.create", strconv.FormatInt(upstream.ID, 10),
		map[string]any{"project": name, "eco": upstream.Eco, "name": upstream.Name})
	writeJSON(w, http.StatusCreated, upstream)
	return nil
}

func (a *API) patchUpstream(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		return err
	}
	var body upstreamBody
	if err := a.decode(r, &body); err != nil {
		return err
	}
	current, err := a.Projects.Upstream(name, id)
	if err != nil {
		return err
	}
	value := body.value(current)
	if body.Credential == nil {
		value.CredentialID = current.CredentialID
	}
	var createdCredential int64
	if body.Credential != nil {
		createdCredential, err = a.Credentials.Create(credential.Plain{
			Label: body.Credential.Label, Kind: body.Credential.Kind,
			Username: body.Credential.Username, Password: body.Credential.Password,
			Token: body.Credential.Token,
		})
		if err != nil {
			return control.NewError(http.StatusBadRequest, "invalid_credential",
				"%s", err.Error())
		}
		value.CredentialID = &createdCredential
	}
	upstream, err := a.Projects.UpdateUpstream(name, value)
	if err != nil {
		if createdCredential != 0 {
			_ = a.Credentials.Delete(createdCredential)
		}
		return err
	}
	if createdCredential != 0 && current.CredentialID != nil {
		_ = a.Credentials.Delete(*current.CredentialID)
	}
	a.audit(r, actor, "upstream.update", r.PathValue("id"), map[string]any{"project": name})
	writeJSON(w, http.StatusOK, upstream)
	return nil
}

func (a *API) deleteUpstream(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := a.Projects.DeleteUpstream(name, id); err != nil {
		return err
	}
	a.audit(r, actor, "upstream.delete", r.PathValue("id"), map[string]any{"project": name})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	return nil
}

func (a *API) listSnapshots(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	if _, _, err := a.requireView(r, name); err != nil {
		return err
	}
	rows, err := a.Catalog.ListSnapshots(name, 100)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, snapshot := range rows {
		out = append(out, snapshotJSON(snapshot))
	}
	// The head, because a transfer needs it and it cannot be inferred from this list. A
	// rollback moves the head backwards without removing what came after, so the tip of
	// the parent chain and the checkpoint a project is actually on are different
	// questions — and an import is refused unless the pack's base matches the second
	// one. Empty until the project has been checkpointed.
	head, err := a.Catalog.GetHead(name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list(out), "head": head})
	return nil
}

func (a *API) createSnapshotJob(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := a.decode(r, &body); err != nil {
		return err
	}
	return a.submitJob(w, r, actor, name, "checkpoint", body)
}

func (a *API) rollbackJob(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	return a.submitJob(w, r, actor, name, "rollback",
		map[string]any{"snapshot": r.PathValue("id")})
}

func (a *API) exportJob(w http.ResponseWriter, r *http.Request) error {
	return a.projectJob(w, r, "export")
}

// lockwarmJob pre-fetches everything a uv.lock pins. The job has been registered in
// ops since it was written; until now the only way to reach it was the legacy
// POST /api/jobs shim, so the versioned API could not warm a cache at all.
func (a *API) lockwarmJob(w http.ResponseWriter, r *http.Request) error {
	return a.projectJob(w, r, "lockwarm")
}

func (a *API) importJob(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	var (
		actor control.User
		err   error
	)
	if _, projectErr := a.Projects.Get(name); projectErr == nil {
		_, actor, err = a.requireOperate(r, name)
	} else if !isProjectNotFound(projectErr) {
		return projectErr
	} else {
		actor, err = a.guard.RequireCreate(r)
	}
	if err != nil {
		return err
	}
	var body map[string]any
	if r.ContentLength != 0 {
		if err := a.decode(r, &body); err != nil {
			return err
		}
	}
	return a.submitJob(w, r, actor, name, "import", body)
}

func (a *API) projectJob(w http.ResponseWriter, r *http.Request, action string) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body map[string]any
	if r.ContentLength != 0 {
		if err := a.decode(r, &body); err != nil {
			return err
		}
	}
	return a.submitJob(w, r, actor, name, action, body)
}

func (a *API) submitJob(
	w http.ResponseWriter, r *http.Request, actor control.User,
	project, action string, params map[string]any,
) error {
	record, err := a.Jobs.Submit(project, action, actorName(actor), params)
	if err != nil {
		return err
	}
	a.audit(r, actor, "job.create", strconv.FormatInt(record.ID, 10),
		map[string]any{"project": project, "action": action})
	writeJSON(w, http.StatusAccepted, record)
	return nil
}

func (a *API) getStats(w http.ResponseWriter, r *http.Request) error {
	project := r.URL.Query().Get("project")
	if project != "" {
		if _, _, err := a.requireView(r, project); err != nil {
			return err
		}
	} else if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	result, err := a.Catalog.Stats(catalog.StatsQuery{
		Project: project, Eco: r.URL.Query().Get("eco"),
	})
	if err != nil {
		return err
	}
	// Storage rides along rather than living at its own endpoint: every caller that
	// wants the totals wants the room left beside them, and splitting the two would
	// mean two round trips to draw one tile.
	storage, err := a.storageNow()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, statsResponse{StatsResult: result, Storage: storage})
	return nil
}

// statsResponse embeds the catalog aggregate so existing fields keep their shape and
// existing clients keep working.
type statsResponse struct {
	catalog.StatsResult
	Storage storageDetail `json:"storage"`
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	rows, err := a.Jobs.List(100)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list(rows)})
	return nil
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		return err
	}
	record, err := a.Jobs.Get(id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, record)
	return nil
}

func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) error {
	id, err := parseInt64(r.PathValue("id"))
	if err != nil {
		return err
	}
	record, err := a.Jobs.Get(id)
	if err != nil {
		return err
	}
	_, actor, err := a.requireOperate(r, record.Project)
	if err != nil {
		return err
	}
	if err := a.Jobs.Cancel(id); err != nil {
		return err
	}
	a.audit(r, actor, "job.cancel", r.PathValue("id"), nil)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": id})
	return nil
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) error {
	project := r.URL.Query().Get("project")
	if project == "" {
		if _, err := a.guard.RequireSuperuser(r); err != nil {
			return err
		}
	} else if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	rows, err := a.Tokens.List(project)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": list(rows)})
	return nil
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Project    string `json:"project"`
		Eco        string `json:"eco"`
		Scope      string `json:"scope"`
		Label      string `json:"label"`
		TTLSeconds int64  `json:"ttl_seconds"`
		RateLimit  int    `json:"rate_limit"`
		RateBurst  int    `json:"rate_burst"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	if body.Project == "" {
		body.Project = config.GlobalProject
	}
	_, actor, err := a.requireOperate(r, body.Project)
	if err != nil {
		return err
	}
	if body.Eco != "" {
		if _, found := a.Ecos.Get(body.Eco); !found {
			return control.NewError(http.StatusBadRequest, "invalid_ecosystem",
				"unknown ecosystem: %s", body.Eco)
		}
	}
	token, secret, err := a.Tokens.IssueWithLimit(body.Project, body.Eco, body.Scope,
		body.Label, actor.Username, time.Duration(body.TTLSeconds)*time.Second,
		body.RateLimit, body.RateBurst)
	if err != nil {
		return err
	}
	a.audit(r, actor, "token.create", token.ID,
		map[string]any{"project": body.Project, "eco": body.Eco, "scope": body.Scope})
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "secret": secret})
	return nil
}

func (a *API) deleteToken(w http.ResponseWriter, r *http.Request) error {
	token, err := a.Tokens.Get(r.PathValue("id"))
	if errors.Is(err, control.ErrNotFound) {
		return control.NewError(http.StatusNotFound, "token_not_found", "no such token")
	}
	if err != nil {
		return err
	}
	project := token.Project
	if project == "" {
		project = config.GlobalProject
	}
	_, actor, err := a.requireOperate(r, project)
	if err != nil {
		return err
	}
	if err := a.Tokens.Revoke(token.ID); err != nil {
		return err
	}
	a.audit(r, actor, "token.delete", token.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": token.ID})
	return nil
}

// errNoAccounts is what the account endpoints answer where there are no accounts.
//
// Not 401. An instance with accounts disabled — pkgcache on a laptop — has nobody to
// authenticate as, so "authentication required" describes a door that does not exist and
// sends a client looking for a login form. The console already branches on
// me.auth_enabled and hides the panel; this is for everything that does not, starting
// with a person reading curl output and wondering what credential they are missing.
func errNoAccounts() error {
	return control.NewError(http.StatusNotFound, "auth_disabled",
		"this instance has no accounts, so there are no users to manage")
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) error {
	if !a.Accounts.Enabled() {
		return errNoAccounts()
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return err
	}
	users, err := a.Accounts.List(actor)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": list(users)})
	return nil
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) error {
	if !a.Accounts.Enabled() {
		return errNoAccounts()
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return err
	}
	var body struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		Role      string `json:"role"`
		ReportsTo string `json:"reports_to"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	user, err := a.Accounts.Create(actor, body.Username, body.Password, body.Role, body.ReportsTo)
	if err != nil {
		return err
	}
	a.audit(r, actor, "user.create", user.Username, map[string]any{"role": user.Role})
	writeJSON(w, http.StatusCreated, user)
	return nil
}

func (a *API) patchUser(w http.ResponseWriter, r *http.Request) error {
	if !a.Accounts.Enabled() {
		return errNoAccounts()
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := a.decode(r, &raw); err != nil {
		return err
	}
	change := auth.UserChanges{}
	if value, found := raw["role"]; found {
		role, ok := value.(string)
		if !ok {
			return control.NewError(http.StatusBadRequest, "invalid_role", "role must be a string")
		}
		change.Role = &role
	}
	if value, found := raw["password"]; found {
		password, ok := value.(string)
		if !ok {
			return control.NewError(http.StatusBadRequest, "invalid_password", "password must be a string")
		}
		change.Password = &password
	}
	if value, found := raw["reports_to"]; found {
		change.ReportsToSet = true
		if value != nil {
			manager, ok := value.(string)
			if !ok {
				return control.NewError(http.StatusBadRequest, "invalid_reports_to",
					"reports_to must be a string or null")
			}
			change.ReportsTo = &manager
		}
	}
	user, err := a.Accounts.Update(actor, r.PathValue("name"), change)
	if err != nil {
		return err
	}
	a.audit(r, actor, "user.update", user.Username, nil)
	writeJSON(w, http.StatusOK, user)
	return nil
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) error {
	if !a.Accounts.Enabled() {
		return errNoAccounts()
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return err
	}
	username := r.PathValue("name")
	if err := a.Accounts.Delete(actor, username); err != nil {
		return err
	}
	a.audit(r, actor, "user.delete", username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": username})
	return nil
}

// cookieSecure decides the session cookie's Secure attribute.
//
// Derived from the connection that is issuing it, not from configuration alone: a
// cookie marked Secure is never sent back over plain HTTP, so guessing wrong in one
// direction leaks the session and in the other silently breaks every login. An
// explicit auth.cookie_secure still wins, for deployments terminating TLS somewhere
// this process cannot observe.
func cookieSecure(r *http.Request, snapshot *config.Snapshot) bool {
	if snapshot.Auth.CookieSecure != nil {
		return *snapshot.Auth.CookieSecure
	}
	if r.TLS != nil {
		return true
	}
	if snapshot.Server.TrustProxy &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(snapshot.Auth.PublicOrigin), "https://")
}

// refuseCleartextLogin rejects a password exchange that would cross the network in the
// clear on a host that has an encrypted way in.
//
// The condition is deliberately the exact negation of cookieSecure's: if this request
// would receive a cookie without the Secure attribute, the session it mints is
// replayable by anyone on the path, and so is the password that bought it. The
// listener-level redirect already prevents this on the single port, but the check
// belongs here too — it is the API, not the listener layout, that knows a credential is
// being exchanged, and an explicit-listener or future embedding could route around the
// redirect without anyone noticing.
//
// Only when this process terminates TLS. A deployment with no certificate pair is
// plaintext on purpose — behind a TLS-terminating proxy, or on a trusted network — and
// refusing its logins would break it for no gain, since there is no https to redirect
// to. Such a deployment should set auth.public_origin or auth.cookie_secure, which
// makes cookieSecure true and this check moot.
func refuseCleartextLogin(r *http.Request, snapshot *config.Snapshot) error {
	if !snapshot.Server.TLS.Enabled() || cookieSecure(r, snapshot) {
		return nil
	}
	return control.NewError(http.StatusUpgradeRequired, "https_required",
		"refusing to accept a password over cleartext: this server terminates TLS, "+
			"so sign in over https instead. If TLS terminates in front of this "+
			"process, set auth.public_origin to the browser-facing https origin.")
}

func (a *API) login(w http.ResponseWriter, r *http.Request) error {
	if err := refuseCleartextLogin(r, a.Config.Current()); err != nil {
		return err
	}
	ip := a.guard.ClientIP(r)
	if a.Sessions.Blocked(ip) {
		return control.NewError(http.StatusTooManyRequests, "login_throttled",
			"too many failed attempts; try again later")
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	user, ok := a.Accounts.Authenticate(body.Username, body.Password)
	if !ok {
		a.Sessions.RecordFailure(ip)
		return control.NewError(http.StatusUnauthorized, "invalid_credentials",
			"invalid username or password")
	}
	a.Sessions.ClearFailures(ip)
	token, err := a.Sessions.Create(user.Username)
	if err != nil {
		return err
	}
	snapshot := a.Config.Current()
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r, snapshot),
		MaxAge: int(snapshot.Auth.SessionTTL.Seconds()),
	})
	a.audit(r, user, "session.login", user.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "role": user.Role})
	return nil
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) error {
	actor, _ := a.guard.Actor(r)
	if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
		a.Sessions.Drop(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		// The clearing cookie must carry the same attributes as the one it replaces, or
		// the browser treats it as a different cookie and the session stays live.
		Name: auth.SessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: cookieSecure(r, a.Config.Current()),
		MaxAge: -1,
	})
	a.audit(r, actor, "session.logout", actor.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (a *API) me(w http.ResponseWriter, r *http.Request) error {
	if !a.Accounts.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_enabled": false, "authenticated": false,
		})
		return nil
	}
	user, found := a.guard.Actor(r)
	if !found {
		if !a.Config.Current().Auth.AnonRead {
			// The 401 body carries guest_available so the sign-in screen knows
			// whether to offer the guest button. The console cannot ask any other
			// way: every endpoint that could answer is itself behind this check.
			return control.NewErrorWithDetail(http.StatusUnauthorized,
				"authentication_required", map[string]any{
					"guest_available": a.guestAvailable(),
				}, "authentication required")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_enabled": true, "authenticated": false, "anon_read": true,
			"guest_available": a.guestAvailable(),
		})
		return nil
	}
	if auth.IsGuest(user) {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_enabled": true, "authenticated": true,
			"username": auth.GuestUser, "role": auth.RoleGuest,
			"guest": true, "readonly": true, "project": config.GlobalProject,
			"guest_available": true,
		})
		return nil
	}
	// Grants ride along with the identity because the console has to decide whether to
	// draw a project's controls before it has made any request against that project.
	// Without this it could only find out by pressing the button and reading the 403,
	// which is exactly the experience an access list is supposed to remove.
	grants, err := a.Accounts.GrantsFor(user.Username)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_enabled": true, "authenticated": true,
		"anon_read": a.Config.Current().Auth.AnonRead,
		"username":  user.Username, "role": user.Role,
		"reports_to": nullable(user.ReportsTo),
		"grants":     grants,
	})
	return nil
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireSuperuser(r); err != nil {
		return err
	}
	rows, err := a.DB.ListAudit(parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": list(rows)})
	return nil
}

var _ = eco.ListenerPathPrefixed
