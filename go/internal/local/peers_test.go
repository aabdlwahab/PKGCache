//go:build !windows

package local

import "testing"

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

// Only ecosystems that can hold a peer row and would ever be asked for one. A row for
// anything else is refused by the control plane or never consulted by the engine, and
// either way it is a line in somebody's chain that does nothing.
func TestPeerEcosystemsAreTheOnesThatWork(t *testing.T) {
	allowed := map[string]bool{"pypi": true, "oci": true}
	for _, eco := range peerEcosystems {
		if !allowed[eco] {
			t.Errorf("%s cannot serve a peer row", eco)
		}
	}
	if len(peerEcosystems) != len(allowed) {
		t.Errorf("peerEcosystems is %v, want every one that works", peerEcosystems)
	}
}
