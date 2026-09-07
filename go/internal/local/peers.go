package local

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Borrowing bytes from the laptop next to you.
//
// A peer is another pkgcache, and the protocol between them is deliberately smaller than
// the one for a team cache: /peer/v1/have and /peer/v1/blob/<sha256>, and nothing else.
// It is digest-first, so the bytes verify themselves and neither machine has to trust
// what the other calls anything — which is the whole reason two laptops can do this and
// a team cache needs a pinned CA.
//
// The engine asks a peer before it gives up offline and before it reaches an upstream,
// so a pair of machines on a plane can fill each other in. What a peer cannot do is
// answer a question about a name: it is asked for a digest, so whichever machine wants
// the file still has to know which file it wants. See digestEcosystems and siblingRows.

// digestEcosystems are the ones the digest-first peer protocol can answer for.
//
// Two conditions, and both have to hold. The engine consults a peer only where the
// adapter knows the digest before it fetches, which rules out npm, apt and gomod — they
// name their content and do not hash it up front. And a peer is stored as an upstream
// row, which an ecosystem that derives its origin from the request cannot have at all:
// the control plane refuses one for git and apt with "derives its upstream from
// requests".
//
// This is the narrow half of what `peer add` writes. The wide half is siblingRows, which
// does not need a digest at all.
var digestEcosystems = []string{"pypi", "oci"}

