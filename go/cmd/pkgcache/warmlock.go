package main

import (
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
	"github.com/aabdlwahab/PKGCache/internal/eco/pypi"
	"github.com/aabdlwahab/PKGCache/internal/local"
	"github.com/aabdlwahab/PKGCache/internal/lockwarm"
)

// `pkgcache warmlock` — pull a uv.lock's contents into the cache, and point the lock at it.
//
// A lock file is the one place that says exactly which bytes a project needs, which makes
// it the best possible thing to warm a cache from: no resolver to run, no guessing, and
// the same answer on every machine that shares the lock.
//
// Two halves, and the second is the one that pays off later. Warming fills the cache.
// Rewriting points the lock at the cache, so `uv sync` on a machine with no internet still
// resolves — the URLs in the file are the cache's own. uv verifies hashes either way, and
// the hashes are untouched, so a rewritten lock installs exactly the artefacts the
// original named or it fails.
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
		fmt.Fprint(fs.Output(), `pkgcache warmlock — warm the cache from a uv.lock, and point the lock at it

usage: pkgcache warmlock [flags]

Fetches every file the lock pins, so they are in this machine's cache, and then rewrites
the lock so its URLs name the cache instead of the internet. The original is kept beside
it as uv.lock.old.

The rewrite touches only URLs. Hashes, versions, ordering, comments and formatting are
unchanged, so `+"`diff uv.lock.old uv.lock`"+` shows exactly what moved — and uv still
verifies every hash, so a rewritten lock installs the same artefacts or fails.

-warm-only fills the cache and writes nothing, which is what a CI job wants when the lock
in the repository has to stay as it is.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
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
	packages, err := lockwarm.Parse(string(original))
	if err != nil {
		return fmt.Errorf("warmlock: %s: %w", *lockPath, err)
	}
	if len(packages) == 0 {
		// Not an error. A lock with only path or git sources has nothing a registry
		// cache can hold, and saying so beats writing an identical file back.
		fmt.Printf("%s pins no registry files; nothing to warm\n", *lockPath)
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
	err = lockwarm.Warm(ctx, daemonHandler(root), project, packages, indexes, *workers,
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
			failed.Load(), *lockPath)
	}
	if *warmOnly {
		return nil
	}

	rewritten := lockwarm.Rewrite(string(original), packages, indexes, base)
	if rewritten == string(original) {
		fmt.Printf("%s already points at this cache\n", *lockPath)
		return nil
	}
	backup := *lockPath + *suffix
	// Written before the lock is replaced, and refused if it would overwrite something.
	// The copy is the way back, and silently landing on an older one would take that away.
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf(
			"warmlock: %s already exists, so %s was left alone.\n"+
				"  Move it aside, or pass -backup-suffix to choose another name",
			backup, *lockPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(backup, original, 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", backup, err)
	}
	if err := os.WriteFile(*lockPath, []byte(rewritten), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", *lockPath, err)
	}
	fmt.Printf("%s now points at %s\n", *lockPath, base)
	fmt.Printf("the original is %s — `diff %s %s` shows only URLs\n",
		backup, backup, filepath.Base(*lockPath))
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
