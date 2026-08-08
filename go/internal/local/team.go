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
	"strings"

	"github.com/brightskies/pkgreg/internal/app"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
	"github.com/brightskies/pkgreg/internal/trust"
)

// The middle tier: a team's cache, sitting between this machine and the registries.
//
// It is expressed as an ordinary upstream chain rather than as a mode. Configuring a
// team cache writes two rows per index — the team at priority 10 and the public
// registry at 20 — and everything after that is the engine walking a chain it already
// knows how to walk. There is no "team cache" concept anywhere below this file, which
// is what keeps the fallback behaviour identical to any other chain and testable the
// same way.

// Team is the cache this machine asks before it asks the internet.
type Team struct {
	// Server is the origin, for example https://cache.internal:8443.
	Server string `json:"server"`
	// Fingerprint is the CA's SHA-256, kept so `status` can show what was pinned.
	Fingerprint string `json:"fingerprint"`
	// Project scopes the URLs on the team's side, which need not match this machine's.
	Project string `json:"project"`
	// Direct records whether the public registry is in the chain behind the team cache.
	// False is `setup -no-direct`: a machine that must never reach a registry itself.
	Direct bool `json:"direct"`
	// NoCache turns local caching off entirely, leaving a verified loopback bridge to
	// the team cache and nothing else — which is exactly what pkgreg-client did.
	//
	// It is a promise as much as a setting: in this mode no store is opened, no
	// database is created and nothing is written to the cache directory, so the merged
	// binary costs an existing client's users its size and nothing else.
	NoCache bool `json:"no_cache,omitempty"`
}

func teamPath(dataDir string) string { return filepath.Join(dataDir, "team.json") }

// TeamCAPath is where the verified CA is kept, and is what upstream.ca_file points at.
func TeamCAPath(dataDir string) string { return filepath.Join(dataDir, "team-ca.crt") }

// ReadTeam returns the configured team cache, and whether there is one.
func ReadTeam(dataDir string) (Team, bool, error) {
	data, err := os.ReadFile(teamPath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Team{}, false, nil
		}
		return Team{}, false, fmt.Errorf("local: read team cache settings: %w", err)
	}
	var team Team
	if err := json.Unmarshal(data, &team); err != nil || team.Server == "" {
		return Team{}, false, nil
	}
	return team, true, nil
}

// WriteTeam records the team cache.
func WriteTeam(dataDir string, team Team) error {
	data, err := json.MarshalIndent(team, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(teamPath(dataDir), append(data, '\n'), 0o600)
}

// ClearTeam forgets it, CA included.
func ClearTeam(dataDir string) {
	_ = os.Remove(teamPath(dataDir))
	_ = os.Remove(TeamCAPath(dataDir))
}

// chainedEcosystems are the ones a team cache can front today.
//
// pypi and npm compose an upstream URL as origin plus a package path, so the team's
// equivalent of an index is a URL this cache can be pointed at directly. The others
// cannot be chained this way and are deliberately absent rather than half-supported:
// apt and git derive their origin from the request itself, files is local-only, and
// oci resolves through a registry alias whose path shape needs its own decision — see
// docs/client-merge-plan.md, open question 2.
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
}

// ConfigureChains rewrites this cache's upstreams to put the team cache in front.
//
// It opens the store directly, so the caller must have stopped the daemon first — the
// same single-writer rule prune follows. Rows this wrote before are removed and
// replaced rather than merged, so running setup twice leaves one chain rather than two.
func ConfigureChains(ctx context.Context, snap *config.Snapshot, team Team, has bool) error {
	instance, err := app.Open(snap)
	if err != nil {
		return err
	}
	defer func() { _ = instance.Close() }()

	existing, err := instance.Projects.Upstreams(config.GlobalProject)
	if err != nil {
		return err
	}
	for _, row := range existing {
		if !managedRow(row) {
			continue
		}
		if err := instance.Projects.DeleteUpstream(config.GlobalProject, row.ID); err != nil {
			return err
		}
	}
	if !has {
		return nil
	}

	server := strings.TrimRight(team.Server, "/")
	project := team.Project
	if project == "" {
		project = config.GlobalProject
	}
	for _, entry := range chainedEcosystems {
		if _, found := instance.Ecos.Get(entry.eco); !found {
			continue
		}
		rows := []control.Upstream{{
			Eco: entry.eco, Name: entry.index, URL: entry.teamURL(server, project),
			Kind: "origin", Priority: teamPriority, Enabled: true,
		}}
		if team.Direct {
			rows = append(rows, control.Upstream{
				Eco: entry.eco, Name: entry.index, URL: entry.public,
				Kind: "origin", Priority: publicPriority, Enabled: true,
			})
		}
		for _, row := range rows {
			if _, err := instance.Projects.AddUpstream(config.GlobalProject, row); err != nil {
				return fmt.Errorf("local: configure %s upstream: %w", entry.eco, err)
			}
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

// ApplyTeamTrust points the outbound pool at the verified team CA, if there is one.
func ApplyTeamTrust(snap *config.Snapshot) error {
	_, has, err := ReadTeam(snap.DataDir)
	if err != nil {
		return err
	}
	if !has {
		return nil
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
		ctx, http.MethodGet, strings.TrimRight(team.Server, "/")+"/healthz", nil)
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
