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
// the file still has to know which file it wants. See peerEcosystems.

// peerEcosystems are the ones a peer can actually answer for.
//
// Two conditions, and both have to hold. The engine consults a peer only where the
// adapter knows the digest before it fetches, which rules out npm, apt and gomod — they
// name their content and do not hash it up front, so a row for them would sit in the
// chain and never once be asked. And a peer is stored as an upstream row, which an
// ecosystem that derives its origin from the request cannot have at all: the control
// plane refuses one for git and apt outright, with "derives its upstream from requests".
//
// git is the interesting exclusion, because it does know its digests. It has no upstream
// rows to put a peer in, so borrowing a git object from a sibling would need a different
// mechanism than this one, and pretending otherwise here would write a row the server
// rejects.
var peerEcosystems = []string{"pypi", "oci"}

// Peer is one sibling cache, as this machine has it configured.
type Peer struct {
	// ID is the upstream row, for removal.
	ID int64 `json:"id"`
	// Eco is which ecosystem's chain this row sits in. One sibling means one row per
	// ecosystem, which is what the upstream table is shaped for.
	Eco string `json:"eco"`
	// Name is the label the rows share, so several rows read as one sibling.
	Name string `json:"name"`
	// URL is the sibling's base address.
	URL string `json:"url"`
	// Priority orders it against the origins beside it.
	Priority int  `json:"priority"`
	Enabled  bool `json:"enabled"`
}

// peerPriority puts a sibling ahead of every origin.
//
// A peer is on the same desk and an origin is on the internet, so asking the sibling
// first is right on latency alone. It costs nothing when the sibling does not have the
// blob: /peer/v1/have is one small request, and the engine falls through.
const peerPriority = 5

// ListPeers reports the siblings one project borrows from.
func ListPeers(ctx context.Context, state State, project string) ([]Peer, error) {
	var body struct {
		Upstreams []struct {
			Peer
			Kind string `json:"kind"`
		} `json:"upstreams"`
	}
	if err := newProjectAPI(state).do(ctx, http.MethodGet,
		"/api/v1/projects/"+project+"/upstreams", nil, &body); err != nil {
		return nil, err
	}
	var peers []Peer
	for _, row := range body.Upstreams {
		if row.Kind == "peer" {
			peers = append(peers, row.Peer)
		}
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		return peers[i].Eco < peers[j].Eco
	})
	return peers, nil
}

// AddPeer points one project at a sibling, one row per ecosystem that can use it.
//
// Idempotent: whatever this sibling already had here is removed first, so running it
// twice leaves one set of rows rather than two. That is not tidiness — the first version
// wrote a row per ecosystem and stopped at the first refusal, which left half a sibling
// configured and made the obvious fix, running it again, produce a chain with the same
// peer in it twice. Re-running is also how a token is rotated, and rotating must not
// double the rows it rotates.
func AddPeer(
	ctx context.Context, state State, project, name, peerURL, token string,
) ([]Peer, error) {
	normalized, err := NormalizePeerURL(peerURL)
	if err != nil {
		return nil, err
	}
	for _, existing := range []string{name, normalized} {
		if _, err := RemovePeer(ctx, state, project, existing); err != nil &&
			!strings.Contains(err.Error(), "no peer here") {
			return nil, err
		}
	}
	api := newProjectAPI(state)
	var added []Peer
	for _, eco := range peerEcosystems {
		request := map[string]any{
			"eco": eco, "name": name, "url": normalized, "kind": "peer",
			"priority": peerPriority, "enabled": true,
			// The secret is stored once per row by the control plane, which seals it;
			// nothing here keeps a copy and `peer ls` never shows one.
			"credential": map[string]string{"kind": "bearer", "token": token},
		}
		var created Peer
		if err := api.do(ctx, http.MethodPost,
			"/api/v1/projects/"+project+"/upstreams", request, &created); err != nil {
			return added, fmt.Errorf("adding the %s row: %w", eco, err)
		}
		added = append(added, created)
	}
	return added, nil
}

// RemovePeer forgets every row a sibling was added as.
func RemovePeer(ctx context.Context, state State, project, nameOrURL string) (int, error) {
	peers, err := ListPeers(ctx, state, project)
	if err != nil {
		return 0, err
	}
	normalized, urlErr := NormalizePeerURL(nameOrURL)
	api := newProjectAPI(state)
	removed := 0
	for _, peer := range peers {
		if peer.Name != nameOrURL && !(urlErr == nil && peer.URL == normalized) {
			continue
		}
		if err := api.do(ctx, http.MethodDelete,
			fmt.Sprintf("/api/v1/projects/%s/upstreams/%d", project, peer.ID),
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
