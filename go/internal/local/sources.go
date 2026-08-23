package local

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/trust"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

// Pointing a project at a team cache while the cache is running.
//
// `pkgcache setup` does this from the command line with the daemon stopped, which is the
// simple case: nothing is serving, so the store has one writer and the trust file is read
// fresh at the next start. Doing it from the window is the same four steps in a live
// process, and the fourth one is the one that used to be impossible:
//
//  1. fetch the CA and refuse it unless it matches the fingerprint the person was given;
//  2. write it into the record, which rewrites the bundle;
//  3. rewrite that project's chain through the live project service;
//  4. reload the outbound pool, so the process trusts the cache it has just pinned.
//
// Without (4) the configuration was correct and every fetch to the new middle tier failed
// with a certificate error, until somebody restarted the daemon. See Pool.ReloadTrust.

// Sources implements the control plane's per-project upstream surface for a local cache.
type Sources struct {
	// DataDir is where team.json and the trust bundle live.
	DataDir string
	// Store writes the upstream rows. The daemon's own project service.
	Store ChainStore
	// Ecos answers which ecosystems this instance has.
	Ecos *eco.Registry
	// Pool is reloaded when the trust bundle changes.
	Pool *upstream.Pool
	// Snapshot is the running configuration, for the CA file path the pool reads.
	Snapshot *config.Snapshot
}

// probeTimeout bounds the reachability check a listing performs. Short: it runs while
// somebody waits for a window to paint, and a team cache that takes longer than this to
// answer a health check is one they need to know about anyway.
const probeTimeout = 2 * time.Second

// Sources reports what every project resolves through.
func (s *Sources) Sources(ctx context.Context) ([]controlapi.SourceState, error) {
	set, err := ReadTeams(s.DataDir)
	if err != nil {
		return nil, err
	}
	projects, err := s.Store.List()
	if err != nil {
		return nil, err
	}
	// One probe per distinct server, not per project: five projects pointed at one cache
	// is one question, and asking it five times would make a listing five times slower
	// for the same answer.
	reachable := map[string]bool{}
	for _, project := range projects {
		if team, has := set.For(project.Name); has {
			if _, done := reachable[team.Server]; !done {
				probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				reachable[team.Server] = ReachableTeam(probeCtx, s.DataDir, team)
				cancel()
			}
		}
	}
	states := make([]controlapi.SourceState, 0, len(projects))
	for _, project := range projects {
		state := controlapi.SourceState{Project: project.Name}
		team, has := set.For(project.Name)
		if has {
			_, own := set.Own(project.Name)
			up := reachable[team.Server]
			state.Server = team.Server
			state.Fingerprint = team.Fingerprint
			state.TeamProject = team.Project
			state.Direct = team.Direct
			state.Inherited = !own
			state.Reachable = &up
		} else {
			// No team cache is not an absence of configuration; it is the configuration.
			// Direct says so, rather than leaving a caller to infer it from empty fields.
			state.Direct = true
		}
		states = append(states, state)
	}
	return states, nil
}

