package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco/npm"
	"github.com/aabdlwahab/PKGCache/internal/eco/pypi"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/lockwarm"
)

// `pkgcache warmlock` — pull a lock file's contents into the cache, and point the lock at it.
//
// Five formats: uv.lock, package-lock.json (npm-shrinkwrap.json with it), yarn.lock in
// both the v1 and berry shapes, and pnpm-lock.yaml. Which one it is is read from the
// bytes, not the filename.
//
// A lock file is the one place that says exactly which bytes a project needs, which makes
// it the best possible thing to warm a cache from: no resolver to run, no guessing, and
// the same answer on every machine that shares the lock.
//
// Two halves, and the second is the one that pays off later. Warming fills the cache.
// Rewriting points the lock at the cache, so `uv sync`, `npm ci` or `yarn install` on a
// machine with no internet still resolves — the URLs in the file are the cache's own. The
// tools verify hashes either way, and the hashes are untouched, so a rewritten lock
// installs exactly the artefacts the original named or it fails.
//
// Two of the formats cannot be pointed anywhere by editing them: yarn berry and pnpm
// write no URLs, only a package and a version, and decide the registry at install time
// from configuration. For those the first half still pays off — the cache is filled, and
// the one setting that has to change is named.
//
// The rewrite is textual and replaces only quoted URL tokens: formatting, ordering,
// comments and hashes survive byte-for-byte. That is what makes the .old file a diff
// somebody can read rather than a reformat they have to trust.

func runWarmlock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("warmlock", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindLocalFlags(fs)
	lockPath := fs.String("lock", "uv.lock", "the lock file to warm from")
	workers := fs.Int("workers", 8, "how many files to fetch at once")
	warmOnly := fs.Bool("warm-only", false, "fill the cache and leave the lock file alone")
	suffix := fs.String("backup-suffix", ".old", "what to call the copy of the original")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache warmlock — warm the cache from a lock file, and point the lock at it

usage: pkgcache warmlock [flags]

Fetches every artefact the lock pins, so they are in this machine's cache, and then
rewrites the lock so its URLs name the cache instead of the internet. The original is kept
beside it as <lock>.old.

Understands uv.lock, package-lock.json (and npm-shrinkwrap.json), yarn.lock in both its
v1 and berry forms, and pnpm-lock.yaml. The format is read from the file's contents, so
-lock may point at a copy under any name; with no -lock, the first lock that exists here
is used.

uv, npm and yarn v1 write the URL of every artefact into the lock, so those are rewritten.
Yarn berry and pnpm write no URLs at all — they take the registry from configuration — so
for those the cache is filled and the setting to change is named, and the lock is left
exactly as it was.

Where it rewrites, it touches only URLs. Hashes, versions, ordering, comments and
formatting are unchanged, so `+"`diff <lock>.old <lock>`"+` shows exactly what moved — and
every one of these tools still verifies its own hashes, so a rewritten lock installs the
same artefacts or fails.

-warm-only fills the cache and writes nothing, which is what a CI job wants when the lock
in the repository has to stay as it is.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// With no -lock, use whichever lock this directory actually has. Defaulting to
	// uv.lock and reporting it missing would be unhelpful in a project that has a
	// package-lock.json sitting right there.
	chosen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lock" {
			chosen = true
		}
	})
	if !chosen {
		*lockPath = lockFileHere(*lockPath)
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}

	// #nosec G304 -- the path is the caller's, and reading it is the whole command.
	original, err := os.ReadFile(*lockPath)
	if err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	// Which lock this is, decided by the bytes rather than the filename, so `-lock`
	// pointed at a copy under any name still does the right thing.
	if kind := detectJSLock(original); kind != nil {
		return warmJSLock(ctx, snap, kind, string(original), *lockPath,
			*workers, *warmOnly, *suffix)
	}
	return warmUVLock(ctx, snap, string(original), *lockPath, *workers, *warmOnly, *suffix)
}

