package local

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/control/credential"
	"github.com/aabdlwahab/PKGCache/internal/eco"
)

// Borrowing from the machine next to you, as the daemon does it.
//
// The logic lives here rather than in the command line because there are three callers —
// the terminal, the console and the window — and a sibling is not one row. It is one row
// per ecosystem it fronts plus the digest rows, and three implementations of that
// arithmetic would be three chances to write a chain that half works.

// siblingPriority puts a sibling ahead of the team cache and the public registry.
//
// Ahead on latency alone: a machine on the same desk answers before one on the internet,
// and the fall-through costs one small request when it does not have the file. Its own
// position rather than the team's, so `pkgcache setup` rewriting the team chain leaves a
// sibling where it was — managedRow only claims the two positions setup owns.
const siblingPriority = 5

// PeerRows is the row surface peer management needs from the project service.
type PeerRows interface {
	Upstreams(project string) ([]control.Upstream, error)
	AddUpstream(project string, row control.Upstream) (control.Upstream, error)
	DeleteUpstream(project string, id int64) error
}

// PeerCredentials seals the token the digest half authenticates with.
type PeerCredentials interface {
	Create(credential.Plain) (int64, error)
	Delete(id int64) error
}

// Peers implements the control plane's sibling surface for a local cache.
type Peers struct {
	// Store writes the upstream rows. The daemon's own project service.
	Store PeerRows
	// Credentials seals peer tokens. Nil means the digest half is skipped.
	Credentials PeerCredentials
	// Ecos answers which ecosystems this instance has, so no row is written for one
	// this build does not serve.
	Ecos *eco.Registry
}

var _ controlapi.LocalPeers = (*Peers)(nil)

