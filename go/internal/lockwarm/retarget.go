package lockwarm

import (
	"net"
	"net/url"
	"slices"
	"strings"
)

// CacheRegistry reports the cache base behind a registry URL that a previous rewrite
// produced, given the index names this cache serves, and whether it was one.
//
// This is what makes an already-rewritten lock workable again. A pkgcache daemon takes
// whatever port it is given when it starts, so the address in a lock rewritten yesterday
// is not the address serving today. Without recognising it, such a lock is
// indistinguishable from one naming a registry the cache was never configured for, and
// the only advice possible — add an upstream — is advice that cannot help.
//
// Two things make the recognition safe. The +simple sentinel is this server's own
// spelling and no real index serves a path segment beginning with "+", and the part
// before it has to be an index this cache actually has an upstream for. What remains in
// front is taken as the base whatever its shape, because that shape has varied: a
// pkgcache writes <host>/<project>/pypi, a pkgreg deployment may mount the indexes at the
// root, and locks from both are still out there.
func CacheRegistry(registry string, served map[string]string) (base, index string, ok bool) {
	rest, found := strings.CutSuffix(strings.TrimRight(registry, "/"), "/+simple")
	if !found {
		return "", "", false
	}
	// Longest name first: an index called root/pypi must win over one called pypi when
	// the cache happens to serve both, or the base comes back with a segment too many.
	names := make([]string, 0, len(served))
	for name := range served {
		names = append(names, strings.Trim(name, "/"))
	}
	slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })
	for _, name := range names {
		base, found := strings.CutSuffix(rest, "/"+name)
		if found && strings.Contains(base, "://") {
			return base, name, true
		}
	}
	return "", "", false
}

// LocalCache reports whether a cache base is one on this machine.
//
// The distinction decides whether a stale address may be repaired without being asked. A
// loopback address cannot have meant another machine, so a lock naming one that has moved
// is stale by definition and re-pointing it is the only useful thing to do. A routable
// address is the opposite: it is most likely a shared pkgreg somebody chose deliberately,
// and quietly dragging that lock back to this laptop would be a worse failure than
// refusing to touch it.
func LocalCache(base string) bool {
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// Adopt teaches the map that a registry URL is served by a named index, so a lock already
// pointing at a cache resolves the same way a pristine one does.
func (m IndexMap) Adopt(registry, index string) {
	m[strings.TrimRight(registry, "/")] = index
}
