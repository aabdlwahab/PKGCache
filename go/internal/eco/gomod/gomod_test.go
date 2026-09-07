package gomod

import (
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/eco"
)

// The freshness split is the whole of what this adapter decides about caching, and both
// halves are load-bearing. A list treated as immutable pins a module's versions at
// whatever they were the first time anybody asked; a zip treated as mutable revalidates
// content the module system guarantees never changes.
func TestFreshnessSplitsMovingAnswersFromFixedOnes(t *testing.T) {
	descriptor := New().Descriptor()
	for _, test := range []struct {
		key     string
		revalid bool
	}{
		{"list/goproxy/github.com/google/uuid", true},
		{"latest/goproxy/github.com/google/uuid", true},
		{"goproxy/@v/github.com/google/uuid/v1.6.0.zip", false},
		{"goproxy/@v/github.com/google/uuid/v1.6.0.mod", false},
		{"goproxy/@v/github.com/google/uuid/v1.6.0.info", false},
	} {
		got := descriptor.FreshnessFor(test.key)
		if revalidates := got != eco.Immutable; revalidates != test.revalid {
			t.Errorf("%s revalidates=%v, want %v", test.key, revalidates, test.revalid)
		}
	}
}

// Only the zip is a package. Listing .info and .mod as artifacts would show three rows
// per dependency, none of which anybody installed.
func TestOnlyTheZipIsAnArtifact(t *testing.T) {
	for _, test := range []struct {
		key           string
		name, version string
		ok            bool
	}{
		{"goproxy/@v/github.com/google/uuid/v1.6.0.zip", "github.com/google/uuid", "v1.6.0", true},
		{"goproxy/@v/github.com/google/uuid/v1.6.0.mod", "", "", false},
		{"goproxy/@v/github.com/google/uuid/v1.6.0.info", "", "", false},
		{"list/goproxy/github.com/google/uuid", "", "", false},
		// The protocol's own escaping: an upper-case letter arrives as !x, and the name
		// is recorded as it is addressed rather than guessed back into mixed case.
		{"goproxy/@v/github.com/!burnt!sushi/toml/v1.4.0.zip",
			"github.com/!burnt!sushi/toml", "v1.4.0", true},
	} {
		name, version, arch, ok := parseArtifactKey(test.key)
		if ok != test.ok {
			t.Errorf("%s parsed=%v, want %v", test.key, ok, test.ok)
			continue
		}
		if !ok {
			continue
		}
		if name != test.name || version != test.version || arch != "" {
			t.Errorf("%s = %q %q %q, want %q %q and no arch",
				test.key, name, version, arch, test.name, test.version)
		}
	}
}

// A module path reaches an upstream URL and a cache key, so a traversal in it would
// reach both. The protocol escapes upper case with "!" and reserves "@", so neither a
// dot-dot nor an "@" is ever a legitimate path.
func TestModulePathsThatMustBeRefused(t *testing.T) {
	for _, module := range []string{
		"", "..", "github.com/../../etc", "github.com//uuid",
		"github.com/google/uuid/@v/list", "github.com/./uuid",
	} {
		if validModulePath(module) {
			t.Errorf("%q was accepted", module)
		}
	}
	for _, module := range []string{
		"github.com/google/uuid", "github.com/!burnt!sushi/toml",
		"golang.org/x/sync", "gopkg.in/yaml.v3",
	} {
		if !validModulePath(module) {
			t.Errorf("%q was refused", module)
		}
	}
}

// Only the three files the protocol defines. Anything else is a request this cache has
// no freshness rule for, and caching a response it cannot reason about is worse than
// declining it.
func TestOnlyTheProtocolsFilesAreServed(t *testing.T) {
	for _, name := range []string{"v1.6.0.zip", "v1.6.0.mod", "v1.6.0.info"} {
		if _, _, ok := splitFile(name); !ok {
			t.Errorf("%s was refused", name)
		}
	}
	for _, name := range []string{"v1.6.0.ziphash", "list", "v1.6.0", ".zip", "v1.6.0.tar"} {
		if _, _, ok := splitFile(name); ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The default has to be one path segment, because the module path is the greedy capture
// and the router allows only one. A two-segment name here would make every module
// request ambiguous.
func TestTheDefaultProxyNameIsOneSegment(t *testing.T) {
	for name, origin := range New().Descriptor().DefaultUpstreams {
		if name == "" || strings.Contains(name, "/") {
			t.Errorf("proxy name %q is not a single segment", name)
		}
		if origin == "" {
			t.Errorf("proxy %q has no origin", name)
		}
	}
}
