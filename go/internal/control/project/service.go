// Package project owns tenant and upstream mutations plus live config publication.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/credential"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
)

var validName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

const globalFlag = "global_project"

// Service manages projects and republishes the live routing snapshot.
type Service struct {
	db      *control.DB
	config  *config.Store
	ecos    *eco.Registry
	metrics *obs.Metrics
	secrets *credential.Store
	global  control.Project
}

// New builds the project service and loads its persisted state.
func New(
	db *control.DB,
	cfg *config.Store,
	ecosystems *eco.Registry,
	secrets *credential.Store,
	metrics *obs.Metrics,
) (*Service, error) {
	service := &Service{
		db: db, config: cfg, ecos: ecosystems, metrics: metrics,
		secrets: secrets,
		global:  control.Project{Name: config.GlobalProject, DataPlaneAuth: "public"},
	}
	if raw, found, err := db.Flag(globalFlag); err != nil {
		return nil, err
	} else if found {
		if err := json.Unmarshal([]byte(raw), &service.global); err != nil {
			return nil, fmt.Errorf("project: decode global settings: %w", err)
		}
		service.global.Name = config.GlobalProject
	}
	// Snapshot-provided projects are startup seeds used by tests and migrations.
	for name, seeded := range cfg.Current().Projects {
		if name == config.GlobalProject {
			continue
		}
		if _, err := db.Project(name); err == nil {
			continue
		} else if !errors.Is(err, control.ErrNotFound) {
			return nil, err
		}
		if err := db.CreateProject(control.Project{
			Name: name, Owner: seeded.Owner, Offline: seeded.Offline,
			QuotaBytes: seeded.QuotaBytes, QuotaArtifacts: seeded.QuotaArtifacts,
			RateLimit: seeded.RateLimit, RateBurst: seeded.RateBurst,
			DataPlaneAuth: defaultAuth(seeded.DataPlaneAuth),
		}); err != nil {
			return nil, err
		}
	}
	if err := service.publish(); err != nil {
		return nil, err
	}
	return service, nil
}

// List returns global first, then named projects.
func (s *Service) List() ([]control.Project, error) {
	projects, err := s.db.ListProjects()
	if err != nil {
		return nil, err
	}
	return append([]control.Project{s.global}, projects...), nil
}

// Get resolves global or a named project.
func (s *Service) Get(name string) (control.Project, error) {
	if name == config.GlobalProject || name == "" {
		return s.global, nil
	}
	project, err := s.db.Project(name)
	if errors.Is(err, control.ErrNotFound) {
		return control.Project{}, control.NewError(http.StatusNotFound, "project_not_found",
			"no such project: %s", name)
	}
	return project, err
}

// Create registers a named project.
func (s *Service) Create(name, owner string) (control.Project, error) {
	if err := s.validateName(name); err != nil {
		return control.Project{}, err
	}
	if _, err := s.db.Project(name); err == nil {
		return control.Project{}, control.NewError(http.StatusConflict, "project_exists",
			"project already exists: %s", name)
	} else if !errors.Is(err, control.ErrNotFound) {
		return control.Project{}, err
	}
	project := control.Project{Name: name, Owner: owner, DataPlaneAuth: "public"}
	if err := s.db.CreateProject(project); err != nil {
		return control.Project{}, err
	}
	if err := s.publish(); err != nil {
		return control.Project{}, err
	}
	s.metrics.InitProjectSeries(name)
	return s.Get(name)
}

// Patch updates mutable settings.
type Patch struct {
	Owner          *string
	Offline        *bool
	QuotaBytes     *int64
	QuotaArtifacts *int64
	DataPlaneAuth  *string
	RateLimit      *int
	RateBurst      *int
}

