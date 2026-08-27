package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/app"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/trust"
)

// The middle tier: a team's cache, sitting between this machine and the registries.
//
// It is expressed as an ordinary upstream chain rather than as a mode. Configuring a
// team cache writes two rows per index — the team at priority 10 and the public
// registry at 20 — and everything after that is the engine walking a chain it already
// knows how to walk. There is no "team cache" concept anywhere below this file, which
// is what keeps the fallback behaviour identical to any other chain and testable the
// same way.
//
// Configuration is per local project, because that is what a project on a laptop is
// for: work resolves through the company's cache while a side project goes straight to
// the public registry. The global project's configuration is the fallback for any
// project without one — a machine pointed at a team cache should route everything
// through it until told otherwise — and the *remote* project name is inherited
// verbatim rather than derived from the local one, because deriving it would silently
// create tenants on somebody else's server.

// Team is the cache this machine asks before it asks the internet.
type Team struct {
	// Server is the origin, for example https://cache.internal:8443.
	Server string `json:"server"`
	// Fingerprint is the CA's SHA-256, kept so `status` can show what was pinned.
	Fingerprint string `json:"fingerprint"`
	// Project is the project name on the team's side, which need not match the local
	// one this configuration is filed under.
	Project string `json:"project"`
	// Direct records whether the public registry is in the chain behind the team cache.
	// False is `setup -no-direct`: a machine that must never reach a registry itself.
	Direct bool `json:"direct"`
	// CAPEM is the verified CA, kept here rather than only in a file so that the trust
	// bundle every outbound request uses is derivable from this record alone. Two team
	// caches mean two CAs, one pool, and no orphaned file when one is removed.
	CAPEM string `json:"ca_pem,omitempty"`
	// NoCache turns local caching off entirely, leaving a verified loopback bridge to
	// the team cache and nothing else — which is exactly what pkgreg-client did.
	//
	// It is a promise as much as a setting: in this mode no store is opened, no
	// database is created and nothing is written to the cache directory, so the merged
	// binary costs an existing client's users its size and nothing else.
	NoCache bool `json:"no_cache,omitempty"`
}

func teamPath(dataDir string) string { return filepath.Join(dataDir, "team.json") }

// TeamCAPath is where the trust bundle is kept, and is what upstream.ca_file points at.
//
// A bundle rather than a certificate: two projects may reach two different team caches,
// each with its own self-minted CA, and the outbound pool has one root pool. It is
// rewritten from the stored records on every write, so it never holds a CA no team
// still uses.
func TeamCAPath(dataDir string) string { return filepath.Join(dataDir, "team-ca.crt") }

// TeamSet is the team configuration for each local project.
type TeamSet struct {
	// Projects is keyed by the local project name. An entry for the global project is
	// the fallback for every project without one of its own.
	Projects map[string]Team `json:"projects"`
}

// teamFile is what is on disk, and it accepts both shapes.
//
// The embedded Team is the single-team document written before projects existed here.
// Reading it costs four lines and means a cache configured yesterday keeps working; it
// is never written back, so a file is upgraded the first time setup touches it.
type teamFile struct {
	Projects map[string]Team `json:"projects"`
	Team
}

// For resolves the team cache a local project asks, falling back to the global
// project's. The second return is whether there is one at all.
func (s TeamSet) For(project string) (Team, bool) {
	if team, found := s.Projects[project]; found && team.Server != "" {
		return team, true
	}
	if team, found := s.Projects[config.GlobalProject]; found && team.Server != "" {
		return team, true
	}
	return Team{}, false
}

// Own reports the configuration filed under exactly this project, with no fallback.
// `setup` needs it to tell "inherited" from "chosen", which are different things to
// print and different things to remove.
func (s TeamSet) Own(project string) (Team, bool) {
	team, found := s.Projects[project]
	return team, found && team.Server != ""
}

// Set files a team cache under a local project.
func (s *TeamSet) Set(project string, team Team) {
	if s.Projects == nil {
		s.Projects = map[string]Team{}
	}
	s.Projects[project] = team
}

// Remove forgets one project's own configuration. It may still inherit the global
// project's, which is the correct outcome: removing a project's override is not the
// same as excusing it from the machine's default.
func (s *TeamSet) Remove(project string) { delete(s.Projects, project) }

// Any reports whether any project has a team cache.
func (s TeamSet) Any() bool {
	for _, team := range s.Projects {
		if team.Server != "" {
			return true
		}
	}
	return false
}

// Bridged returns the machine-wide bridge-only configuration, if that is what this is.
//
// -no-cache is a property of the machine and not of a project: it promises that no
// store is opened and no database created, which cannot be true for one project and
// false for another. setup refuses to file it under anything but the global project,
// and this is the only reader that matters.
func (s TeamSet) Bridged() (Team, bool) {
	team, found := s.Projects[config.GlobalProject]
	if found && team.Server != "" && team.NoCache {
		return team, true
	}
	return Team{}, false
}