// warmUVLock warms and rewrites a uv.lock.
func warmUVLock(
	ctx context.Context, snap *config.Snapshot,
	original, lockPath string, workers int, warmOnly bool, suffix string,
) error {
	packages, err := lockwarm.Parse(original)
	if err != nil {
		return fmt.Errorf("warmlock: %s: %w", lockPath, err)
	}
	if len(packages) == 0 {
		// Not an error. A lock with only path or git sources has nothing a registry
		// cache can hold, and saying so beats writing an identical file back.
		fmt.Printf("%s pins no registry files; nothing to warm\n", lockPath)
		return nil
	}

	// Somebody asking to warm a cache is asking for packages, so this starts it.
	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
	if err != nil {
		return err
	}
	project := local.CurrentProject(snap.DataDir)
	// Two addresses, and they are not the same one. Warm builds the whole path itself —
	// /<project>/pypi/<index>/+f/... — so its handler is given the daemon's root. Rewrite
	// is handed the project-scoped base, because what goes into the lock file has to be
	// the URL a client fetches. Conflating them makes every warm a 404.
	root := state.BaseURL()
	base := root + "/" + project + "/pypi"

	upstreams := pypi.New().Descriptor().DefaultUpstreams
	indexes := lockwarm.NewIndexMap(upstreams)
	// A lock this command has already rewritten names the cache, not the internet, and
	// would otherwise look like a registry nothing serves. Registering the cache's own
	// URLs against the same indexes makes such a lock warmable again — which is the point
	// on a second machine, where the lock arrives from the repository already pointing
	// here and the cache is empty.
	for index := range upstreams {
		indexes[base+"/"+index+"/+simple"] = index
	}

	// Every registry in the lock has to be one this cache serves before anything is
	// fetched. Warming half a lock and then failing would leave the file rewritten
	// against a cache that cannot answer for all of it.
	var unknown []string
	for _, pkg := range packages {
		if _, ok := indexes.Index(pkg.Registry); !ok {
			unknown = append(unknown, pkg.Registry)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf(
			"warmlock: this cache serves no index for %s\n"+
				"  Add it as a PyPI upstream, or use -warm-only and leave the lock alone",
			strings.Join(dedupe(unknown), ", "))
	}

	files := 0
	for _, pkg := range packages {
		files += len(pkg.Files)
	}
	fmt.Printf("warming %d file(s) from %d package(s) into %s\n",
		files, len(packages), project)

	var done, failed atomic.Int64
	err = lockwarm.Warm(ctx, daemonHandler(root), project, packages, indexes, workers,
		func(result lockwarm.Result) {
			switch {
			case result.Err != nil:
				failed.Add(1)
				fmt.Fprintf(os.Stderr, "  %s: %v\n", result.Filename, result.Err)
			case result.Status >= 400:
				failed.Add(1)
				fmt.Fprintf(os.Stderr, "  %s: HTTP %d\n", result.Filename, result.Status)
			default:
				done.Add(1)
			}
		})
	if err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	fmt.Printf("warmed %d, failed %d\n", done.Load(), failed.Load())
	if failed.Load() > 0 {
		// The lock is not rewritten when the cache does not hold all of it: a file
		// pointing at a cache that 404s for one wheel is worse than one pointing at the
		// internet, because the failure arrives later and further from here.
		return fmt.Errorf(
			"warmlock: %d file(s) did not warm, so %s was left alone",
			failed.Load(), lockPath)
	}
	if warmOnly {
		return nil
	}

	rewritten := lockwarm.Rewrite(original, packages, indexes, base)
	if rewritten == original {
		fmt.Printf("%s already points at this cache\n", lockPath)
		return nil
	}
	backup := lockPath + suffix
	// Written before the lock is replaced, and refused if it would overwrite something.
	// The copy is the way back, and silently landing on an older one would take that away.
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf(
			"warmlock: %s already exists, so %s was left alone.\n"+
				"  Move it aside, or pass -backup-suffix to choose another name",
			backup, lockPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(backup, []byte(original), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", backup, err)
	}
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", lockPath, err)
	}
	fmt.Printf("%s now points at %s\n", lockPath, base)
	fmt.Printf("the original is %s — `diff %s %s` shows only URLs\n",
		backup, backup, filepath.Base(lockPath))
	return nil
}

// daemonHandler adapts the running cache to the http.Handler lockwarm warms through.
//
// lockwarm was written for pkgreg, where the data plane is in the same process and a
// handler is the natural thing to hand it. Here the cache is a separate process on
// loopback, so this is the same interface over a real request — which keeps one warming
// implementation rather than two that would drift.
func daemonHandler(base string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), r.Method,
			strings.TrimRight(base, "/")+r.URL.RequestURI(), nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		w.WriteHeader(response.StatusCode)
		// Read to completion and discard: the point is that the cache commits the blob,
		// not that this process keeps it.
		_, _ = io.Copy(io.Discard, response.Body)
	})
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// jsLock is one JavaScript lock format: how to read it, and whether it can be pointed
// at the cache by editing it.
type jsLock struct {
	// name is what to call it in messages.
	name string
	// parse reads the registry packages the lock pins.
	parse func(string) ([]lockwarm.NPMPackage, error)
	// rewrite points those at the cache. Nil when the format writes no URLs, which is
	// not a gap in this command: there is nothing in such a file to change.
	rewrite func(string, []lockwarm.NPMPackage, string) string
	// pointAt names the setting that decides where the tool actually fetches from, for
	// the formats where that is the answer instead of a rewrite.
	pointAt string
}

