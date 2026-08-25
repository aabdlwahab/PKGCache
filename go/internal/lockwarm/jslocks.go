// jslocks.go — the three JavaScript lock formats that are not package-lock.json.
//
// They divide by whether the lock says where a tarball came from:
//
//   - yarn classic writes `resolved "<url>#<sha1>"`, so it rewrites like npm's does.
//     The fragment is a checksum yarn actually verifies, so it has to survive.
//
//   - yarn berry writes `resolution: "pkg@npm:1.2.3"` and pnpm writes a `pkg@1.2.3`
//     key with only an integrity hash. Neither names a URL at all: both derive it from
//     the registry in their configuration. There is nothing in those files to rewrite,
//     so warming is the whole of what can be done for them, and pointing the tool's
//     registry at this cache is what makes the warmed copy get used. Verified against
//     both tools with the cache offline.
//
// Everything here derives the tarball name the way the npm registry does — for a scoped
// package the scope is dropped from the filename — because that is the only way to
// address a tarball for a lock that never wrote its URL down.

package lockwarm

import (
	"regexp"
	"strings"
)

var (
	// yarn classic: two-space-indented `resolved "<url>"` inside an entry block.
	yarnResolvedRE = regexp.MustCompile(`(?m)^\s+resolved\s+"([^"]+)"\s*$`)
	// yarn berry: `resolution: "<descriptor>"`, the one field that names the package.
	berryResolutionRE = regexp.MustCompile(`(?m)^\s+resolution:\s+"([^"]+)"\s*$`)
	// pnpm: the keys of the packages: block, at exactly two spaces of indent.
	pnpmEntryRE = regexp.MustCompile(`(?m)^  '?([^'\s][^:]*?)'?:\s*$`)
)

// ParseYarnClassic collects every registry tarball a yarn v1 lock pins.
func ParseYarnClassic(text string) ([]NPMPackage, error) {
	if !strings.Contains(text, "yarn lockfile v1") {
		return nil, errNotFormat("a yarn v1 lock", "no `# yarn lockfile v1` header")
	}
	seen := make(map[string]bool)
	var packages []NPMPackage
	for _, match := range yarnResolvedRE.FindAllStringSubmatch(text, -1) {
		token := match[1]
		if seen[token] {
			continue
		}
		// The fragment is yarn's own sha1 check. Split it off to read the path, and
		// carry it so the rewrite can put it back exactly as it was.
		address, fragment := token, ""
		if hash := strings.Index(token, "#"); hash >= 0 {
			address, fragment = token[:hash], token[hash:]
		}
		pkg, ok := splitTarballURL(address)
		if !ok {
			continue
		}
		seen[token] = true
		pkg.Fragment = fragment
		packages = append(packages, pkg)
	}
	return packages, nil
}

// RewriteYarnClassic points every pinned tarball at the cache, fragment intact.
//
// Yarn quotes these plainly rather than as JSON, so the token is replaced as written.
// Only the URL moves: versions, integrity lines, dependency blocks, comments and the
// file's ordering are untouched.
func RewriteYarnClassic(text string, packages []NPMPackage, base string) string {
	base = strings.TrimRight(base, "/")
	for _, pkg := range packages {
		if pkg.URL == "" {
			continue
		}
		from := `"` + pkg.URL + pkg.Fragment + `"`
		to := `"` + base + "/" + NPMTarballPath(pkg.Name, pkg.Filename) + pkg.Fragment + `"`
		text = strings.ReplaceAll(text, from, to)
	}
	return text
}

// ParseYarnBerry collects every registry package a yarn 2+ lock pins.
//
// Berry's resolution is a descriptor rather than a URL: "@babel/core@npm:7.24.7". Only
// the npm: protocol is a registry package — workspace:, patch:, link:, portal: and file:
// all name something local, and a git or https descriptor names something this cache
// does not stand in front of.
func ParseYarnBerry(text string) ([]NPMPackage, error) {
	if !strings.Contains(text, "__metadata:") {
		return nil, errNotFormat("a yarn berry lock", "no __metadata block")
	}
	seen := make(map[string]bool)
	var packages []NPMPackage
	for _, match := range berryResolutionRE.FindAllStringSubmatch(text, -1) {
		descriptor := match[1]
		if seen[descriptor] {
			continue
		}
		name, version, ok := splitBerryDescriptor(descriptor)
		if !ok {
			continue
		}
		seen[descriptor] = true
		packages = append(packages, NPMPackage{
			Name: name, Version: version, Filename: tarballName(name, version),
		})
	}
	return packages, nil
}