// ReadTeams returns the team configuration for every project.
func ReadTeams(dataDir string) (TeamSet, error) {
	data, err := os.ReadFile(teamPath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TeamSet{}, nil
		}
		return TeamSet{}, fmt.Errorf("local: read team cache settings: %w", err)
	}
	var file teamFile
	if err := json.Unmarshal(data, &file); err != nil {
		// Treated as absent, as everywhere else in this package: a corrupt preference
		// file should cost the preference, not the command.
		return TeamSet{}, nil //nolint:nilerr // a corrupt preference is not an error
	}
	set := TeamSet{Projects: file.Projects}
	if len(set.Projects) == 0 && file.Server != "" {
		set.Projects = map[string]Team{config.GlobalProject: file.Team}
	}
	// A pre-projects cache kept its CA in the file the pool reads. Adopt it, so the
	// bundle stays derivable from these records from here on.
	adoptLegacyCA(dataDir, set)
	return set, nil
}

func adoptLegacyCA(dataDir string, set TeamSet) {
	team, found := set.Projects[config.GlobalProject]
	if !found || team.Server == "" || team.CAPEM != "" {
		return
	}
	caPEM, err := os.ReadFile(TeamCAPath(dataDir))
	if err != nil || len(caPEM) == 0 {
		return
	}
	team.CAPEM = string(caPEM)
	set.Projects[config.GlobalProject] = team
}

// WriteTeams records the configuration and rewrites the trust bundle to match.
//
// One function for both, deliberately: a team.json naming a cache whose CA is not in
// the bundle is a machine that cannot reach its own middle tier, and that is exactly
// the state two separate writes would leave behind if the second one failed.
func WriteTeams(dataDir string, set TeamSet) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(TeamSet{Projects: set.Projects}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(teamPath(dataDir), append(data, '\n'), 0o600); err != nil {
		return err
	}
	return writeTrustBundle(dataDir, set)
}

// writeTrustBundle assembles every distinct team CA into the file the outbound pool
// reads, and removes it when there is none.
func writeTrustBundle(dataDir string, set TeamSet) error {
	seen := map[string]bool{}
	var bundle []byte
	// Sorted, so an unchanged configuration produces an unchanged file and nothing
	// downstream sees a rewrite that changed nothing.
	for _, project := range sortedKeys(set.Projects) {
		pem := strings.TrimSpace(set.Projects[project].CAPEM)
		if pem == "" || seen[pem] {
			continue
		}
		seen[pem] = true
		bundle = append(bundle, []byte(pem+"\n")...)
	}
	if len(bundle) == 0 {
		_ = os.Remove(TeamCAPath(dataDir))
		return nil
	}
	return os.WriteFile(TeamCAPath(dataDir), bundle, 0o600)
}

func sortedKeys(m map[string]Team) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ReadTeam returns the team cache a local project asks. It is the one reader most
// callers want: the session, the trust bundle and `status` all work in one project.
func ReadTeam(dataDir, project string) (Team, bool, error) {
	set, err := ReadTeams(dataDir)
	if err != nil {
		return Team{}, false, err
	}
	team, has := set.For(project)
	return team, has, nil
}

// ClearTeam forgets every team cache, bundle included.
func ClearTeam(dataDir string) {
	_ = os.Remove(teamPath(dataDir))
	_ = os.Remove(TeamCAPath(dataDir))
}

// chainedEcosystems are the ones a team cache can front today.
//
// pypi, npm and oci compose an upstream URL as origin plus a package path, so the
// team's equivalent of an index is a URL this cache can be pointed at directly. The
// remaining three cannot be chained this way and are absent rather than
// half-supported: apt and git derive their origin from the request itself, and files
// has no upstream at all — its content arrives by upload.
var chainedEcosystems = []struct {
	eco string
	// index is the upstream name, and teamURL builds the team's URL for it.
	index   string
	teamURL func(server, project string) string
	public  string
}{
	{
		eco: "pypi", index: "root/pypi",
		teamURL: func(server, project string) string {
			return server + "/" + project + "/pypi/root/pypi/+simple"
		},
		public: "https://pypi.org/simple",
	},
	{
		eco: "npm", index: "registry",
		teamURL: func(server, project string) string {
			return server + "/" + project + "/npm"
		},
		public: "https://registry.npmjs.org",
	},
	// The three registries the OCI adapter knows by default. Each is chained
	// separately because the alias is part of the path, not a parameter: an image
	// pulled from ghcr must reach the team's ghcr, not its Docker Hub.
	//
	// The public origins carry their /v2 for the reason repoRoot documents — a
	// fallback is the head's URL with its prefix swapped, so both ends of a chain
	// have to be roots of the same shape or the derived URL loses the API root.
	{
		eco: "oci", index: "dockerhub",
		teamURL: ociTeamURL("dockerhub"),
		public:  "https://registry-1.docker.io/v2",
	},
	{
		eco: "oci", index: "ghcr",
		teamURL: ociTeamURL("ghcr"),
		public:  "https://ghcr.io/v2",
	},
	{
		eco: "oci", index: "quay",
		teamURL: ociTeamURL("quay"),
		public:  "https://quay.io/v2",
	},
	// Every other registry, in one row. The three above are named because they are
	// spelled with an alias rather than a host; anything else — nvcr.io, gcr.io,
	// public.ecr.aws — is discovered from the image name, and the only thing this
	// machine needs to know is that discovery resolves on the team's cache rather
	// than on the internet. There is no public row: the fallback origin would have to
	// be the registry the pull names, which is not knowable when the chain is written.
	// A machine pointed at a team cache resolving a discovered registry through it,
	// and only through it, is also what makes -no-direct mean what it says.
	{
		eco: "oci", index: ociname.AnyRegistry,
		teamURL: func(server, project string) string {
			if project == "" || project == config.GlobalProject {
				return server + "/v2"
			}
			return server + "/v2/" + project
		},
	},
}