// Peers reports the siblings one project borrows from.
//
// Grouped by the machine rather than reported as rows: one sibling is six rows, and
// listing them individually would show one laptop six times.
func (p *Peers) Peers(_ context.Context, project string) ([]controlapi.PeerState, error) {
	rows, err := p.Store.Upstreams(project)
	if err != nil {
		return nil, err
	}
	byBase := map[string]*controlapi.PeerState{}
	for _, row := range rows {
		// The position is the marker. A name can be edited in the console and a kind is
		// shared with ordinary origins; only siblings sit at this priority.
		if row.Priority != siblingPriority {
			continue
		}
		base := siblingBase(row.URL)
		if base == "" {
			continue
		}
		state := byBase[base]
		if state == nil {
			state = &controlapi.PeerState{Name: PeerName(base), URL: base}
			byBase[base] = state
		}
		if row.Kind == "peer" {
			state.Name = row.Name
			state.Offline = appendOnce(state.Offline, row.Eco)
			continue
		}
		state.Through = appendOnce(state.Through, row.Eco)
		if far := farProject(row.Eco, row.Name, base, row.URL); far != "" {
			state.TheirProject = far
		}
	}
	out := make([]controlapi.PeerState, 0, len(byBase))
	for _, state := range byBase {
		sort.Strings(state.Through)
		sort.Strings(state.Offline)
		state.Through, state.Offline = orEmpty(state.Through), orEmpty(state.Offline)
		if state.TheirProject == "" {
			state.TheirProject = config.GlobalProject
		}
		out = append(out, *state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out, nil
}

// AddPeer points one project at a sibling, in both of the ways that work.
//
// The wide half treats the sibling as a cache to fetch through, exactly as setup treats
// a pkgreg — a pkgcache serves the same data plane, so everything a team cache fronts, a
// laptop fronts too. The narrow half adds the digest rows for the ecosystems that can be
// asked by hash, which are the only ones consulted while a project is offline.
//
// Replaces rather than appends: running it again is how a token is rotated and how a
// half-written sibling is repaired, and neither should double the chain.
func (p *Peers) AddPeer(
	ctx context.Context, project string, spec controlapi.PeerSpec,
) (controlapi.PeerState, error) {
	base, err := NormalizePeerURL(spec.Address)
	if err != nil {
		return controlapi.PeerState{}, err
	}
	theirProject := strings.TrimSpace(spec.TheirProject)
	if theirProject == "" {
		theirProject = config.GlobalProject
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = PeerName(base)
	}
	if err := p.ForgetPeer(ctx, project, base); err != nil &&
		!strings.Contains(err.Error(), "no peer here") {
		return controlapi.PeerState{}, err
	}

	existing, err := p.Store.Upstreams(project)
	if err != nil {
		return controlapi.PeerState{}, err
	}
	occupied := func(ecoID, index string, priority int) bool {
		for _, row := range existing {
			if row.Eco == ecoID && row.Name == index && row.Priority == priority {
				return true
			}
		}
		return false
	}

	state := controlapi.PeerState{Name: name, URL: base, TheirProject: theirProject}
	for _, entry := range chainedEcosystems {
		if _, known := p.Ecos.Get(entry.eco); !known {
			continue
		}
		if _, err := p.Store.AddUpstream(project, control.Upstream{
			Project: project, Eco: entry.eco, Name: entry.index,
			URL:  entry.teamURL(base, theirProject),
			Kind: "origin", Priority: siblingPriority, Enabled: true,
		}); err != nil {
			return state, fmt.Errorf("local: point %s at %s: %w", entry.eco, base, err)
		}
		state.Through = appendOnce(state.Through, entry.eco)

		// The public origin behind it. A configured chain replaces the adapter's own
		// default outright, so without this a miss at the sibling would fail rather
		// than fall through to the registry it would have used a minute ago.
		if entry.public == "" || occupied(entry.eco, entry.index, publicPriority) {
			continue
		}
		if _, err := p.Store.AddUpstream(project, control.Upstream{
			Project: project, Eco: entry.eco, Name: entry.index, URL: entry.public,
			Kind: "origin", Priority: publicPriority, Enabled: true,
		}); err != nil {
			return state, fmt.Errorf("local: keep the public origin for %s: %w", entry.eco, err)
		}
	}

	sort.Strings(state.Through)
	token := spec.Token
	if token == "" && p.Credentials != nil {
		// Asked of the sibling rather than typed by a person, which is what makes adding
		// one a single act from any of the three surfaces. It works because a cache with
		// no accounts allows the control plane to whoever can reach it — the same fact
		// `pkgcache project create` relies on.
		//
		// A refusal is not fatal: everything above is already written and works. Only the
		// offline half is lost, and the caller can see that from an empty Offline.
		if minted, err := MintPeerToken(ctx, base, "sibling"); err == nil {
			token = minted
		}
	}
	if token == "" || p.Credentials == nil {
		return state, nil
	}
	for _, ecoID := range digestEcosystems {
		if _, known := p.Ecos.Get(ecoID); !known {
			continue
		}
		sealed, err := p.Credentials.Create(credential.Plain{
			Label: "peer " + name, Kind: "bearer", Token: token,
		})
		if err != nil {
			return state, fmt.Errorf("local: seal the peer token: %w", err)
		}
		if _, err := p.Store.AddUpstream(project, control.Upstream{
			Project: project, Eco: ecoID, Name: name, URL: base,
			Kind: "peer", Priority: siblingPriority, Enabled: true,
			CredentialID: &sealed,
		}); err != nil {
			_ = p.Credentials.Delete(sealed)
			return state, fmt.Errorf("local: add the %s peer row: %w", ecoID, err)
		}
		state.Offline = appendOnce(state.Offline, ecoID)
	}
	sort.Strings(state.Offline)
	return state, nil
}

// ForgetPeer removes every row a sibling was added as.
//
// Matched on the machine rather than the row, because "stop borrowing from Sam" is what
// somebody means and it is six rows. The public origins written beside it are left: they
// are the chain this cache would have had anyway.
func (p *Peers) ForgetPeer(_ context.Context, project, nameOrURL string) error {
	rows, err := p.Store.Upstreams(project)
	if err != nil {
		return err
	}
	wanted := ""
	if normalized, err := NormalizePeerURL(nameOrURL); err == nil {
		wanted = siblingBase(normalized)
	}
	removed := 0
	for _, row := range rows {
		if row.Priority != siblingPriority {
			continue
		}
		base := siblingBase(row.URL)
		if base == "" {
			continue
		}
		if base != wanted && row.Name != nameOrURL && PeerName(base) != nameOrURL {
			continue
		}
		if err := p.Store.DeleteUpstream(project, row.ID); err != nil {
			return err
		}
		removed++
	}
	if removed == 0 {
		return fmt.Errorf("local: no peer here is called %q", nameOrURL)
	}
	return nil
}

// orEmpty keeps a JSON array an array. A nil slice marshals to null, so a reader would
// have to tell "none" from "not answered", and both mean none.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// farProject reads back which project on the sibling a chain row points at.
//
// Derived from the row rather than stored beside it, because the row is the truth: a
// console that showed a remembered value would keep showing it after somebody edited the
// URL by hand.
func farProject(ecoID, index, base, rowURL string) string {
	rest := strings.TrimPrefix(strings.TrimPrefix(rowURL, base), "/")
	if rest == "" {
		return ""
	}
	// OCI is the one whose project does not lead: the distribution spec fixes /v2 as the
	// API root, so the project is the segment after it and "global" is never written.
	if ecoID == "oci" {
		rest = strings.Trim(strings.TrimPrefix(rest, "v2"), "/")
		if rest == "" || rest == index {
			return config.GlobalProject
		}
		if first, _, _ := strings.Cut(rest, "/"); first != "" {
			return first
		}
		return config.GlobalProject
	}
	if first, _, _ := strings.Cut(rest, "/"); first != "" {
		return first
	}
	return ""
}