// splitBerryDescriptor reads "<name>@npm:<version>" into its two halves.
func splitBerryDescriptor(descriptor string) (name, version string, ok bool) {
	const protocol = "@npm:"
	at := strings.LastIndex(descriptor, protocol)
	if at <= 0 {
		return "", "", false
	}
	name = descriptor[:at]
	version = descriptor[at+len(protocol):]
	// Berry appends bindings to a descriptor when a package is instantiated more than
	// once — peer resolutions, an archive override. The version ends where they begin.
	if bindings := strings.Index(version, "::"); bindings >= 0 {
		version = version[:bindings]
	}
	if open := strings.Index(version, "("); open >= 0 {
		version = version[:open]
	}
	if name == "" || version == "" || !looksLikeVersion(version) {
		return "", "", false
	}
	return name, version, true
}

// ParsePNPM collects every registry package a pnpm lock pins.
//
// The keys of the packages: block carry name and version, in a shape that has changed
// across lockfile versions: "name/1.2.3" in v5, "/name@1.2.3" in v6, "name@1.2.3" in v9.
// All three are read, because a repository's lock is whatever the pnpm that wrote it
// produced and none of those versions are unusual to meet.
func ParsePNPM(text string) ([]NPMPackage, error) {
	if !regexp.MustCompile(`(?m)^lockfileVersion:`).MatchString(text) {
		return nil, errNotFormat("a pnpm lock", "no lockfileVersion at the top level")
	}
	// Only the packages: block. importers: and snapshots: repeat the same keys with
	// different meaning, and the root importer's keys are specifiers, not versions.
	block := text
	if _, after, found := strings.Cut(text, "\npackages:\n"); found {
		block = after
		if before, _, found := strings.Cut(block, "\nsnapshots:"); found {
			block = before
		}
	}
	seen := make(map[string]bool)
	var packages []NPMPackage
	for _, match := range pnpmEntryRE.FindAllStringSubmatch(block, -1) {
		name, version, ok := splitPNPMKey(match[1])
		if !ok || seen[name+"@"+version] {
			continue
		}
		seen[name+"@"+version] = true
		packages = append(packages, NPMPackage{
			Name: name, Version: version, Filename: tarballName(name, version),
		})
	}
	return packages, nil
}

// splitPNPMKey reads a packages: key into name and version.
func splitPNPMKey(key string) (name, version string, ok bool) {
	key = strings.TrimPrefix(strings.TrimSpace(key), "/")
	// A package instantiated against particular peers carries them in parentheses.
	if open := strings.Index(key, "("); open >= 0 {
		key = key[:open]
	}
	if key == "" {
		return "", "", false
	}
	// v6 and v9 join with "@", which is also the scope marker, so the join is the last
	// "@" that is not the leading one. v5 joined with "/" instead.
	if at := strings.LastIndex(key, "@"); at > 0 {
		name, version = key[:at], key[at+1:]
	} else if slash := strings.LastIndex(key, "/"); slash > 0 {
		name, version = key[:slash], key[slash+1:]
	} else {
		return "", "", false
	}
	if name == "" || !looksLikeVersion(version) {
		return "", "", false
	}
	return name, version, true
}

// tarballName is what the npm registry calls a package's tarball: the name with any
// scope dropped, the version, and .tgz.
func tarballName(name, version string) string {
	base := name
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		base = name[slash+1:]
	}
	return base + "-" + version + ".tgz"
}

// looksLikeVersion keeps non-registry sources out on the way past.
//
// A lock's keys and descriptors also carry "workspace:.", "link:../x", "file:y.tgz" and
// full URLs, none of which are a version. Requiring a leading digit is enough to tell
// them apart and does not exclude any published npm version, which must be semver.
func looksLikeVersion(value string) bool {
	if value == "" || value[0] < '0' || value[0] > '9' {
		return false
	}
	return !strings.ContainsAny(value, ":/ ")
}

// errNotFormat keeps the "this is not that file" messages consistent, since pointing
// -lock at the wrong file is the likeliest way to arrive here.
func errNotFormat(what, why string) error {
	return &formatError{what: what, why: why}
}

type formatError struct{ what, why string }

func (e *formatError) Error() string {
	return "lockwarm: not " + e.what + " (" + e.why + ")"
}