// Configure points one project at a cache, after verifying it.
func (s *Sources) Configure(
	ctx context.Context, project string, spec controlapi.SourceSpec,
) (controlapi.SourceState, error) {
	server := strings.TrimSpace(spec.Server)
	if server == "" {
		return controlapi.SourceState{}, control.NewError(http.StatusBadRequest,
			"server_required", "a server address is required")
	}
	if strings.TrimSpace(spec.Fingerprint) == "" {
		// Refused rather than defaulted. The fingerprint is the whole of the trust
		// decision: without it this would fetch a CA from the address it was given and
		// believe whatever answered. `pkgcache setup -ca-file` exists for the case where
		// somebody has the certificate itself, and that is a file, not a form field.
		return controlapi.SourceState{}, control.NewError(http.StatusBadRequest,
			"fingerprint_required",
			"the cache's CA fingerprint is required — the one you were given out of band, "+
				"not from this network")
	}
	if _, err := s.projectExists(project); err != nil {
		return controlapi.SourceState{}, err
	}

	verified, err := trust.Fetch(ctx, trust.Options{
		Server: server, ExpectedSHA256: spec.Fingerprint,
	})
	if err != nil {
		// Carried through as a client error, message intact. This is the most important
		// sentence this function can produce — "the cache at that address is not the one
		// you were told about" — and as a bare error it reached the caller as
		// "internal server error", which reads as a bug in the cache rather than as a
		// refusal to trust a stranger.
		return controlapi.SourceState{}, control.NewError(http.StatusBadRequest,
			"untrusted_server", "%s", err.Error())
	}
	set, err := ReadTeams(s.DataDir)
	if err != nil {
		return controlapi.SourceState{}, err
	}
	teamProject := strings.TrimSpace(spec.TeamProject)
	if teamProject == "" {
		teamProject = config.GlobalProject
	}
	set.Set(project, Team{
		Server:      verified.Base.String(),
		Fingerprint: verified.Fingerprint,
		Project:     teamProject,
		Direct:      spec.Direct,
		CAPEM:       string(verified.CAPEM),
	})
	if err := s.apply(ctx, set); err != nil {
		return controlapi.SourceState{}, err
	}
	up := true
	return controlapi.SourceState{
		Project: project, Server: verified.Base.String(), Fingerprint: verified.Fingerprint,
		TeamProject: teamProject, Direct: spec.Direct, Reachable: &up,
	}, nil
}

// Forget removes one project's own configuration.
func (s *Sources) Forget(ctx context.Context, project string) error {
	set, err := ReadTeams(s.DataDir)
	if err != nil {
		return err
	}
	if _, own := set.Own(project); !own {
		// Nothing of its own to remove. Reported rather than silently succeeding, because
		// the project may still be inheriting the global one and "forgotten" would then be
		// a false statement about where its packages come from.
		return control.NewError(http.StatusNotFound, "no_own_source",
			"%s has no team cache of its own; it follows %s", project, config.GlobalProject)
	}
	set.Remove(project)
	return s.apply(ctx, set)
}

// Adopt writes the chain a newly created project inherits.
//
// It re-applies the whole record rather than one project's rows, because that is already
// the operation that makes every project's chain match the configuration — and a project
// that has just appeared is exactly the case it handles. Applying it twice is harmless:
// ConfigureChains replaces the rows it wrote before rather than adding to them.
func (s *Sources) Adopt(ctx context.Context, _ string) error {
	set, err := ReadTeams(s.DataDir)
	if err != nil {
		return err
	}
	if !set.Any() {
		// Nothing is configured, so there is nothing to inherit and no rows to write.
		return nil
	}
	return s.apply(ctx, set)
}

// apply writes the record, rewrites every chain, and reloads outbound trust.
//
// In that order, and all three every time. A record written without the chain leaves a
// project resolving the old way; a chain written without the reload leaves it unable to
// verify the cache it now points at.
func (s *Sources) apply(ctx context.Context, set TeamSet) error {
	if err := WriteTeams(s.DataDir, set); err != nil {
		return err
	}
	known := func(id string) bool { _, found := s.Ecos.Get(id); return found }
	if _, err := ConfigureChainsOn(ctx, s.Store, known, set); err != nil {
		return err
	}
	caFile := TeamCAPath(s.DataDir)
	if _, err := os.Stat(caFile); err != nil {
		// Every team cache used a publicly-trusted certificate, or there are none left.
		// Either way the bundle is gone and the system roots are the whole answer.
		caFile = ""
	}
	if s.Snapshot != nil {
		s.Snapshot.Upstream.CAFile = caFile
	}
	if s.Pool == nil {
		return nil
	}
	return s.Pool.ReloadTrust(caFile)
}

func (s *Sources) projectExists(project string) (bool, error) {
	projects, err := s.Store.List()
	if err != nil {
		return false, err
	}
	for _, candidate := range projects {
		if candidate.Name == project {
			return true, nil
		}
	}
	return false, control.NewError(http.StatusNotFound, "project_not_found",
		"there is no project named %q on this cache", project)
}
