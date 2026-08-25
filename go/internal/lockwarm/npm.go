// npm.go — the same two halves as uv.lock, for package-lock.json: warm every pinned
// tarball, then point the lock at the cache.
//
// npm records where each tarball came from in "resolved" and what it must hash to in
// "integrity". Only "resolved" is rewritten, so integrity is untouched and npm still
// verifies every byte — a rewritten lock installs the artefacts the original named, or
// it fails. That is the same bargain the uv side makes, for the same reason.

package lockwarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// NPMPackage is one registry tarball pinned by a lock file.
type NPMPackage struct {
	// Name is the package as npm addresses it, scope included: "@babel/core".
	Name string
	// Filename is the tarball's own name. It is not derivable from Name and version:
	// a scoped package drops its scope from the filename.
	Filename string
	// URL is the resolved URL exactly as the lock spells it, which is the token
	// RewriteNPM matches on.
	URL string
	// Registry is the root the tarball came from, so a caller can refuse a lock
	// pinned to a registry this cache does not stand in front of.
	Registry string
}

// ParseNPM collects every registry tarball a package-lock.json pins.
//
// The whole document is walked for "resolved" fields rather than reading a particular
// schema version's container. npm has moved these between "dependencies" (v1),
// both (v2), and "packages" (v3), and a v2 lock lists each tarball in both places; one
// version-agnostic walk with a dedupe covers all three and whatever comes next, which
// beats three parsers that each know one layout.
func ParseNPM(text string) ([]NPMPackage, error) {
	var document any
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		return nil, fmt.Errorf("lockwarm: not a valid package-lock.json: %w", err)
	}
	root, ok := document.(map[string]any)
	if !ok {
		return nil, errors.New("lockwarm: not a package-lock.json (not a JSON object)")
	}
	// The one field every lockfile version carries. Without it this is some other JSON
	// file, and saying so beats warming nothing and reporting success.
	if _, ok := root["lockfileVersion"]; !ok {
		return nil, errors.New("lockwarm: not a package-lock.json (no lockfileVersion)")
	}
	seen := make(map[string]bool)
	var packages []NPMPackage
	collectResolved(document, seen, &packages)
	return packages, nil
}

// collectResolved walks the document and records each distinct registry tarball.
func collectResolved(node any, seen map[string]bool, out *[]NPMPackage) {
	switch value := node.(type) {
	case map[string]any:
		if resolved, ok := value["resolved"].(string); ok && !seen[resolved] {
			// A workspace entry points at a directory in the tree rather than a
			// registry, and is marked as such. It has nothing to warm.
			if link, _ := value["link"].(bool); !link {
				if pkg, ok := splitTarballURL(resolved); ok {
					seen[resolved] = true
					*out = append(*out, pkg)
				}
			}
		}
		for _, child := range value {
			collectResolved(child, seen, out)
		}
	case []any:
		for _, child := range value {
			collectResolved(child, seen, out)
		}
	}
}

// splitTarballURL takes a resolved URL apart into the registry root, the package name
// and the tarball filename.
//
// Every npm registry — the public one, Verdaccio, Artifactory — serves tarballs at
// <root>/<name>/-/<filename>, so the "/-/" is the seam, and everything before it is the
// root plus the name. A leading "@" on the second-to-last segment is what distinguishes
// a scoped name from a root that happens to have a path prefix.
//
// Anything without that shape is not a registry tarball: git dependencies, "file:"
// paths and direct GitHub URLs all land here and are declined, because a cache that
// stands in front of a registry has nothing to say about them.
func splitTarballURL(resolved string) (NPMPackage, bool) {
	parsed, err := url.Parse(resolved)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return NPMPackage{}, false
	}
	marker := strings.LastIndex(parsed.Path, "/-/")
	if marker < 0 {
		return NPMPackage{}, false
	}
	filename := parsed.Path[marker+len("/-/"):]
	if decoded, err := url.PathUnescape(filename); err == nil {
		filename = decoded
	}
	if filename == "" || strings.Contains(filename, "/") {
		return NPMPackage{}, false
	}
	segments := strings.Split(strings.Trim(parsed.Path[:marker], "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return NPMPackage{}, false
	}
	nameFrom := len(segments) - 1
	if nameFrom > 0 && strings.HasPrefix(segments[nameFrom-1], "@") {
		nameFrom--
	}
	name := strings.Join(segments[nameFrom:], "/")
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if name == "" {
		return NPMPackage{}, false
	}
	root := parsed.Scheme + "://" + parsed.Host
	if prefix := strings.Join(segments[:nameFrom], "/"); prefix != "" {
		root += "/" + prefix
	}
	return NPMPackage{Name: name, Filename: filename, URL: resolved, Registry: root}, true
}

// NPMTarballPath is where this cache serves one tarball, below an npm base.
//
// It matches what the npm adapter writes into a rewritten packument, which is what npm
// would have followed had it gone through the cache to resolve. Building the same string
// two ways is a drift risk, so this is the one both the warm and the rewrite use.
func NPMTarballPath(name, filename string) string {
	segments := strings.Split(name, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return strings.Join(segments, "/") + "/-/" + url.PathEscape(filename)
}

// RewriteNPM points every pinned tarball at the cache, changing nothing else.
//
// Only the quoted URL token is replaced, so integrity hashes, versions, ordering and
// formatting survive byte-for-byte — which is what makes the .old file a diff somebody
// can read. json.Marshal produces the token the same way npm wrote it, so a URL that
// needed escaping still matches; one that somehow does not simply goes unrewritten
// rather than being corrupted.
func RewriteNPM(text string, packages []NPMPackage, base string) string {
	base = strings.TrimRight(base, "/")
	for _, pkg := range packages {
		from, err := json.Marshal(pkg.URL)
		if err != nil {
			continue
		}
		to, err := json.Marshal(base + "/" + NPMTarballPath(pkg.Name, pkg.Filename))
		if err != nil {
			continue
		}
		text = strings.ReplaceAll(text, string(from), string(to))
	}
	return text
}

// NPMRegistries lists the distinct registry roots a lock pins, in first-seen order.
// A caller checks these against what the cache serves before warming anything.
func NPMRegistries(packages []NPMPackage) []string {
	seen := make(map[string]bool, len(packages))
	var roots []string
	for _, pkg := range packages {
		if !seen[pkg.Registry] {
			seen[pkg.Registry] = true
			roots = append(roots, pkg.Registry)
		}
	}
	return roots
}
