// Package api serves the versioned control API, SSE stream and legacy console shim.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/auth"
	"github.com/aabdlwahab/PKGCache/internal/control/credential"
	"github.com/aabdlwahab/PKGCache/internal/control/job"
	controlproject "github.com/aabdlwahab/PKGCache/internal/control/project"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/maintenance"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/onboarding"
)

// Options supplies the API's in-process collaborators.
type Options struct {
	DB          *control.DB
	Config      *config.Store
	Accounts    *auth.Accounts
	Sessions    *auth.Sessions
	Tokens      *auth.Tokens
	Credentials *credential.Store
	Projects    *controlproject.Service
	Jobs        *job.Manager
	Catalog     *catalog.DB
	// Budget, when set, reports the disk policy of a cache that has one, for the
	// storage figures every stats reader already asks for.
	//
	// A function rather than a value because it is sampled live: whether a cache is full
	// changes while the process runs. Set by pkgcache's daemon after Open; a server
	// leaves it nil and its responses are unchanged.
	Budget func() (LocalBudget, bool)
	// Maintenance runs collection and removal. Present because removing named content is
	// synchronous — a handful of digests somebody selected, not a sweep worth queueing —
	// while gc and evict stay jobs.
	Maintenance *maintenance.Service
	// Sources, when set, exposes the per-project upstream configuration of a cache that
	// keeps one of its own. It is pkgcache's: the trust pin, the team cache and the chain
	// rewrite live above this package and cannot be imported from it, so they are handed
	// down. A server leaves it nil and the routes are not registered at all.
	Sources LocalSources
	Engine  *engine.Engine
	Ecos    *eco.Registry
	Events  *obs.Bus
	DataDir string
	CAFile  string
	// Log records the errors this package answers 500 for. Without it they were written
	// nowhere at all: the caller was told "internal server error" and the cause was
	// discarded, which is the one failure mode a control plane cannot afford to have.
	Log *slog.Logger
}

// API is the complete control HTTP surface.
type API struct {
	Options
	guard *auth.Guard
	mux   *http.ServeMux
	// closing releases the streaming handlers when the process is shutting down.
	//
	// http.Server.Shutdown waits for active handlers, and an event stream never finishes
	// on its own — so a single open widget window made every `pkgcache stop`, every
	// `setup` and the idle exit itself burn the whole shutdown grace period before the
	// daemon would go. Closed once, by Close.
	closing   chan struct{}
	closeOnce sync.Once
	// patterns records every registered route, in registration order. It exists so a
	// test can check the guest allowlist against reality rather than against a second
	// hand-maintained copy of it.
	patterns []string
}

// RegisteredRoutes returns every route pattern this API serves.
func RegisteredRoutes() []string {
	return New(Options{Config: config.NewStore(ptr(config.Defaults()))}).patterns
}

func ptr[T any](value T) *T { return &value }

type handler func(http.ResponseWriter, *http.Request) error

// New builds the route table.
func New(options Options) *API {
	a := &API{Options: options, closing: make(chan struct{})}
	a.guard = &auth.Guard{
		Accounts: options.Accounts, Sessions: options.Sessions, Config: options.Config,
	}
	a.mux = http.NewServeMux()
	a.v1Routes()
	a.seriesRoutes()
	a.downloadRoutes()
	a.legacyRoutes()
	return a
}

// Close releases the streaming handlers so a shutdown does not have to wait them out.
//
// Called before http.Server.Shutdown, not instead of it: this ends the streams, and
// Shutdown still drains the ordinary requests. Idempotent, because both the listener
// runtime and a test may call it.
func (a *API) Close() {
	a.closeOnce.Do(func() { close(a.closing) })
}

// ServeHTTP dispatches a control request.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// route registers one control endpoint. Every cross-cutting rule that must hold for
// the whole surface belongs here rather than in the handlers, because a rule enforced
// per handler is a rule that a new handler can be written without.
func (a *API) route(pattern string, fn handler) {
	a.patterns = append(a.patterns, pattern)
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if err := a.guard.CheckOrigin(r); err != nil {
			a.writeError(w, r, err)
			return
		}
		// Guest confinement is default-deny and keyed on this pattern, so a route
		// added later is closed to guests until someone lists it. See guest.go.
		scoped, err := a.guestGate(pattern, r)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if err := fn(w, scoped); err != nil {
			a.writeError(w, r, err)
		}
	})
}