// detectJSLock identifies a JavaScript lock from its contents, or returns nil if these
// bytes are not one — in which case the caller treats them as a uv.lock.
//
// Decided by shape, not by filename: a lock copied to another name is still the format
// it is, and yarn's two eras share the name yarn.lock while sharing nothing else.
func detectJSLock(content []byte) *jsLock {
	text := string(content)
	switch {
	case bytes.HasPrefix(bytes.TrimLeft(content, " \t\r\n\ufeff"), []byte("{")):
		// npm-shrinkwrap.json is the same document under another name and lands here.
		return &jsLock{
			name:    "package-lock.json",
			parse:   lockwarm.ParseNPM,
			rewrite: lockwarm.RewriteNPM,
		}
	case strings.Contains(text, "yarn lockfile v1"):
		return &jsLock{
			name:    "yarn.lock (v1)",
			parse:   lockwarm.ParseYarnClassic,
			rewrite: lockwarm.RewriteYarnClassic,
		}
	case strings.Contains(text, "__metadata:"):
		return &jsLock{
			name:    "yarn.lock (berry)",
			parse:   lockwarm.ParseYarnBerry,
			pointAt: "npmRegistryServer in .yarnrc.yml",
		}
	case regexp.MustCompile(`(?m)^lockfileVersion:`).MatchString(text):
		return &jsLock{
			name:    "pnpm-lock.yaml",
			parse:   lockwarm.ParsePNPM,
			pointAt: "the registry pnpm is configured with",
		}
	}
	return nil
}

// npmRegistryAliases are hostnames that serve the public npm registry under another
// name, and so are answerable by a cache standing in front of it.
//
// registry.yarnpkg.com is yarn's own alias for registry.npmjs.org — the same registry,
// which is why a yarn lock's URLs point there by default. Refusing it would make yarn
// support useless out of the box. The bet is safe because it is checked: every one of
// these locks carries integrity hashes that the installing tool verifies, so if the two
// ever served different bytes the install would fail rather than quietly succeed.
var npmRegistryAliases = []string{"https://registry.yarnpkg.com"}

