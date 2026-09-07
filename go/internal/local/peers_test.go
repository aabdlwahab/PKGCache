//go:build !windows

package local

import (
	"strings"
	"testing"
)

// What somebody types has to become one address, or the same machine appears in
// `peer ls` three times under three spellings.
func TestPeerURLsNormalizeToOneAddress(t *testing.T) {
	const want = "http://laptop.local:41780"
	for _, given := range []string{
		"laptop.local", "laptop.local:41780", "http://laptop.local",
		"http://laptop.local:41780", "http://laptop.local:41780/", "  laptop.local  ",
	} {
		got, err := NormalizePeerURL(given)
		if err != nil {
			t.Errorf("%q: %v", given, err)
			continue
		}
		if got != want {
			t.Errorf("%q became %q, want %q", given, got, want)
		}
	}
	// A port somebody gave is theirs to keep.
	if got, err := NormalizePeerURL("laptop.local:9999"); err != nil ||
		got != "http://laptop.local:9999" {
		t.Errorf("an explicit port became %q (%v)", got, err)
	}
}

// The refusals are the ones that would otherwise be stored and fail later, at a moment
// with no obvious connection to the address that was typed.
func TestPeerURLsThatAreRefused(t *testing.T) {
	for _, given := range []string{"", "   ", "ftp://laptop.local", "http://"} {
		if got, err := NormalizePeerURL(given); err == nil {
			t.Errorf("%q was accepted as %q", given, got)
		}
	}
}

// The name is what tells two siblings apart in a listing, so it is the host and not the
// address — a port every cache shares carries no information.
func TestPeerNameIsTheHost(t *testing.T) {
	for address, want := range map[string]string{
		"http://laptop.local:41780": "laptop.local",
		"http://192.168.1.4:41780":  "192.168.1.4",
		"https://desk:8443":         "desk",
	} {
		if got := PeerName(address); got != want {
			t.Errorf("%s named %q, want %q", address, got, want)
		}
	}
}

// The digest half reaches only what can be asked for by hash. A row for anything else is
// refused by the control plane or never consulted by the engine, and either way it is a
// line in somebody's chain that does nothing.
func TestDigestEcosystemsAreTheOnesThatWork(t *testing.T) {
	allowed := map[string]bool{"pypi": true, "oci": true}
	for _, eco := range digestEcosystems {
		if !allowed[eco] {
			t.Errorf("%s cannot serve a peer row", eco)
		}
	}
	if len(digestEcosystems) != len(allowed) {
		t.Errorf("digestEcosystems is %v, want every one that works", digestEcosystems)
	}
}

// The wide half is the whole chained set, and it has to stay that way: the point of
// treating a sibling like a pkgreg is that everything a team cache fronts, a friend's
// laptop fronts too. A chained ecosystem missing from here would work through a server
// and silently not through a laptop.
func TestASiblingFrontsEveryChainedEcosystem(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range chainedEcosystems {
		seen[entry.eco] = true
		if entry.teamURL == nil {
			t.Errorf("%s/%s has no URL builder", entry.eco, entry.index)
			continue
		}
		got := entry.teamURL("http://laptop:41780", "global")
		if got == "" || !strings.HasPrefix(got, "http://laptop:41780/") {
			t.Errorf("%s/%s builds %q against a sibling", entry.eco, entry.index, got)
		}
	}
	for _, want := range []string{"pypi", "npm", "oci", "gomod"} {
		if !seen[want] {
			t.Errorf("%s is not chained, so a sibling cannot front it", want)
		}
	}
}

// A sibling goes in front of a team cache and the public registry, and must not collide
// with either: the upstream table is unique per index per position.
func TestSiblingSitsAheadOfTheRestOfTheChain(t *testing.T) {
	if siblingPriority >= teamPriority || siblingPriority >= publicPriority {
		t.Fatalf("a sibling at %d does not come before the team at %d or public at %d",
			siblingPriority, teamPriority, publicPriority)
	}
}