func (a *API) decode(r *http.Request, value any) error {
	maxBytes := a.Config.Current().Auth.MaxJSONBytes
	reader := io.LimitReader(r.Body, maxBytes+1)
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(value); err != nil {
		return control.NewError(http.StatusBadRequest, "invalid_json", "invalid JSON body: %v", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return control.NewError(http.StatusBadRequest, "invalid_json", "JSON body must contain one value")
	}
	return nil
}

func (a *API) project(r *http.Request, name string) (control.Project, error) {
	if name == "" {
		name = config.GlobalProject
	}
	return a.Projects.Get(name)
}

func (a *API) requireView(r *http.Request, name string) (control.Project, control.User, error) {
	project, err := a.project(r, name)
	if err != nil {
		return control.Project{}, control.User{}, err
	}
	// RequireViewOf rather than RequireView: a guest's permission is defined by the
	// project's name, and only this caller knows it. The route allowlist already
	// confines guests to the global project; this is the second layer, so a route
	// that is on the allowlist and reads a project some other way is still bounded.
	actor, err := a.guard.RequireViewOf(r, project.Name, project.Owner)
	return project, actor, err
}

func (a *API) requireOperate(r *http.Request, name string) (control.Project, control.User, error) {
	project, err := a.project(r, name)
	if err != nil {
		return control.Project{}, control.User{}, err
	}
	actor, err := a.guard.RequireOperate(r, project.Name, project.Owner)
	return project, actor, err
}

// requireOwner is requireOperate without the grant path: superuser, or the project's
// own owner, and nobody else.
//
// A grant says "you may work on this project". Removing the project is not working on
// it, and neither is changing who else may reach it — a shared operator who could do
// either would be able to lock the owner out of their own tenant with one request.
func (a *API) requireOwner(r *http.Request, name string) (control.Project, control.User, error) {
	project, err := a.project(r, name)
	if err != nil {
		return control.Project{}, control.User{}, err
	}
	// With no accounts there is nobody to be the owner and nobody to keep out, which is
	// what every guard in auth.Guard already says for itself. This one has to say it
	// here because it funnels through RequireUser, and RequireUser is deliberately the
	// single guard with no such branch: it is what refuses a guest session, and a guest
	// exists only where accounts do.
	//
	// Without this, a cache with no accounts — pkgcache on a laptop — can create a
	// project through this API and never delete one, from the console or the command
	// line. Grants stay refused regardless: canManageGrants checks the actor again and
	// an empty actor owns nothing.
	if !a.Accounts.Enabled() {
		return project, control.User{}, nil
	}
	actor, err := a.guard.RequireUser(r)
	if err != nil {
		return control.Project{}, control.User{}, err
	}
	if !a.Accounts.CanOperate(actor, project.Owner) {
		return control.Project{}, control.User{}, control.NewError(http.StatusForbidden,
			"forbidden", "only a superuser or this project's owner can do this")
	}
	return project, actor, nil
}

func (a *API) audit(r *http.Request, actor control.User, action, target string, detail map[string]any) {
	record := control.AuditRecord{
		Actor: actor.Username, Action: action, Target: target,
		Detail: detail, ClientIP: a.guard.ClientIP(r),
	}
	id, err := a.DB.AppendAudit(record)
	if err != nil {
		return
	}
	a.Events.Publish(obs.Event{
		Kind: obs.EventAudit, ID: strconv.FormatInt(id, 10), Name: action,
		Detail: target,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// list guarantees a JSON array rather than null.
//
// Go marshals a nil slice as `null`, so "no rows yet" and "this field is absent" left
// the wire looking identical — and every consumer had to defend against a shape that
// only ever means empty. A fresh instance served {"jobs":null}, which is exactly the
// state nobody tests against because every fixture starts by creating something.
func list[T any](rows []T) []T {
	if rows == nil {
		return []T{}
	}
	return rows
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var client *control.Error
	if errors.As(err, &client) {
		body := map[string]any{"error": client.Message, "code": client.Code}
		// Detail is merged rather than nested so a client reads one flat object,
		// and it cannot overwrite error or code: a refusal must never be able to
		// describe itself as something else.
		for key, value := range client.Detail {
			if key != "error" && key != "code" {
				body[key] = value
			}
		}
		writeJSON(w, client.Status, body)
		return
	}
	if errors.Is(err, control.ErrNotFound) || errors.Is(err, catalog.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": err.Error(), "code": "not_found",
		})
		return
	}
	// Logged, not returned: the message may name a path, a host or a query, and a control
	// plane that hands those to an unauthenticated caller is a different bug. The operator
	// gets the cause; the caller gets the status.
	if a.Log != nil {
		a.Log.Error("control plane request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "internal server error", "code": "internal",
	})
}

func projectName(r *http.Request) string {
	name := r.PathValue("project")
	if name == "" {
		name = r.URL.Query().Get("project")
	}
	if name == "" {
		name = config.GlobalProject
	}
	return name
}

func actorName(user control.User) string { return user.Username }

func addressPort(address string) int {
	_, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(rawPort)
	return port
}

func (a *API) ports() map[string]int {
	snapshot := a.Config.Current()
	unified := addressPort(snapshot.Server.UnifiedAddr)
	proxy := addressPort(snapshot.Server.ProxyAddr)
	if snapshot.Server.SinglePort {
		proxy = unified
	}
	out := make(map[string]int)
	for _, descriptor := range a.Ecos.Descriptors() {
		if descriptor.Listener == eco.ListenerForwardProxy {
			out[descriptor.ID] = proxy
		} else {
			out[descriptor.ID] = unified
		}
	}
	return out
}

func (a *API) endpointMap(
	project, host string,
	unifiedPort, proxyPort int,
) map[string]any {
	out := make(map[string]any)
	for _, descriptor := range a.Ecos.Descriptors() {
		ecoPort := unifiedPort
		if descriptor.Listener == eco.ListenerForwardProxy {
			ecoPort = proxyPort
		}
		steps := descriptor.SetupSteps(eco.SetupContext{
			Host: host, Port: ecoPort, Project: project,
			IsGlobal: project == config.GlobalProject,
		})
		out[descriptor.ID] = map[string]any{
			"setup": steps, "listener": descriptor.Listener,
		}
	}
	return out
}

func artifactJSON(artifact catalog.Artifact) map[string]any {
	return map[string]any{
		"project": artifact.Project, "eco": artifact.Eco, "name": artifact.Name,
		"version": artifact.Version, "arch": artifact.Arch, "digest": artifact.Digest,
		"size": artifact.Size, "origin": artifact.Origin, "cached_at": artifact.CachedAt,
		"extra": artifact.Extra,
	}
}

func snapshotJSON(snapshot catalog.Snapshot) map[string]any {
	return map[string]any{
		"id": snapshot.ID, "project": snapshot.Project, "parent": snapshot.Parent,
		"manifest_sha256": snapshot.Manifest, "entry_count": snapshot.EntryCount,
		"total_bytes": snapshot.TotalBytes, "created_at": snapshot.CreatedAt,
		"subject": snapshot.Subject, "author": snapshot.Author,
	}
}

func (a *API) projectJSON(project control.Project) map[string]any {
	return map[string]any{
		"name": project.Name, "owner": nullable(project.Owner),
		"created_at": project.CreatedAt, "offline": project.Offline,
		"quota_bytes": project.QuotaBytes, "quota_artifacts": project.QuotaArtifacts,
		"data_plane_auth": project.DataPlaneAuth,
		"rate_limit":      project.RateLimit, "rate_burst": project.RateBurst,
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, control.NewError(http.StatusBadRequest, "invalid_id", "invalid numeric id")
	}
	return parsed, nil
}

func (a *API) serveCA(w http.ResponseWriter) error {
	payload, err := a.readCA()
	if err != nil {
		return err
	}
	fingerprint, err := onboarding.FingerprintSHA256(payload)
	if err != nil {
		return fmt.Errorf("fingerprint CA certificate: %w", err)
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="pkgreg-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Pkgreg-CA-SHA256", fingerprint)
	_, err = w.Write(payload)
	return err
}

func boolParam(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func pathUnder(root, child string) string { return filepath.Join(root, child) }

func shortSnapshotID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func isProjectNotFound(err error) bool {
	var clientErr *control.Error
	return errors.As(err, &clientErr) && clientErr.Code == "project_not_found"
}
