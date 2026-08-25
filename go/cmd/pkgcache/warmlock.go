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
// Two formats: uv.lock, and npm's package-lock.json (npm-shrinkwrap.json with it). Which
// one it is is read from the bytes, not the filename.
//
// A lock file is the one place that says exactly which bytes a project needs, which makes
// it the best possible thing to warm a cache from: no resolver to run, no guessing, and
// the same answer on every machine that shares the lock.
//
// Two halves, and the second is the one that pays off later. Warming fills the cache.
// Rewriting points the lock at the cache, so `uv sync` or `npm ci` on a machine with no
// internet still resolves — the URLs in the file are the cache's own. Both tools verify
// hashes either way, and the hashes are untouched, so a rewritten lock installs exactly
// the artefacts the original named or it fails.
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
		fmt.Fprint(fs.Output(), `pkgcache warmlock — warm the cache from a lock file, and point the lock at it

usage: pkgcache warmlock [flags]

Fetches every artefact the lock pins, so they are in this machine's cache, and then
rewrites the lock so its URLs name the cache instead of the internet. The original is kept
beside it as <lock>.old.

Understands uv.lock and npm's package-lock.json (and npm-shrinkwrap.json). The format is
read from the file's contents, so -lock may point at a copy under any name. With no -lock,
the first of uv.lock, package-lock.json or npm-shrinkwrap.json that exists here is used.

The rewrite touches only URLs. Hashes, versions, ordering, comments and formatting are
unchanged, so `+"`diff <lock>.old <lock>`"+` shows exactly what moved — and uv and npm
both still verify every hash, so a rewritten lock installs the same artefacts or fails.

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
	if isNPMLock(original) {
		return warmNPMLock(ctx, snap, string(original), *lockPath, *workers, *warmOnly, *suffix)
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
	if rewritten == string(original) {
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

// isNPMLock reports whether these bytes are a package-lock.json rather than a uv.lock.
//
// Decided by shape, not by filename: the two formats could not look less alike, and a
// lock copied to another name is still the format it is. npm-shrinkwrap.json is the same
// document under a different name and lands here too.
func isNPMLock(content []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(content, " \t\r\n\ufeff"), []byte("{"))
}

// warmNPMLock warms and rewrites a package-lock.json.
//
// The same two halves as the uv path, and the same refusal to rewrite a lock the cache
// cannot fully answer for. What differs is the address: npm has a single upstream, so a
// tarball is addressed by name below the project's npm base with no index in the path.
func warmNPMLock(
	ctx context.Context, snap *config.Snapshot,
	original, lockPath string, workers int, warmOnly bool, suffix string,
) error {
	packages, err := lockwarm.ParseNPM(original)
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

	fmt.Printf("warming %d tarball(s) into %s\n", len(packages), project)

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
	fmt.Printf("warmed %d, failed %d\n", done.Load(), failed.Load())
	if failed.Load() > 0 {
		return fmt.Errorf(
			"warmlock: %d tarball(s) did not warm, so %s was left alone",
			failed.Load(), lockPath)
	}
	if warmOnly {
		return nil
	}

	rewritten := lockwarm.RewriteNPM(original, packages, base)
	if rewritten == original {
		fmt.Printf("%s already points at this cache\n", lockPath)
		return nil
	}
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
	for _, candidate := range []string{"uv.lock", "package-lock.json", "npm-shrinkwrap.json"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return fallback
}