// ociTeamURL builds the team cache's root for one registry alias.
//
// OCI is the one ecosystem whose project does not ride a leading path segment. The
// distribution spec fixes /v2 as the API root, so the server reads the project from the
// segment after it — and there it treats "global" as a registry alias rather than as the
// global project, because the global project is the one that is never named. So the
// global project addresses /v2/<alias> and every other project /v2/<project>/<alias>,
// which is what the server's own router expects to parse back.
func ociTeamURL(alias string) func(server, project string) string {
	return func(server, project string) string {
		if project == "" || project == config.GlobalProject {
			return server + "/v2/" + alias
		}
		return server + "/v2/" + project + "/" + alias
	}
}

// ChainRows is the upstream chain one team configuration means, as rows.
//
// Extracted so the two appliers cannot drift: setup writes rows through the store with
// the daemon stopped, and `project create` writes them through the daemon's own API
// while it serves. Same rows either way, or a project created after setup would resolve
// differently from one created before it.
func ChainRows(team Team, known func(eco string) bool) []control.Upstream {
	server := strings.TrimRight(team.Server, "/")
	project := team.Project
	if project == "" {
		project = config.GlobalProject
	}
	var rows []control.Upstream
	for _, entry := range chainedEcosystems {
		if known != nil && !known(entry.eco) {
			continue
		}
		rows = append(rows, control.Upstream{
			Eco: entry.eco, Name: entry.index, URL: entry.teamURL(server, project),
			Kind: "origin", Priority: teamPriority, Enabled: true,
		})
		if team.Direct && entry.public != "" {
			rows = append(rows, control.Upstream{
				Eco: entry.eco, Name: entry.index, URL: entry.public,
				Kind: "origin", Priority: publicPriority, Enabled: true,
			})
		}
	}
	return rows
}

// ConfigureChains rewrites every project's upstreams to match the team configuration.
//
// It opens the store, so the caller must have stopped the daemon first — the same
// single-writer rule prune follows. Every project is visited, not only the configured
// ones, because a project that has just lost its team cache has rows to remove.
//
// Rows this wrote before are removed and replaced rather than merged, so running setup
// twice leaves one chain rather than two.
//
// The names it returns are configurations for projects that do not exist here. Nothing
// is wrong with the file — a chain may legitimately be configured before the project it
// is for — but it is also what a typo looks like, and a silently inert configuration is
// the worst outcome available: the machine keeps working, and quietly bypasses the cache
// it was pointed at.
func ConfigureChains(
	ctx context.Context, snap *config.Snapshot, set TeamSet,
) (unknown []string, err error) {
	instance, err := app.Open(snap) //nolint:contextcheck // single-writer storage; its lifetime is the process's, not a request's
	if err != nil {
		return nil, err
	}
	defer func() { _ = instance.Close() }()
	known := func(eco string) bool { _, found := instance.Ecos.Get(eco); return found }
	return ConfigureChainsOn(ctx, instance.Projects, known, set)
}

// ChainStore is the slice of the project service chain configuration needs.
//
// An interface because there are two callers with the same job and different access: the
// command line opens the store with the daemon stopped, and the daemon itself already has
// the live service. Both must write the same rows — a project configured from the window
// and one configured from the terminal that resolved differently would be a bug nobody
// could see.
type ChainStore interface {
	List() ([]control.Project, error)
	Upstreams(project string) ([]control.Upstream, error)
	AddUpstream(project string, row control.Upstream) (control.Upstream, error)
	DeleteUpstream(project string, id int64) error
}

