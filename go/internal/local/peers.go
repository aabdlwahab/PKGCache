package local

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
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

// Sibling is one other machine's cache, as this one has it configured.
//
// The arithmetic that produces it lives in the daemon — see peersurface.go — because the
// console and the window ask for the same thing and a sibling is not one row. These are
// the client half: what the terminal calls to reach that surface.
type Sibling = controlapi.PeerState

// ListPeers reports the siblings one project borrows from.
func ListPeers(ctx context.Context, state State, project string) ([]Sibling, error) {
	var body struct {
		Peers []Sibling `json:"peers"`
	}
	err := newProjectAPI(state).do(ctx, http.MethodGet,
		"/api/v1/local/peers/"+project, nil, &body)
	return body.Peers, err
}

// AddPeer points one project at a sibling.
func AddPeer(
	ctx context.Context, state State, project string, spec controlapi.PeerSpec,
) (Sibling, error) {
	var added Sibling
	err := newProjectAPI(state).do(ctx, http.MethodPost,
		"/api/v1/local/peers/"+project, spec, &added)
	return added, err
}

// RemovePeer forgets every row a sibling was added as.
func RemovePeer(ctx context.Context, state State, project, nameOrURL string) error {
	return newProjectAPI(state).do(ctx, http.MethodDelete,
		"/api/v1/local/peers/"+project+"/"+url.PathEscape(nameOrURL), nil, nil)
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
