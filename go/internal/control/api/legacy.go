package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	controlproject "github.com/aabdlwahab/PKGCache/internal/control/project"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

func (a *API) legacyRoutes() {
	a.route("GET /api/projects", a.legacyProjects)
	a.route("POST /api/projects", a.legacyCreateProject)
	a.route("DELETE /api/projects/{project}", a.deleteProject)
	a.route("POST /api/projects/{project}/mode", a.legacyProjectMode)
	a.route("POST /api/projects/{project}/owner", a.legacyProjectOwner)
	a.route("GET /api/proxies", a.legacyProxies)
	a.route("GET /api/downloads", a.legacyDownloads)
	a.route("GET /api/recent", a.legacyRecent)
	a.route("GET /api/manifests", a.legacyManifests)
	a.route("GET /api/stats", a.legacyStats)
	a.route("GET /api/history", a.legacyHistory)
	a.route("GET /api/endpoints", a.legacyEndpoints)
	a.route("GET /api/shuttle", a.legacyShuttle)
	a.route("GET /api/packages", a.legacyPackages)
	a.route("GET /api/token", a.legacyTokenStatus)
	a.route("POST /api/token", a.legacyToken)
	a.route("GET /api/jobs", a.legacyJobs)
	a.route("POST /api/jobs", a.legacyStartJob)
	a.route("GET /api/jobs/{id}", a.legacyJob)
	a.route("GET /api/lockfile", a.legacyLockfile)
	a.route("GET /api/ca.crt", func(w http.ResponseWriter, _ *http.Request) error {
		return a.serveCA(w)
	})
	a.route("GET /api/setup.sh", a.getSetupShell)
	a.route("GET /api/setup.ps1", a.getSetupPowerShell)
	a.route("POST /api/artifacts", a.legacyPutArtifact)
	a.route("DELETE /api/artifacts", a.legacyDeleteArtifact)
	a.route("GET /api/me", a.me)
	a.route("POST /api/login", a.login)
	a.route("POST /api/logout", a.logout)
	a.route("GET /api/users", a.listUsers)
	a.route("POST /api/users", a.createUser)
	a.route("PATCH /api/users/{name}", a.patchUser)
	a.route("DELETE /api/users/{name}", a.deleteUser)
}

func (a *API) legacyCreateProject(w http.ResponseWriter, r *http.Request) error {
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
	a.audit(r, actor, "project.create", project.Name, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": project.Name, "ports": a.ports(),
		"repo":    pathUnder(a.DataDir, "projects/"+project.Name),
		"default": false, "offline": project.Offline, "owner": nullable(project.Owner),
	})
	return nil
}

