package feed

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Writing the whole repository, which is the only operation an operator performs.
//
// Everything above this is a piece: read a package, render an index, hash it into a
// Release, sign that. This is the one function that does them in the right order, because
// the order is the trust chain and getting it wrong produces a repository that looks
// complete and verifies as nothing.

// RepoOptions describes a repository to write.
type RepoOptions struct {
	// Root is the directory the repository lives in — served as-is over HTTP, so its
	// layout is its API.
	Root string
	// Debs are the package files to publish. Read, hashed and copied into the pool.
	Debs []string
	// Key signs the Release. Required: an unsigned repository is one apt will refuse
	// without `[trusted=yes]`, and anybody who writes that has turned off the only check
	// standing between them and an arbitrary package.
	Key *PGPKey

	Origin      string
	Label       string
	Suite       string
	Component   string
	Description string
	// Date fixes the publication time. Zero means now; a test sets it.
	Date time.Time
	// ValidUntil is how long apt should accept this Release. Zero leaves it out, which is
	// what an air-gapped machine needs.
	ValidUntil time.Duration
}

// RepoResult reports what was written.
type RepoResult struct {
	// Packages is one entry per published .deb, in index order.
	Packages []DebPackage
	// Architectures is what was found among them, sorted.
	Architectures []string
	// Files is every path written, relative to Root, sorted. `pkgreg doctor` walks it.
	Files []string
	// Fingerprint is the key the Release was signed with.
	Fingerprint string
}