// warmJSLock warms a JavaScript lock, and rewrites it when the format has URLs to move.
func warmJSLock(
	ctx context.Context, snap *config.Snapshot, kind *jsLock,
	original, lockPath string, workers int, warmOnly bool, suffix string,
) error {
	packages, err := kind.parse(original)
	if err != nil {
		return fmt.Errorf("warmlock: %s: %w", lockPath, err)
	}
	if len(packages) == 0 {
		// A lock with only workspace links, git or file: dependencies pins nothing a
		// registry cache can hold. Saying so beats writing an identical file back.
		fmt.Printf("%s pins no registry tarballs; nothing to warm\n", lockPath)
		return nil
	}

	state, err := local.Ensure(ctx, local.EnsureOptions{Snapshot: snap, Notes: os.Stderr})
	if err != nil {
		return err
	}
	project := local.CurrentProject(snap.DataDir)
	root := state.BaseURL()
	base := root + "/" + project + "/npm"

	// Every registry the lock names has to be one this cache stands in front of, checked
	// before a single byte is fetched. Warming half a lock and then failing would leave
	// the file rewritten against a cache that cannot answer for all of it.
	//
	// The cache's own base counts as known, which is what makes a lock that has already
	// been rewritten warmable again — the case that matters on a second machine, where
	// the lock arrives from the repository already pointing here and the cache is empty.
	served := map[string]bool{base: true}
	for _, upstream := range npm.New().Descriptor().DefaultUpstreams {
		served[strings.TrimRight(upstream, "/")] = true
	}
	for _, alias := range npmRegistryAliases {
		served[alias] = true
	}
	var unknown []string
	for _, registry := range lockwarm.NPMRegistries(packages) {
		if !served[registry] {
			unknown = append(unknown, registry)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf(
			"warmlock: this cache serves no npm registry for %s\n"+
				"  Point the npm upstream at it, or use -warm-only and leave the lock alone",
			strings.Join(dedupe(unknown), ", "))
	}

	// Both counts, because they differ and the second is the one the tally below uses:
	// a lock often pins two versions of a package, and one packument covers both.
	names := make(map[string]bool, len(packages))
	for _, pkg := range packages {
		names[pkg.Name] = true
	}
	requests := len(names) + len(packages)
	fmt.Printf("%s: warming %d package(s) into %s — %d requests, metadata and tarballs\n",
		kind.name, len(packages), project, requests)

	var done, failed atomic.Int64
	err = lockwarm.WarmNPM(ctx, daemonHandler(root), project, packages, workers,
		func(result lockwarm.Result) {
			switch {
			case result.Err != nil:
				failed.Add(1)
				fmt.Fprintf(os.Stderr, "  %s: %v\n", result.Filename, result.Err)
			case result.Status >= 400:
				failed.Add(1)
				fmt.Fprintf(os.Stderr, "  %s: HTTP %d\n", result.Filename, result.Status)
			default:
				done.Add(1)
			}
		})
	if err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	fmt.Printf("warmed %d of %d, failed %d\n", done.Load(), requests, failed.Load())
	if failed.Load() > 0 {
		return fmt.Errorf(
			"warmlock: %d request(s) did not warm, so %s was left alone",
			failed.Load(), lockPath)
	}

	// Two of the four formats write no tarball URL: yarn berry resolves a descriptor and
	// pnpm stores only an integrity hash, and both take the registry from configuration.
	// There is nothing in those files to point anywhere, so the cache is filled and the
	// setting that decides where the tool fetches from is named instead. Verified against
	// both tools installing from this cache with it offline.
	if kind.rewrite == nil {
		fmt.Printf("%s names no tarball URLs, so it is unchanged.\n", kind.name)
		fmt.Printf("  Set %s to %s to install what was just warmed.\n", kind.pointAt, base)
		return nil
	}
	if warmOnly {
		return nil
	}

	rewritten := kind.rewrite(original, packages, base)
	if rewritten == original {
		fmt.Printf("%s already points at this cache\n", lockPath)
		return nil
	}
	return writeRewrittenLock(lockPath, original, rewritten, suffix, base)
}

// writeRewrittenLock replaces the lock, keeping the original beside it.
//
// The copy is written first and never over something that is already there: it is the
// way back, and silently landing on an older one would take that away.
func writeRewrittenLock(lockPath, original, rewritten, suffix, base string) error {
	backup := lockPath + suffix
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf(
			"warmlock: %s already exists, so %s was left alone.\n"+
				"  Move it aside, or pass -backup-suffix to choose another name",
			backup, lockPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(backup, []byte(original), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", backup, err)
	}
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", lockPath, err)
	}
	fmt.Printf("%s now points at %s\n", lockPath, base)
	fmt.Printf("the original is %s — `diff %s %s` shows only URLs\n",
		backup, backup, filepath.Base(lockPath))
	return nil
}

// lockFileHere picks the lock to use when the caller named none.
//
// Falls back to the given default so a directory with no lock at all still produces the
// familiar "no such file" naming uv.lock, rather than naming whichever candidate happened
// to be checked last.
func lockFileHere(fallback string) string {
	for _, candidate := range []string{
		"uv.lock", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return fallback
}