func (a *API) legacyProjects(w http.ResponseWriter, r *http.Request) error {
	projects, err := a.Projects.List()
	if err != nil {
		return err
	}
	if a.Accounts.Enabled() {
		actor, found := a.guard.Actor(r)
		if !found {
			if !a.Config.Current().Auth.AnonRead {
				return control.NewError(http.StatusUnauthorized,
					"authentication_required", "authentication required")
			}
		} else if actor.Role != "superuser" {
			projects = a.Projects.Visible(projects, func(project control.Project) bool {
				return a.Accounts.CanViewOn(actor, project.Name, project.Owner)
			})
		}
	}
	rows := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		repo := a.DataDir
		if project.Name != config.GlobalProject {
			repo = pathUnder(a.DataDir, "projects/"+project.Name)
		}
		rows = append(rows, map[string]any{
			"name": project.Name, "ports": a.ports(), "repo": repo,
			"default": project.Name == config.GlobalProject,
			"offline": project.Offline, "owner": nullable(project.Owner),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": list(rows)})
	return nil
}

func (a *API) legacyProjectMode(w http.ResponseWriter, r *http.Request) error {
	name := projectName(r)
	_, actor, err := a.requireOperate(r, name)
	if err != nil {
		return err
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	if body.Target != "online" && body.Target != "offline" {
		return control.NewError(http.StatusBadRequest, "invalid_mode",
			"mode target must be online or offline")
	}
	offline := body.Target == "offline"
	project, err := a.Projects.Update(name,
		structProjectPatchOffline(offline))
	if err != nil {
		return err
	}
	a.audit(r, actor, "project.update", name, map[string]any{"offline": offline})
	writeJSON(w, http.StatusOK, map[string]any{"name": project.Name, "offline": project.Offline})
	return nil
}

func structProjectPatchOffline(value bool) controlproject.Patch {
	return controlproject.Patch{Offline: &value}
}

func (a *API) legacyProjectOwner(w http.ResponseWriter, r *http.Request) error {
	actor, err := a.guard.RequireSuperuser(r)
	if err != nil {
		return err
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if err := a.decode(r, &body); err != nil {
		return err
	}
	owner, found := a.Accounts.Get(body.Owner)
	if !found || (owner.Role != "admin" && owner.Role != "superuser") {
		return control.NewError(http.StatusBadRequest, "invalid_owner",
			"owner must be an existing admin or superuser")
	}
	project, err := a.Projects.Update(projectName(r),
		controlproject.Patch{Owner: &body.Owner})
	if err != nil {
		return err
	}
	a.audit(r, actor, "project.update", project.Name, map[string]any{"owner": body.Owner})
	writeJSON(w, http.StatusOK, map[string]any{"name": project.Name, "owner": project.Owner})
	return nil
}

func (a *API) legacyProxies(w http.ResponseWriter, r *http.Request) error {
	project, _, err := a.requireView(r, projectName(r))
	if err != nil {
		return err
	}
	live := a.Config.Current()
	projectOffline := live.OfflineFor(project.Name)
	roles := make([]map[string]any, 0, 6)
	for _, id := range legacyLiveEcosystems() {
		roles = append(roles, map[string]any{
			"role": id, "up": true, "offline": projectOffline,
		})
	}
	// This endpoint is a compatibility shim for the retired console. It modeled
	// two Python listeners even though the Go deployment is one process (and may
	// use one port), so retain those service rows and legacy ecosystem names.
	globalOffline := live.OfflineFor(config.GlobalProject)
	serviceStatus := map[bool]string{true: "offline", false: "online"}[globalOffline]
	services := []map[string]any{
		{"name": "pkgcache (unified port)", "state": "running", "status": serviceStatus},
		{"name": "pkgcache (apt proxy)", "state": "running", "status": serviceStatus},
	}
	hardOffline := live.Upstream.Offline
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "profile": map[bool]string{true: "offline", false: "online"}[hardOffline],
		"services": services, "project": project.Name, "roles": roles,
		"up": len(roles), "offline": projectOffline,
	})
	return nil
}

func (a *API) legacyDownloads(w http.ResponseWriter, r *http.Request) error {
	if _, _, err := a.requireView(r, projectName(r)); err != nil {
		return err
	}
	sources := make(map[string]any)
	for _, id := range legacyLiveEcosystems() {
		sources[id] = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": projectName(r), "sources": sources, "age": 0,
	})
	return nil
}

func (a *API) legacyRecent(w http.ResponseWriter, r *http.Request) error {
	if _, _, err := a.requireView(r, projectName(r)); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": projectName(r), "pulls": []any{},
	})
	return nil
}

func (a *API) legacyManifests(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	rows, _, err := a.Catalog.QueryArtifacts(catalog.ArtifactQuery{Project: project})
	if err != nil {
		return err
	}
	ecosystems := make(map[string][]map[string]any)
	for _, id := range legacyEcosystems() {
		ecosystems[id] = []map[string]any{}
	}
	for _, artifact := range rows {
		id := legacyEco(artifact.Eco)
		ecosystems[id] = append(ecosystems[id], artifactJSON(artifact))
	}
	count, bytes, err := a.Catalog.CountEntries(project)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project, "ecosystems": ecosystems,
		"checkpointed": map[string]int{}, "age": 0,
		"usage": map[string]any{"disk": map[string]int64{}, "disk_total": bytes,
			"docker_deduped": count},
	})
	return nil
}