// Update modifies a project and publishes the change immediately.
func (s *Service) Update(name string, patch Patch) (control.Project, error) {
	project, err := s.Get(name)
	if err != nil {
		return control.Project{}, err
	}
	if patch.Owner != nil {
		project.Owner = *patch.Owner
	}
	if patch.Offline != nil {
		project.Offline = *patch.Offline
	}
	if patch.QuotaBytes != nil {
		if *patch.QuotaBytes < 0 {
			return control.Project{}, invalid("quota_bytes cannot be negative")
		}
		project.QuotaBytes = *patch.QuotaBytes
	}
	if patch.QuotaArtifacts != nil {
		if *patch.QuotaArtifacts < 0 {
			return control.Project{}, invalid("quota_artifacts cannot be negative")
		}
		project.QuotaArtifacts = *patch.QuotaArtifacts
	}
	if patch.DataPlaneAuth != nil {
		if *patch.DataPlaneAuth != "public" && *patch.DataPlaneAuth != "token" {
			return control.Project{}, invalid("data_plane_auth must be public or token")
		}
		project.DataPlaneAuth = *patch.DataPlaneAuth
	}
	if patch.RateLimit != nil {
		if *patch.RateLimit < 0 {
			return control.Project{}, invalid("rate_limit cannot be negative")
		}
		project.RateLimit = *patch.RateLimit
	}
	if patch.RateBurst != nil {
		if *patch.RateBurst < 0 {
			return control.Project{}, invalid("rate_burst cannot be negative")
		}
		project.RateBurst = *patch.RateBurst
	}
	if name == config.GlobalProject {
		encoded, err := json.Marshal(project)
		if err != nil {
			return control.Project{}, err
		}
		if err := s.db.SetFlag(globalFlag, string(encoded)); err != nil {
			return control.Project{}, err
		}
		s.global = project
	} else if err := s.db.UpdateProject(project); err != nil {
		return control.Project{}, err
	}
	if err := s.publish(); err != nil {
		return control.Project{}, err
	}
	return project, nil
}

// Delete unregisters a project. The API removes its catalog roots first; immutable
// bytes remain for online GC so shared content is never removed eagerly.
func (s *Service) Delete(name string) error {
	if name == config.GlobalProject {
		return invalid("the global project cannot be deleted")
	}
	upstreams, err := s.db.ListUpstreams(name)
	if err != nil {
		return err
	}
	if err := s.db.DeleteProject(name); err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return control.NewError(http.StatusNotFound, "project_not_found",
				"no such project: %s", name)
		}
		return err
	}
	for _, upstream := range upstreams {
		if upstream.CredentialID != nil && s.secrets != nil {
			_ = s.secrets.Delete(*upstream.CredentialID)
		}
	}
	return s.publish()
}

// Upstreams returns a project's persisted overrides.
func (s *Service) Upstreams(project string) ([]control.Upstream, error) {
	if _, err := s.Get(project); err != nil {
		return nil, err
	}
	dbProject := project
	if project == config.GlobalProject {
		dbProject = ""
	}
	return s.db.ListUpstreams(dbProject)
}

// Upstream returns one project-scoped override.
func (s *Service) Upstream(project string, id int64) (control.Upstream, error) {
	if _, err := s.Get(project); err != nil {
		return control.Upstream{}, err
	}
	upstream, err := s.db.Upstream(dbProject(project), id)
	if err != nil {
		return control.Upstream{}, control.NewError(http.StatusNotFound,
			"upstream_not_found", "no such upstream: %d", id)
	}
	upstream.Project = project
	return upstream, nil
}

// AddUpstream creates a live override.
func (s *Service) AddUpstream(project string, upstream control.Upstream) (control.Upstream, error) {
	if _, err := s.Get(project); err != nil {
		return control.Upstream{}, err
	}
	if err := s.validateUpstream(&upstream); err != nil {
		return control.Upstream{}, err
	}
	upstream.Project = dbProject(project)
	id, err := s.db.CreateUpstream(upstream)
	if err != nil {
		return control.Upstream{}, err
	}
	upstream.ID = id
	if err := s.publish(); err != nil {
		return control.Upstream{}, err
	}
	upstream.Project = project
	return upstream, nil
}