// WriteRepository publishes packages into an apt repository.
//
// Idempotent: running it twice over the same inputs produces the same bytes, which is what
// makes "has the repository changed?" a question with an answer.
func WriteRepository(options RepoOptions) (RepoResult, error) {
	if options.Root == "" {
		return RepoResult{}, fmt.Errorf("feed: a repository needs a root directory")
	}
	if options.Key == nil {
		return RepoResult{}, fmt.Errorf(
			"feed: a repository must be signed.\n" +
				"  An unsigned one needs [trusted=yes] on every client, which turns off the\n" +
				"  only check between an operator and an arbitrary package")
	}
	suite := options.Suite
	if suite == "" {
		suite = "stable"
	}
	component := options.Component
	if component == "" {
		component = "main"
	}

	// ---- read every package before writing anything ------------------------------
	//
	// A half-written repository is worse than none: apt caches indexes, so a client that
	// fetched the broken state keeps it until something changes again. One unreadable
	// .deb stops the whole publish.
	byArch := make(map[string][]DebPackage)
	var all []DebPackage
	for _, source := range options.Debs {
		pkg, err := ReadDeb(source)
		if err != nil {
			return RepoResult{}, err
		}
		name, arch := pkg.Get("Package"), pkg.Get("Architecture")
		if name == "" || arch == "" {
			return RepoResult{}, fmt.Errorf(
				"feed: %s has no Package or Architecture field; it cannot be indexed",
				filepath.Base(source))
		}
		// The source name is what the pool fans out on, and it is the source package's
		// name where there is one — pkgcache and pkgcache-desktop share a pool directory
		// because they are built from one source.
		poolName := pkg.Get("Source")
		if poolName == "" {
			poolName = name
		}
		pkg.Filename = PoolPath(component, poolName, filepath.Base(source))
		pkg.source = source
		byArch[arch] = append(byArch[arch], pkg)
		all = append(all, pkg)
	}

	architectures := make([]string, 0, len(byArch))
	for arch := range byArch {
		architectures = append(architectures, arch)
	}
	sort.Strings(architectures)

	// Stable order within an architecture, so the index is byte-identical between runs.
	for _, arch := range architectures {
		list := byArch[arch]
		sort.Slice(list, func(i, j int) bool {
			if list[i].Get("Package") != list[j].Get("Package") {
				return list[i].Get("Package") < list[j].Get("Package")
			}
			return list[i].Get("Version") < list[j].Get("Version")
		})
		byArch[arch] = list
	}

	var written []string
	write := func(relative string, body []byte) error {
		full := filepath.Join(options.Root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("feed: %w", err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil { // #nosec G306 -- served over HTTP.
			return fmt.Errorf("feed: %w", err)
		}
		written = append(written, relative)
		return nil
	}

	// ---- the pool ----------------------------------------------------------------
	for _, pkg := range all {
		// #nosec G304 -- the operator named this file on the command line.
		body, err := os.ReadFile(pkg.source)
		if err != nil {
			return RepoResult{}, fmt.Errorf("feed: %w", err)
		}
		if err := write(pkg.Filename, body); err != nil {
			return RepoResult{}, err
		}
	}

	// ---- the indexes, and the Release that vouches for them ----------------------
	distDir := path.Join("dists", suite)
	var indexes []IndexFile
	for _, arch := range architectures {
		index := PackagesIndex(byArch[arch])
		gzipped, err := Gzip(index)
		if err != nil {
			return RepoResult{}, fmt.Errorf("feed: compress index: %w", err)
		}
		plain := path.Join(component, "binary-"+arch, "Packages")
		indexes = append(indexes,
			IndexFile{Path: plain, Body: index},
			IndexFile{Path: plain + ".gz", Body: gzipped},
		)
	}
	for _, index := range indexes {
		if err := write(path.Join(distDir, index.Path), index.Body); err != nil {
			return RepoResult{}, err
		}
	}

	release := ReleaseFile(ReleaseOptions{
		Origin:        orDefault(options.Origin, "pkgreg"),
		Label:         orDefault(options.Label, "pkgcache"),
		Suite:         suite,
		Codename:      suite,
		Components:    []string{component},
		Architectures: architectures,
		Description:   options.Description,
		Date:          options.Date,
		ValidUntil:    options.ValidUntil,
	}, indexes)

	// Release, then the two signatures over it. In this order because each depends on the
	// bytes of the one before, and a Release rewritten after being signed is a repository
	// whose signature covers a file that no longer exists.
	if err := write(path.Join(distDir, "Release"), release); err != nil {
		return RepoResult{}, err
	}
	inRelease, err := options.Key.ClearSign(release)
	if err != nil {
		return RepoResult{}, err
	}
	if err := write(path.Join(distDir, "InRelease"), inRelease); err != nil {
		return RepoResult{}, err
	}
	detached, err := options.Key.DetachSign(release)
	if err != nil {
		return RepoResult{}, err
	}
	if err := write(path.Join(distDir, "Release.gpg"), detached); err != nil {
		return RepoResult{}, err
	}

	// The public key, beside the repository it verifies. A client needs it before it can
	// trust anything here, and making them find it elsewhere is how people end up running
	// `[trusted=yes]` instead.
	public, err := options.Key.ArmoredPublic()
	if err != nil {
		return RepoResult{}, err
	}
	if err := write("pkgcache-archive-keyring.asc", public); err != nil {
		return RepoResult{}, err
	}

	sort.Strings(written)
	return RepoResult{
		Packages:      all,
		Architectures: architectures,
		Files:         written,
		Fingerprint:   options.Key.Fingerprint(),
	}, nil
}

// SourcesLine is the line a client puts in /etc/apt/sources.list.d.
//
// deb822 rather than the one-line form: it is what current apt documents, it names the
// keyring explicitly instead of trusting every key on the system, and it has somewhere to
// put a comment saying where it came from.
func SourcesLine(origin, suite, component string) string {
	if suite == "" {
		suite = "stable"
	}
	if component == "" {
		component = "main"
	}
	return strings.Join([]string{
		"# Written by pkgcache setup. Remove this file to stop using this repository.",
		"Types: deb",
		"URIs: " + strings.TrimSuffix(origin, "/"),
		"Suites: " + suite,
		"Components: " + component,
		"Signed-By: /usr/share/keyrings/pkgcache-archive-keyring.asc",
		"",
	}, "\n")
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// Where an instance keeps its repository and the key that signs it.
//
// Siblings, not parent and child, and that is the whole point: RepoDir is served over
// HTTP as a directory tree, so a signing key anywhere beneath it would be one path
// traversal bug away from being downloadable. Keeping it outside means no handler can
// reach it however wrong the handler is.

// RepoDir is the apt repository's root, served as static files.
func RepoDir(dataDir string) string { return filepath.Join(dataDir, "apt", "repo") }

// KeyPath is the private signing key. Never inside RepoDir.
func KeyPath(dataDir string) string {
	return filepath.Join(dataDir, "apt", "signing-key.asc")
}