// Peer is one upstream row, of either half.
type Peer struct {
	ID       int64  `json:"id"`
	Eco      string `json:"eco"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
}

// Sibling is one other machine's cache, as this one has it configured.
//
// Rows are the storage and this is the fact: one machine, written as several rows
// because the upstream table is per ecosystem. Reporting the rows would say "127.0.0.1"
// six times, which reads as six machines.
type Sibling struct {
	// Name is what it is called here.
	Name string
	// URL is its base address.
	URL string
	// Through are the ecosystems fetched through it, the way a team cache is used.
	Through []string
	// Offline are the ones that also answer by digest while this project is offline.
	Offline []string
}

// siblingPriority puts a sibling ahead of the team cache and the public registry.
//
// A peer is on the same desk and an origin is on the internet, so asking the sibling
// first is right on latency alone. It costs nothing when the sibling does not have the
// blob: /peer/v1/have is one small request, and the engine falls through.
const siblingPriority = 5

// ListPeers reports the siblings one project borrows from, both halves together.
//
// A sibling is recognised by its priority rather than by its name or kind: siblingRows
// and the digest rows are the only things written at that position, and it is the one
// marker that survives somebody renaming a row in the console.
func ListPeers(ctx context.Context, state State, project string) ([]Sibling, error) {
	rows, err := listUpstreams(ctx, newProjectAPI(state), project)
	if err != nil {
		return nil, err
	}
	byBase := map[string]*Sibling{}
	for _, row := range rows {
		if row.Priority != siblingPriority {
			continue
		}
		base := siblingBase(row.URL)
		if base == "" {
			continue
		}
		sibling := byBase[base]
		if sibling == nil {
			sibling = &Sibling{Name: PeerName(base), URL: base}
			byBase[base] = sibling
		}
		if row.Kind == "peer" {
			sibling.Name = row.Name
			sibling.Offline = appendOnce(sibling.Offline, row.Eco)
			continue
		}
		sibling.Through = appendOnce(sibling.Through, row.Eco)
	}
	out := make([]Sibling, 0, len(byBase))
	for _, sibling := range byBase {
		sort.Strings(sibling.Through)
		sort.Strings(sibling.Offline)
		out = append(out, *sibling)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out, nil
}

// siblingBase reduces any row's URL to the machine it names.
//
// The digest rows carry the base already; the chain rows carry a path onto it — the
// project, the ecosystem and an index. Both are the same machine, and grouping them by
// anything but the authority would list it twice.
func siblingBase(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func appendOnce(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// AddPeer points one project at a sibling, in both of the ways that work.
//
// The wide half treats the sibling as a cache to fetch through, exactly as `setup`
// treats a pkgreg — because a pkgcache serves the same data plane a pkgreg does, so
// every ecosystem a team cache can front, a friend's laptop can front too. That is
// pypi, npm, oci and gomod: the whole chained set, rather than the two the digest
// protocol reaches.
//
// The narrow half adds the digest-first peer rows for pypi and oci. They are not
// redundant: an origin is not consulted when a project is offline, and a peer is. So the
// wide half is what makes everything work, and the narrow half is what keeps two of them
// working on a plane.
//
// Idempotent: whatever this sibling already had here is removed first, so running it
// twice leaves one set of rows rather than two. That is not tidiness — an earlier version
// wrote a row per ecosystem and stopped at the first refusal, which left half a sibling
// configured and made the obvious fix, running it again, produce a duplicate chain.
// Re-running is also how a token is rotated.
func AddPeer(
	ctx context.Context, state State, project, name, peerURL, theirProject, token string,
	known func(eco string) bool,
) (Sibling, error) {
	normalized, err := NormalizePeerURL(peerURL)
	if err != nil {
		return Sibling{}, err
	}
	if strings.TrimSpace(theirProject) == "" {
		theirProject = config.GlobalProject
	}
	if _, err := RemovePeer(ctx, state, project, normalized); err != nil &&
		!strings.Contains(err.Error(), "no peer here") {
		return Sibling{}, err
	}
	api := newProjectAPI(state)
	path := "/api/v1/projects/" + project + "/upstreams"

	// What is already configured, so a public fallback is added only where the chain
	// does not have one. Writing a second row at the same position is refused by the
	// control plane, and rightly: two origins at one place in a chain is a contradiction.
	existing, err := listUpstreams(ctx, api, project)
	if err != nil {
		return Sibling{}, err
	}
	occupied := func(eco, index string, priority int) bool {
		for _, row := range existing {
			if row.Eco == eco && row.Name == index && row.Priority == priority {
				return true
			}
		}
		return false
	}

	sibling := Sibling{Name: name, URL: normalized}
	for _, entry := range chainedEcosystems {
		if known != nil && !known(entry.eco) {
			continue
		}
		row := map[string]any{
			"eco": entry.eco, "name": entry.index,
			"url":  entry.teamURL(normalized, theirProject),
			"kind": "origin", "priority": siblingPriority, "enabled": true,
		}
		if err := api.do(ctx, http.MethodPost, path, row, nil); err != nil {
			return sibling, fmt.Errorf("pointing %s at %s: %w", entry.eco, normalized, err)
		}
		sibling.Through = appendOnce(sibling.Through, entry.eco)

		// The public origin behind it. A configured chain replaces the adapter's own
		// default outright, so without this a miss at the sibling would fail rather than
		// fall through to the registry it would have used a minute ago.
		if entry.public == "" || occupied(entry.eco, entry.index, publicPriority) {
			continue
		}
		fallback := map[string]any{
			"eco": entry.eco, "name": entry.index, "url": entry.public,
			"kind": "origin", "priority": publicPriority, "enabled": true,
		}
		if err := api.do(ctx, http.MethodPost, path, fallback, nil); err != nil {
			return sibling, fmt.Errorf("keeping the public origin for %s: %w", entry.eco, err)
		}
	}

	// The digest half, where a token was obtained. Without one the sibling still works
	// for everything above; it just cannot answer while this project is offline.
	if token == "" {
		return sibling, nil
	}
	for _, eco := range digestEcosystems {
		if known != nil && !known(eco) {
			continue
		}
		row := map[string]any{
			"eco": eco, "name": name, "url": normalized, "kind": "peer",
			"priority": siblingPriority, "enabled": true,
			// Stored once per row and sealed by the control plane; nothing here keeps a
			// copy and `peer ls` never shows one.
			"credential": map[string]string{"kind": "bearer", "token": token},
		}
		if err := api.do(ctx, http.MethodPost, path, row, nil); err != nil {
			return sibling, fmt.Errorf("adding the %s peer row: %w", eco, err)
		}
		sibling.Offline = appendOnce(sibling.Offline, eco)
	}
	sort.Strings(sibling.Through)
	sort.Strings(sibling.Offline)
	return sibling, nil
}

// listUpstreams reports every row one project has, of any kind.
func listUpstreams(ctx context.Context, api projectAPI, project string) ([]Peer, error) {
	var body struct {
		Upstreams []Peer `json:"upstreams"`
	}
	err := api.do(ctx, http.MethodGet, "/api/v1/projects/"+project+"/upstreams", nil, &body)
	return body.Upstreams, err
}

// RemovePeer forgets every row a sibling was added as, of either half.
//
// Matched on the machine rather than the row, because "stop borrowing from Sam" is the
// thing somebody means, and it is six rows.
func RemovePeer(ctx context.Context, state State, project, nameOrURL string) (int, error) {
	api := newProjectAPI(state)
	rows, err := listUpstreams(ctx, api, project)
	if err != nil {
		return 0, err
	}
	normalized, urlErr := NormalizePeerURL(nameOrURL)
	wanted := ""
	if urlErr == nil {
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
		if err := api.do(ctx, http.MethodDelete,
			fmt.Sprintf("/api/v1/projects/%s/upstreams/%d", project, row.ID),
			nil, nil); err != nil {
			return removed, err
		}
		removed++
	}
	if removed == 0 {
		return 0, fmt.Errorf("local: no peer here is called %q", nameOrURL)
	}
	return removed, nil
}

// MintPeerToken asks a cache for a token a sibling can use against it.
//
// Asked of the sibling rather than typed by a person, which is what makes adding one a
// single command. It works because a cache with no accounts allows the control plane to
// whoever can reach it — the same fact that makes `pkgcache project create` work — and
// where that is not true the caller is told to run `pkgcache peer token` over there and
// pass the result.
func MintPeerToken(ctx context.Context, base, label string) (string, error) {
	var created struct {
		Secret string `json:"secret"`
	}
	api := projectAPI{base: strings.TrimRight(base, "/"), client: peerClient()}
	request := map[string]string{
		"project": config.GlobalProject, "scope": "peer", "label": label,
	}
	if err := api.do(ctx, http.MethodPost, "/api/v1/tokens", request, &created); err != nil {
		return "", err
	}
	if created.Secret == "" {
		return "", errors.New("local: that cache issued a token but did not return it")
	}
	return created.Secret, nil
}

// peerClient reaches a sibling.
//
// Short, because every use of it is a person waiting at a prompt: a laptop that is
// asleep, gone from the network or listening only on its own loopback should say so in
// seconds rather than hold the terminal for a minute.
func peerClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// NormalizePeerURL turns what somebody types into the base a peer request is built on.
//
// A person types a host, or a host and a port, or pastes the address out of `status`.
// All three mean the same sibling, and storing them differently would make one machine
// look like three in `peer ls`.
func NormalizePeerURL(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", errors.New("local: which cache? give its address")
	}
	if !strings.Contains(text, "://") {
		text = "http://" + text
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return "", fmt.Errorf("local: %s is not an address: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("local: %s names no host", raw)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("local: %s is not http or https", raw)
	}
	if parsed.Port() == "" {
		// The port every pkgcache listens on unless it was told otherwise. Guessing it
		// is right far more often than making somebody type it.
		parsed.Host = fmt.Sprintf("%s:%d", parsed.Host, config.LocalPort)
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.Path, "/"), nil
}

// KnownEcosystems reports which ecosystems a cache serves, for deciding which rows are
// worth writing.
//
// Asked of the daemon rather than assumed, because a build can be assembled with fewer:
// writing a gomod row against a cache that has no gomod adapter would be refused, and
// refused halfway through leaves half a sibling configured.
func KnownEcosystems(ctx context.Context, state State) (func(string) bool, error) {
	var body struct {
		Ecosystems []struct {
			ID string `json:"id"`
		} `json:"ecosystems"`
	}
	if err := newProjectAPI(state).do(
		ctx, http.MethodGet, "/api/v1/ecosystems", nil, &body); err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(body.Ecosystems))
	for _, ecosystem := range body.Ecosystems {
		have[ecosystem.ID] = true
	}
	return func(id string) bool { return have[id] }, nil
}

// PeerName is what a sibling is called here when nobody said.
//
// The host, without the port: two machines are told apart by name, and a port every
// cache shares carries no information. Falls back to the whole address where there is no
// host to take, which cannot happen after NormalizePeerURL but is not worth a panic.
func PeerName(base string) string {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" {
		return base
	}
	return parsed.Hostname()
}

// ReachPeer reports whether a sibling answers, and says something useful when it does not.
func ReachPeer(ctx context.Context, base string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/healthz", http.NoBody)
	if err != nil {
		return err
	}
	response, err := peerClient().Do(request)
	if err != nil {
		return fmt.Errorf("%s did not answer: %w\n"+
			"  a cache listens on loopback unless it was told otherwise, so a sibling on\n"+
			"  another machine has to be started with PKGCACHE_ADDR=0.0.0.0:%d",
			base, err, config.LocalPort)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 400 {
		return fmt.Errorf("%s answered %s, which is not a pkgcache", base, response.Status)
	}
	return nil
}