// UpdateUpstream modifies a live override.
func (s *Service) UpdateUpstream(project string, upstream control.Upstream) (control.Upstream, error) {
	if _, err := s.Get(project); err != nil {
		return control.Upstream{}, err
	}
	if err := s.validateUpstream(&upstream); err != nil {
		return control.Upstream{}, err
	}
	upstream.Project = dbProject(project)
	if err := s.db.UpdateUpstream(upstream); err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return control.Upstream{}, control.NewError(http.StatusNotFound,
				"upstream_not_found", "no such upstream: %d", upstream.ID)
		}
		return control.Upstream{}, err
	}
	if err := s.publish(); err != nil {
		return control.Upstream{}, err
	}
	upstream.Project = project
	return upstream, nil
}

// DeleteUpstream removes an override.
func (s *Service) DeleteUpstream(project string, id int64) error {
	if _, err := s.Get(project); err != nil {
		return err
	}
	current, err := s.db.Upstream(dbProject(project), id)
	if err != nil {
		return control.NewError(http.StatusNotFound, "upstream_not_found",
			"no such upstream: %d", id)
	}
	if err := s.db.DeleteUpstream(dbProject(project), id); err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return control.NewError(http.StatusNotFound, "upstream_not_found",
				"no such upstream: %d", id)
		}
		return err
	}
	if current.CredentialID != nil && s.secrets != nil {
		_ = s.secrets.Delete(*current.CredentialID)
	}
	return s.publish()
}

func (s *Service) publish() error {
	stored, err := s.db.ListProjects()
	if err != nil {
		return err
	}
	projects := make(map[string]config.Project, len(stored)+1)
	projects[config.GlobalProject] = toConfig(s.global)
	for _, project := range stored {
		projects[project.Name] = toConfig(project)
	}
	rows, err := s.db.AllUpstreams()
	if err != nil {
		return err
	}
	overrides := make(map[string]map[string]map[string][]config.Endpoint)
	peers := make(map[string]map[string][]config.Peer)
	for _, upstream := range rows {
		if !upstream.Enabled {
			continue
		}
		project := upstream.Project
		if project == "" {
			project = config.GlobalProject
		}
		var plain config.UpstreamCredential
		if upstream.CredentialID != nil && s.secrets != nil {
			secret, err := s.secrets.Get(*upstream.CredentialID)
			if err != nil {
				return fmt.Errorf("project: unseal credential for %s/%s/%s: %w",
					project, upstream.Eco, upstream.Name, err)
			}
			plain = config.UpstreamCredential{
				Kind: secret.Kind, Username: secret.Username,
				Password: secret.Password, Token: secret.Token,
			}
		}
		if upstream.Kind == "peer" {
			if peers[project] == nil {
				peers[project] = make(map[string][]config.Peer)
			}
			peers[project][upstream.Eco] = append(peers[project][upstream.Eco], config.Peer{
				URL: upstream.URL, Priority: upstream.Priority, Credential: plain,
			})
			continue
		}
		if overrides[project] == nil {
			overrides[project] = make(map[string]map[string][]config.Endpoint)
		}
		if overrides[project][upstream.Eco] == nil {
			overrides[project][upstream.Eco] = make(map[string][]config.Endpoint)
		}
		// Appended rather than assigned. Two rows sharing a name are a chain — a team
		// cache and the registry behind it — and assigning let the last row read from
		// the database silently win, in whatever order it happened to arrive.
		overrides[project][upstream.Eco][upstream.Name] = append(
			overrides[project][upstream.Eco][upstream.Name],
			config.Endpoint{
				URL: upstream.URL, Priority: upstream.Priority, Credential: plain,
			})
	}
	sortChains(overrides)
	return s.config.SetControl(projects, overrides, peers)
}