// ConfigureChainsOn is ConfigureChains against an already-open store.
func ConfigureChainsOn(
	ctx context.Context, store ChainStore, known func(string) bool, set TeamSet,
) (unknown []string, err error) {
	projects, err := store.List()
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, project := range projects {
		present[project.Name] = true
	}
	for _, name := range sortedKeys(set.Projects) {
		if !present[name] && set.Projects[name].Server != "" {
			unknown = append(unknown, name)
		}
	}
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		team, has := set.For(project.Name)
		existing, err := store.Upstreams(project.Name)
		if err != nil {
			return nil, err
		}
		for _, row := range existing {
			if !managedRow(row) {
				continue
			}
			if err := store.DeleteUpstream(project.Name, row.ID); err != nil {
				return nil, err
			}
		}
		if !has {
			continue
		}
		for _, row := range ChainRows(team, known) {
			if _, err := store.AddUpstream(project.Name, row); err != nil {
				return nil, fmt.Errorf("local: configure %s upstream for %s: %w",
					row.Eco, project.Name, err)
			}
		}
	}
	return unknown, nil
}

// ConfigureChainVia writes one project's chain through a running daemon.
//
// This is the path `pkgcache project create` takes, and it is why the chain is
// materialised at all: upstream rows are per project in the database, so a project
// created after setup would otherwise inherit nothing and resolve straight to the
// public registry — silently, because a chain missing its first row is still a valid
// chain.
func ConfigureChainVia(ctx context.Context, state State, project string, team Team, has bool) error {
	api := newProjectAPI(state)
	path := "/api/v1/projects/" + project + "/upstreams"
	var existing struct {
		Upstreams []control.Upstream `json:"upstreams"`
	}
	if err := api.do(ctx, http.MethodGet, path, nil, &existing); err != nil {
		return err
	}
	for _, row := range existing.Upstreams {
		if !managedRow(row) {
			continue
		}
		if err := api.do(ctx, http.MethodDelete,
			fmt.Sprintf("%s/%d", path, row.ID), nil, nil); err != nil {
			return err
		}
	}
	if !has {
		return nil
	}
	// No ecosystem filter here: the daemon rejects an unknown one itself, and asking it
	// for the list first would be a round trip to duplicate a check it already makes.
	for _, row := range ChainRows(team, nil) {
		if err := api.do(ctx, http.MethodPost, path, row, nil); err != nil {
			return fmt.Errorf("local: configure %s upstream for %s: %w", row.Eco, project, err)
		}
	}
	return nil
}

// Priorities are spaced so an operator can wedge something between them by hand.
const (
	teamPriority   = 10
	publicPriority = 20
)

// managedRow reports whether a row was written by ConfigureChains.
//
// Identified by priority and ecosystem rather than by a marker column, because an
// upstream's kind is constrained to origin or peer and there is nowhere else to record
// one. That is narrow enough to be safe: these are the exact rows setup writes, and a
// hand-written upstream at the same priority for the same index is one setup would have
// replaced anyway.
func managedRow(row control.Upstream) bool {
	if row.Kind == "peer" {
		return false
	}
	if row.Priority != teamPriority && row.Priority != publicPriority {
		return false
	}
	for _, entry := range chainedEcosystems {
		if row.Eco == entry.eco && row.Name == entry.index {
			return true
		}
	}
	return false
}

// ApplyTeamTrust points the outbound pool at the verified team CAs, if there are any.
//
// One bundle for every project's team cache: the pool has a single root pool, and a
// daemon serves every project, so the trust it needs is the union.
func ApplyTeamTrust(snap *config.Snapshot) error {
	set, err := ReadTeams(snap.DataDir)
	if err != nil {
		return err
	}
	if !set.Any() {
		return nil
	}
	// Written from the records on every WriteTeams, but a cache whose file predates
	// that is repaired here rather than left with a bundle it cannot rebuild.
	if err := writeTrustBundle(snap.DataDir, set); err != nil {
		return err
	}
	path := TeamCAPath(snap.DataDir)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The team cache may use a publicly-trusted certificate, in which case
			// there is nothing extra to trust and the system roots are enough.
			return nil
		}
		return err
	}
	snap.Upstream.CAFile = path
	return nil
}

// ReachableTeam reports whether the team cache is answering.
//
// A liveness probe against its health endpoint, not a package request: this runs from
// `status`, and asking a cache for a package to find out whether it is up would both
// cost more and record a request nobody made.
func ReachableTeam(ctx context.Context, dataDir string, team Team) bool {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(team.Server, "/")+"/healthz", http.NoBody)
	if err != nil {
		return false
	}
	client := probeClient
	if caPEM, readErr := os.ReadFile(TeamCAPath(dataDir)); readErr == nil {
		if trusting, buildErr := trust.ClientFor(caPEM); buildErr == nil {
			trusting.Timeout = probeClient.Timeout
			client = trusting
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode == http.StatusOK
}
