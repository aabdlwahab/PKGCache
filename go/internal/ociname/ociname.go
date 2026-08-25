// Package ociname decides which registry the first segment of an image name refers to.
//
// A cache that fronts several registries has to put the registry in the path, because
// /v2/ is the only entry point Docker will ever ask for. The old answer was a table of
// aliases an operator had to extend — one line for Docker Hub, one for ghcr, one for
// quay, and a support ticket for every registry after that. This package replaces the
// table with a rule: a first segment that names a host is a registry, and the cache can
// reach it without being told about it first. nvcr.io, gcr.io, mcr.microsoft.com and
// public.ecr.aws all work the day someone pulls from them.
//
// The rule is Docker's own. `docker pull nvcr.io/nvidia/pytorch:24.01` is a registry
// pull and `docker pull nvidia/pytorch` is a Docker Hub one, and what separates them is
// exactly this: a dot (or a port, or the word localhost) in the first component. Reusing
// that test means a path this cache accepts and a reference Docker accepts always divide
// the same way — there is no third interpretation to get wrong.
//
// Everything here is syntax. Whether the cache may actually fetch from a registry it was
// not configured with is policy, and lives with the configuration that decides it.
package ociname

import (
	"net"
	"strconv"
	"strings"
)

// AnyRegistry is the upstream name a chain files its rule for discovered registries
// under — the cache in front of this one, which every discovered registry hangs off as
// one more path segment.
//
// It is not a name a client can ask for: "*" has no dot, so it never resolves as a
// registry host, and the OCI adapter refuses it as a path segment outright.
const AnyRegistry = "*"

// Registry is the registry one image-name segment refers to.
type Registry struct {
	// Segment is what this registry is called inside a cache path. It is the host for
	// anything discovered, and the historical alias for the three registries that had
	// one before discovery existed.
	Segment string
	// Host is the authority to fetch from, carrying a port when the segment did.
	Host string
	// Public is true for a routable DNS name: not an IP literal, not localhost, no
	// port. Only these are safe to reach without an operator saying so, because only
	// these are certain not to be an address that means something different to this
	// host than it does to the caller.
	Public bool
	// Library is true where single-segment repositories live under library/, which is
	// a Docker Hub convention and not part of the distribution spec.
	Library bool
}

// hub is Docker Hub, which is spelled more ways than any other registry.
//
// The pull endpoint is registry-1.docker.io while the name everyone writes is docker.io,
// so every spelling folds onto one segment. Two spellings resolving to two path segments
// would mean two cache namespaces, two inventories and the same layers fetched twice.
var hub = Registry{Segment: "dockerhub", Host: "registry-1.docker.io", Public: true, Library: true}

// wellKnown maps the spellings that do not resolve by themselves onto their registry.
//
// This is not the extensible list it replaced. Nothing needs to be added here for a new
// registry to work; entries exist only where a host and the segment naming it differ —
// Docker Hub's several names, and the two aliases this cache shipped before discovery,
// which keep working because paths and Dockerfiles in the wild already say them.
var wellKnown = map[string]Registry{
	"dockerhub":               hub,
	"docker.io":               hub,
	"index.docker.io":         hub,
	"registry-1.docker.io":    hub,
	"registry.hub.docker.com": hub,
	"ghcr":                    {Segment: "ghcr", Host: "ghcr.io", Public: true},
	"ghcr.io":                 {Segment: "ghcr", Host: "ghcr.io", Public: true},
	"quay":                    {Segment: "quay", Host: "quay.io", Public: true},
	"quay.io":                 {Segment: "quay", Host: "quay.io", Public: true},
}

// Lookup classifies one image-name segment. It reports false for a segment that names a
// repository rather than a registry, which is the common case: "library", "nvidia",
// "astral-sh".
func Lookup(segment string) (Registry, bool) {
	segment = strings.ToLower(strings.TrimSpace(segment))
	if reg, ok := wellKnown[segment]; ok {
		return reg, true
	}
	host, port, ok := splitHost(segment)
	if !ok {
		return Registry{}, false
	}
	return Registry{
		Segment: segment,
		Host:    segment,
		Public:  port == "" && host != "localhost" && !isIP(host) && strings.Contains(host, "."),
	}, true
}

// Origin is the URL a discovered registry is fetched from.
//
// Always https. A discovered registry was named by whoever ran the pull rather than by
// the operator, so this is not the place to let a path segment choose plaintext.
func (r Registry) Origin() string { return "https://" + r.Host }

// splitHost validates a segment as a host, optionally with a port.
//
// Deliberately strict about the characters, because this string ends up in a URL the
// cache will fetch: anything that could carry userinfo, a path, a query or a second
// authority past a naive parser is refused here rather than sanitised later.
func splitHost(segment string) (host, port string, ok bool) {
	host = segment
	if index := strings.LastIndex(segment, ":"); index >= 0 {
		host, port = segment[:index], segment[index+1:]
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", "", false
		}
	}
	if host == "localhost" {
		return host, port, true
	}
	// A host has to be dotted to be one. Without this, every Docker Hub namespace —
	// "nvidia/cuda", "astral-sh/uv" — would read as a registry, which is the same
	// mistake Docker's own reference parser avoids the same way.
	if !strings.Contains(host, ".") {
		return "", "", false
	}
	for _, label := range strings.Split(host, ".") {
		if !validLabel(label) {
			return "", "", false
		}
	}
	return host, port, true
}

func validLabel(label string) bool {
	if label == "" || len(label) > 63 ||
		strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// isIP reports whether a host is an address literal rather than a name.
//
// It matters because an address is not a name: 169.254.169.254 is cloud instance
// metadata, 10.x is somebody's private network, and a caller who chooses the first path
// segment must not be able to choose those.
func isIP(host string) bool { return net.ParseIP(host) != nil }