func (a *API) legacyStats(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	result, err := a.Catalog.Stats(catalog.StatsQuery{Project: project})
	if err != nil {
		return err
	}
	var requests, hits, misses int64
	leaderboard := make(map[string][]catalog.PackageCount)
	byEco := make([]catalog.EcoStats, 0, len(result.ByEco))
	for _, row := range result.ByEco {
		row.Eco = legacyEco(row.Eco)
		byEco = append(byEco, row)
		requests += row.Requests
		hits += row.HitCount
		misses += row.MissCount
	}
	for _, row := range result.Leaderboard {
		row.Eco = legacyEco(row.Eco)
		leaderboard[row.Eco] = append(leaderboard[row.Eco], row)
	}
	var hitRate any
	if hits+misses > 0 {
		hitRate = float64(hits) / float64(hits+misses)
	}
	topLargest := make([]map[string]any, 0, len(result.TopLargest))
	for _, artifact := range result.TopLargest {
		item := artifactJSON(artifact)
		item["eco"] = legacyEco(artifact.Eco)
		topLargest = append(topLargest, item)
	}
	recentAdded := make([]map[string]any, 0, len(result.RecentAdded))
	for _, artifact := range result.RecentAdded {
		item := artifactJSON(artifact)
		item["eco"] = legacyEco(artifact.Eco)
		recentAdded = append(recentAdded, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project,
		"totals": map[string]any{"packages": result.TotalBlobs, "size": result.TotalBytes,
			"requests": requests, "hits": hits, "misses": misses},
		"hit_rate": hitRate, "bytes_saved": int64(0), "time_saved_seconds": 0,
		"by_eco": byEco, "by_arch": []any{}, "leaderboard": leaderboard,
		"top_largest": topLargest, "recent_added": recentAdded,
		"bandwidth": map[string]any{"current_bps": 0, "samples": []any{}},
	})
	return nil
}

