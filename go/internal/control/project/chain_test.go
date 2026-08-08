package project

import (
	"testing"

	"github.com/brightskies/pkgreg/internal/config"
)

// The chain a request walks is decided here, so its order is worth pinning at the point
// it is produced rather than only where it is consumed.
func TestSortChainsOrdersByPriorityThenURL(t *testing.T) {
	overrides := map[string]map[string]map[string][]config.Endpoint{
		"global": {
			"pypi": {
				"root/pypi": {
					{URL: "https://pypi.org/simple", Priority: 20},
					{URL: "https://cache.internal/simple", Priority: 10},
					// Two at the same priority: without a second sort key these would
					// swap places between restarts, and a cache would appear to change
					// its mind about where it fetches from.
					{URL: "https://b.example", Priority: 15},
					{URL: "https://a.example", Priority: 15},
				},
			},
		},
	}
	sortChains(overrides)
	chain := overrides["global"]["pypi"]["root/pypi"]
	want := []string{
		"https://cache.internal/simple",
		"https://a.example",
		"https://b.example",
		"https://pypi.org/simple",
	}
	if len(chain) != len(want) {
		t.Fatalf("chain = %+v", chain)
	}
	for i, url := range want {
		if chain[i].URL != url {
			t.Fatalf("chain[%d] = %q, want %q (whole chain: %+v)", i, chain[i].URL, url, chain)
		}
	}
}

// Sorting must be stable across repeated projections, because publish runs on every
// control-plane change and a chain that reorders is a cache that silently moves.
func TestSortChainsIsStable(t *testing.T) {
	build := func() map[string]map[string]map[string][]config.Endpoint {
		return map[string]map[string]map[string][]config.Endpoint{
			"global": {"npm": {"registry": {
				{URL: "https://one.example", Priority: 5},
				{URL: "https://two.example", Priority: 5},
				{URL: "https://three.example", Priority: 5},
			}}},
		}
	}
	first := build()
	sortChains(first)
	for range 10 {
		next := build()
		sortChains(next)
		for i := range next["global"]["npm"]["registry"] {
			if next["global"]["npm"]["registry"][i].URL !=
				first["global"]["npm"]["registry"][i].URL {
				t.Fatalf("chain reordered between projections: %+v vs %+v",
					first["global"]["npm"]["registry"], next["global"]["npm"]["registry"])
			}
		}
	}
}