// sortChains puts every chain in the order it will be tried: lowest priority first,
// then by URL so that a configuration always produces the same chain. Without the
// second key, two endpoints at the same priority would swap places between restarts
// and a cache would appear to change its mind about where it fetches from.
func sortChains(overrides map[string]map[string]map[string][]config.Endpoint) {
	for _, ecosystems := range overrides {
		for _, names := range ecosystems {
			for _, chain := range names {
				sort.SliceStable(chain, func(i, j int) bool {
					if chain[i].Priority != chain[j].Priority {
						return chain[i].Priority < chain[j].Priority
					}
					return chain[i].URL < chain[j].URL
				})
			}
		}
	}
}

func (s *Service) validateName(name string) error {
	if len(name) < 1 || len(name) > 40 || !validName.MatchString(name) {
		return invalid("project name must be 1–40 lowercase letters/digits separated by '.', '_' or '-'")
	}
	reserved := map[string]bool{
		config.GlobalProject: true, "root": true, "v2": true, "api": true,
		"healthz": true, "readyz": true, "metrics": true, "version": true,
	}
	for _, descriptor := range s.ecos.Descriptors() {
		reserved[descriptor.ID] = true
		for alias := range descriptor.DefaultUpstreams {
			reserved[alias] = true
		}
	}
	if reserved[name] {
		return invalid("%q is reserved", name)
	}
	// A project name may contain dots, and an OCI pull reads the segment after /v2/ as
	// a project before it reads it as a registry. A project called "gcr.io" would
	// therefore swallow every gcr.io pull on the instance. Discovery makes that a live
	// hazard rather than a theoretical one, so a name that reads as a registry host is
	// refused the same way an alias is.
	if _, isRegistry := ociname.Lookup(name); isRegistry {
		return invalid("%q reads as a registry host, which an image path resolves first", name)
	}
	return nil
}

func (s *Service) validateUpstream(upstream *control.Upstream) error {
	ecosystem, found := s.ecos.Get(upstream.Eco)
	if !found {
		return invalid("unknown ecosystem: %s", upstream.Eco)
	}
	if ecosystem.Descriptor().Upstreams == eco.UpstreamNone {
		return invalid("%s derives its upstream from requests", upstream.Eco)
	}
	upstream.Name = strings.Trim(upstream.Name, "/ ")
	if upstream.Name == "" || strings.Contains(upstream.Name, "..") {
		return invalid("upstream name is required")
	}
	parsed, err := url.Parse(upstream.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return invalid("upstream URL must be absolute HTTP or HTTPS")
	}
	upstream.URL = strings.TrimRight(upstream.URL, "/")
	if upstream.Kind == "" {
		upstream.Kind = "origin"
	}
	if upstream.Kind != "origin" && upstream.Kind != "peer" {
		return invalid("upstream kind must be origin or peer")
	}
	return nil
}

// Visible filters projects by actor relationship.
//
// The predicate receives the whole project rather than its owner: visibility can now
// also come from a grant, which is keyed on the project's name, and a callback that
// only ever saw the owner could not express that.
func (s *Service) Visible(
	projects []control.Project, canView func(project control.Project) bool,
) []control.Project {
	out := make([]control.Project, 0, len(projects))
	for _, project := range projects {
		if canView(project) {
			out = append(out, project)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == config.GlobalProject {
			return true
		}
		if out[j].Name == config.GlobalProject {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func toConfig(project control.Project) config.Project {
	return config.Project{
		Name: project.Name, Owner: project.Owner, Offline: project.Offline,
		QuotaBytes: project.QuotaBytes, QuotaArtifacts: project.QuotaArtifacts,
		RateLimit: project.RateLimit, RateBurst: project.RateBurst,
		DataPlaneAuth: defaultAuth(project.DataPlaneAuth),
	}
}

func defaultAuth(value string) string {
	if value == "" {
		return "public"
	}
	return value
}

func dbProject(project string) string {
	if project == config.GlobalProject {
		return ""
	}
	return project
}

func invalid(format string, args ...any) error {
	return control.NewError(http.StatusBadRequest, "invalid_request", format, args...)
}