func (a *API) legacyHistory(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	rows, err := a.Catalog.ListSnapshots(project, 100)
	if err != nil {
		return err
	}
	head, err := a.Catalog.GetHead(project)
	if err != nil {
		return err
	}
	commits := make([]map[string]any, 0, len(rows))
	for _, snapshot := range rows {
		short := snapshot.ID
		if len(short) > 12 {
			short = short[:12]
		}
		commits = append(commits, map[string]any{
			"hash": snapshot.ID, "short": short, "date": snapshot.CreatedAt,
			"subject": snapshot.Subject, "is_checkpoint": true,
			"is_head": snapshot.ID == head,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": head, "commits": commits})
	return nil
}

func (a *API) legacyEndpoints(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a.legacyEndpointMap(project))
	return nil
}

func (a *API) legacyShuttle(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, projectErr := a.Projects.Get(project); projectErr == nil {
		if _, _, err := a.requireView(r, project); err != nil {
			return err
		}
	} else if !isProjectNotFound(projectErr) {
		return projectErr
	} else if _, err := a.guard.RequireCreate(r); err != nil {
		return err
	}
	inDir := pathUnder(a.DataDir, "shuttle/in")
	checkpoints := make([]any, 0)
	entries, _ := os.ReadDir(inDir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}
		file, err := os.Open(filepath.Join(inDir, entry.Name()))
		if err != nil {
			continue
		}
		pack, inspectErr := snapshot.InspectPack(file, project)
		_ = file.Close()
		if inspectErr != nil {
			continue
		}
		for _, checkpoint := range pack.Snapshots {
			checkpoints = append(checkpoints, map[string]any{
				"hash": checkpoint.ID, "short": shortSnapshotID(checkpoint.ID),
				"date": checkpoint.CreatedAt, "subject": checkpoint.Subject,
				"file": entry.Name(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project, "export_dir": pathUnder(a.DataDir, "shuttle/out"),
		"import_dir": inDir, "import_ready": len(checkpoints) > 0,
		"import_checkpoints": checkpoints,
	})
	return nil
}

func (a *API) legacyPackages(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	query := r.URL.Query()
	sortBy := query.Get("sort")
	if sortBy == "" {
		sortBy = "name"
	}
	rows, _, err := a.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: project, Eco: query.Get("eco"), Search: query.Get("q"),
		Sort: sortBy, Page: parseInt(query.Get("page"), 1), PageSize: 1000,
	})
	if err != nil {
		return err
	}
	ecosystems := make(map[string][]map[string]any)
	for _, artifact := range rows {
		id := legacyEco(artifact.Eco)
		ecosystems[id] = append(ecosystems[id], artifactJSON(artifact))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": project, "ecosystems": ecosystems,
		"page": parseInt(query.Get("page"), 1), "sort": sortBy,
	})
	return nil
}

func (a *API) legacyTokenStatus(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	if _, _, err := a.requireView(r, project); err != nil {
		return err
	}
	rows, err := a.Tokens.List(project)
	if err != nil {
		return err
	}
	set := false
	for _, token := range rows {
		if token.Eco == "files" && token.Scope == "write" {
			set = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"set": set})
	return nil
}

func (a *API) legacyToken(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Project string `json:"project"`
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
	token, secret, err := a.Tokens.Issue(body.Project, "files", "write",
		"legacy files write token", actor.Username, 0)
	if err != nil {
		return err
	}
	a.audit(r, actor, "token.create", token.ID,
		map[string]any{"project": body.Project, "eco": "files", "scope": "write"})
	writeJSON(w, http.StatusOK, map[string]any{"token": secret})
	return nil
}

func (a *API) legacyJobs(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	rows, err := a.Jobs.List(100)
	if err != nil {
		return err
	}
	summary := make([]map[string]any, 0, len(rows))
	busy := false
	for _, row := range rows {
		if row.Status == "running" || row.Status == "queued" {
			busy = true
		}
		summary = append(summary, map[string]any{
			"id": row.ID, "action": row.Action, "status": row.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"busy": busy, "jobs": list(summary)})
	return nil
}

func (a *API) legacyStartJob(w http.ResponseWriter, r *http.Request) error {
	var body map[string]any
	if err := a.decode(r, &body); err != nil {
		return err
	}
	action, _ := body["action"].(string)
	delete(body, "action")
	project, _ := body["project"].(string)
	if project == "" {
		project = config.GlobalProject
	}
	var actor control.User
	var err error
	switch action {
	case "mode":
		actor, err = a.guard.RequireSuperuser(r)
	case "import":
		if _, projectErr := a.Projects.Get(project); projectErr == nil {
			_, actor, err = a.requireOperate(r, project)
		} else if !isProjectNotFound(projectErr) {
			err = projectErr
		} else {
			actor, err = a.guard.RequireCreate(r)
		}
	case "lockwarm":
		_, actor, err = a.requireView(r, project)
	default:
		_, actor, err = a.requireOperate(r, project)
	}
	if err != nil {
		return err
	}
	record, err := a.Jobs.Submit(project, action, actor.Username, body)
	if err != nil {
		return err
	}
	a.audit(r, actor, "job.create", strconv.FormatInt(record.ID, 10),
		map[string]any{"project": project, "action": action})
	writeJSON(w, http.StatusOK, map[string]any{"id": record.ID})
	return nil
}

func (a *API) legacyJob(w http.ResponseWriter, r *http.Request) error {
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
	offset := parseInt(r.URL.Query().Get("offset"), 0)
	if offset < 0 || offset > len(record.Log) {
		offset = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": record.ID, "action": record.Action, "status": record.Status,
		"log": record.Log[offset:], "offset": len(record.Log),
	})
	return nil
}

func (a *API) legacyLockfile(w http.ResponseWriter, r *http.Request) error {
	if _, _, err := a.requireView(r, projectName(r)); err != nil {
		return err
	}
	path := filepath.Join(a.DataDir, "lockwarm", projectName(r), "uv.lock")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return control.NewError(http.StatusNotFound, "not_found",
			"no rewritten lock for this project yet")
	} else if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="uv.lock"`)
	http.ServeFile(w, r, path)
	return nil
}

func (a *API) legacyPutArtifact(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	_, actor, err := a.requireView(r, project)
	if err != nil {
		return err
	}
	key := strings.Trim(r.URL.Query().Get("path"), "/")
	if key == "" || strings.Contains(key, "..") {
		return control.NewError(http.StatusBadRequest, "invalid_path", "artifact path is required")
	}
	result, err := a.Engine.Put(project, "files", key, r.Body, engine.PutOptions{
		MediaType: r.Header.Get("Content-Type"),
		Overwrite: boolParam(r.URL.Query().Get("overwrite")),
		Artifact:  &catalog.Artifact{Name: key},
		Origin:    "console:" + a.guard.ClientIP(r),
	})
	if errors.Is(err, engine.ErrExists) {
		return control.NewError(http.StatusConflict, "exists", "%s", err.Error())
	}
	if err != nil {
		return err
	}
	a.audit(r, actor, "artifact.put", project+"/files/"+key,
		map[string]any{"size": result.Size, "sha256": result.Digest})
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"path": key, "size": result.Size, "sha256": result.Digest,
		"url": fmt.Sprintf("/%s/files/%s", project, key),
	})
	return nil
}

func (a *API) legacyDeleteArtifact(w http.ResponseWriter, r *http.Request) error {
	project := projectName(r)
	_, actor, err := a.requireOperate(r, project)
	if err != nil {
		return err
	}
	key := strings.Trim(r.URL.Query().Get("path"), "/")
	if key == "" || strings.Contains(key, "..") {
		return control.NewError(http.StatusBadRequest, "invalid_path", "artifact path is required")
	}
	if err := a.Engine.DeleteEntry(project, "files", key); err != nil {
		return err
	}
	_ = a.Engine.DeleteArtifacts(project, "files", key)
	a.audit(r, actor, "artifact.delete", project+"/files/"+key, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func legacyEcosystems() []string {
	return []string{"docker", "npm", "pip", "apt", "apk", "git", "files"}
}

func legacyLiveEcosystems() []string {
	// apk shares apt's forward-proxy health/download feed in the retired stack
	// and therefore was not a separate live-source row.
	return []string{"docker", "npm", "pip", "apt", "git", "files"}
}

func legacyEco(id string) string {
	switch id {
	case "oci":
		return "docker"
	case "pypi":
		return "pip"
	default:
		return id
	}
}

func (a *API) legacyEndpointMap(project string) map[string]any {
	live := a.Config.Current()
	unified := addressPort(live.Server.UnifiedAddr)
	proxy := addressPort(live.Server.ProxyAddr)
	if live.Server.SinglePort {
		proxy = unified
	}
	imageProject := ""
	proxyUser := ""
	if project != config.GlobalProject {
		imageProject = project + "/"
		proxyUser = project + "@"
	}
	ca := "/path/to/ca.crt"
	aptNote := "plain-HTTP forward proxy; keep http:// mirror lines"
	if project != config.GlobalProject {
		aptNote += fmt.Sprintf("; the '%s@' username selects this project", project)
	}
	return map[string]any{
		"docker": map[string]any{
			"url":  fmt.Sprintf("<host>:%d/%sdockerhub/<image>", unified, imageProject),
			"note": "upstreams: dockerhub | ghcr | quay; Docker Hub official images live under library/",
			"setup": []string{
				"# trust the cache's CA for this registry (one-time per host):",
				fmt.Sprintf("sudo mkdir -p /etc/docker/certs.d/<host>:%d", unified),
				fmt.Sprintf("sudo cp ca.crt /etc/docker/certs.d/<host>:%d/ca.crt", unified),
				"# then pull through the cache:",
				fmt.Sprintf("docker pull <host>:%d/%sdockerhub/library/alpine:3.20", unified, imageProject),
				fmt.Sprintf("docker pull <host>:%d/%sghcr/<org>/<image>:<tag>", unified, imageProject),
			},
		},
		"npm": map[string]any{
			"url":  fmt.Sprintf("https://<host>:%d/%s/npm/", unified, project),
			"note": "set once with npm config, or per-install with --registry",
			"setup": []string{
				fmt.Sprintf("npm config set registry https://<host>:%d/%s/npm/", unified, project),
				"npm config set cafile " + ca,
				"npm install <pkg>",
			},
		},
		"pip": map[string]any{
			"url":  fmt.Sprintf("https://<host>:%d/%s/pypi/root/pypi/+simple/", unified, project),
			"note": "other indexes: root/pytorch-cu124, root/pytorch-cpu, … (see /+indexes)",
			"setup": []string{
				fmt.Sprintf("pip install --index-url https://<host>:%d/%s/pypi/root/pypi/+simple/ --cert %s <pkg>", unified, project, ca),
				"# uv:",
				fmt.Sprintf("UV_INDEX_URL=https://<host>:%d/%s/pypi/root/pypi/+simple/ SSL_CERT_FILE=%s uv pip install <pkg>", unified, project, ca),
				"# or persist in ~/.config/pip/pip.conf:  index-url = … / cert = " + ca,
			},
		},
		"apt": map[string]any{
			"url":  fmt.Sprintf("http://%s<host>:%d/", proxyUser, proxy),
			"note": aptNote,
			"setup": []string{
				fmt.Sprintf("echo 'Acquire::http::Proxy \"http://%s<host>:%d\";' | sudo tee /etc/apt/apt.conf.d/01proxy", proxyUser, proxy),
				"sudo apt-get update && sudo apt-get install -y <pkg>",
			},
		},
		"apk": map[string]any{
			"url":  fmt.Sprintf("http://%s<host>:%d/", proxyUser, proxy),
			"note": "same proxy as apt; switch /etc/apk/repositories to http:// first",
			"setup": []string{
				"sed -i 's/https/http/' /etc/apk/repositories",
				fmt.Sprintf("http_proxy=http://%s<host>:%d apk add --no-cache <pkg>", proxyUser, proxy),
			},
		},
		"git": map[string]any{
			"url":  fmt.Sprintf("https://<host>:%d/%s/git/<upstream-host>/<owner>/<repo>.git", unified, project),
			"note": "read-only mirror-and-serve; the real upstream host goes in the path",
			"setup": []string{
				fmt.Sprintf("git config --global http.\"https://<host>:%d/\".sslCAInfo %s", unified, ca),
				"# transparent adoption (covers submodules, pip git+https, CPM, …):",
				fmt.Sprintf("git config --global url.\"https://<host>:%d/%s/git/github.com/\".insteadOf \"https://github.com/\"", unified, project),
				"# or clone explicitly:",
				fmt.Sprintf("git clone https://<host>:%d/%s/git/github.com/<owner>/<repo>.git", unified, project),
			},
		},
		"files": map[string]any{
			"url":  fmt.Sprintf("https://<host>:%d/%s/files/<path>", unified, project),
			"note": "generic artifacts: anonymous GET; PUT/DELETE need this project's write token",
			"setup": []string{
				fmt.Sprintf("wget --ca-certificate=ca.crt https://<host>:%d/%s/files/<path>", unified, project),
				"# upload (token from the console's Connect page):",
				"curl --cacert ca.crt -T <file> -H \"Authorization: Bearer $TOKEN\" \\",
				fmt.Sprintf("     https://<host>:%d/%s/files/<path>", unified, project),
			},
		},
	}
}
