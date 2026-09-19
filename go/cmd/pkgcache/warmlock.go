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
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	eager := fs.Bool("eager", false, "fetch the distribution files too, not just the indexes")
	target := fs.String("target", "auto",
		"which platform -eager fetches wheels for: auto, all, or <os>/<arch>[/<python>]")
	retarget := fs.Bool("retarget", false,
		"re-point a lock that names another machine's cache at this one")
	recursive := fs.Bool("recursive", false,
		"warm every lock file below this directory, not just the one here")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache warmlock — warm the cache from a lock file, and point the lock at it

usage: pkgcache warmlock [flags]
       pkgcache warmlock -recursive [dir]

Makes this machine's cache able to serve everything a lock pins, and then rewrites the
lock so its URLs name the cache instead of the internet. The original is kept beside it
as <lock>.old.

Understands uv.lock, package-lock.json (and npm-shrinkwrap.json), yarn.lock in both its
v1 and berry forms, and pnpm-lock.yaml. The format is read from the file's contents, so
-lock may point at a copy under any name; with no -lock, the first lock that exists here
is used.

-recursive does every lock below a directory instead — the whole of a monorepo in one
command, whatever mix of formats it uses, taking every lock in a directory rather than
the first. It skips node_modules, vendored and virtualenv trees and dot directories,
whose locks belong to somebody else. One lock failing does not stop the rest; they are
named together at the end, and the command still exits non-zero.

For uv.lock, what is fetched up front is the index page of each package — one small
document apiece, and the thing the cache has to read before it can serve any file. The
distributions then arrive the first time something asks for them, through the same cache.
That is the right trade for a machine with a network: a uv.lock pins one version per
package but lists every file the index holds for it, across every platform and Python, so
fetching them all moves far more than any one machine installs.

-eager fetches the distributions too, which is what a machine that will not have a
network later actually needs. It fetches only the files -target can install:

  -target auto              this machine, and the Python in ./.venv if there is one
  -target all               every file, every platform — the old behaviour
  -target linux/x86_64/cp312
  -target macos/arm64       every Python, that platform
  -target musllinux/aarch64

uv, npm and yarn v1 write the URL of every artefact into the lock, so those are rewritten.
Yarn berry and pnpm write no URLs at all — they take the registry from configuration — so
for those the cache is filled and the setting to change is named, and the lock is left
exactly as it was.

Where it rewrites, it touches only URLs. Hashes, versions, ordering, comments and
formatting are unchanged, so `+"`diff <lock>.old <lock>`"+` shows exactly what moved — and
every one of these tools still verifies its own hashes, so a rewritten lock installs the
same artefacts or fails.

A lock this command rewrote before names a cache at the port the daemon had that day, and
the daemon takes a new one every time it starts. Such a lock is recognised and re-pointed
at wherever the cache is listening now, keeping the <lock>.old already beside it, since
that copy rather than the current file is the original. A lock naming another machine's
cache is left alone unless -retarget says to move it here.

-warm-only fills the cache and writes nothing, which is what a CI job wants when the lock
in the repository has to stay as it is.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	chosen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lock" {
			chosen = true
		}
	})
	if *recursive && chosen {
		return errors.New(
			"warmlock: -lock names one file and -recursive walks a tree; use one or the other")
	}
	if !*recursive && !chosen {
		// With no -lock, use whichever lock this directory actually has. Defaulting to
		// uv.lock and reporting it missing would be unhelpful in a project that has a
		// package-lock.json sitting right there.
		*lockPath = lockFileHere(*lockPath)
	}
	snap, err := config.LoadLocal(collect())
	if err != nil {
		return err
	}
	opts := uvOptions{
		workers:  *workers,
		warmOnly: *warmOnly,
		suffix:   *suffix,
		eager:    *eager,
		target:   *target,
		retarget: *retarget,
	}
	if *recursive {
		root := "."
		if fs.NArg() > 0 {
			root = fs.Arg(0)
		}
		return warmTree(ctx, snap, root, opts)
	}
	return warmOne(ctx, snap, *lockPath, opts)
}

// warmOne warms one lock file, choosing the format from its bytes rather than its name,
// so a -lock pointed at a copy under any name still does the right thing.
func warmOne(
	ctx context.Context, snap *config.Snapshot, lockPath string, opts uvOptions,
) error {
	// #nosec G304 -- the path is the caller's, and reading it is the whole command.
	original, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	if kind := detectJSLock(original); kind != nil {
		return warmJSLock(ctx, snap, kind, string(original), lockPath,
			opts.workers, opts.warmOnly, opts.suffix)
	}
	return warmUVLock(ctx, snap, string(original), lockPath, opts)
}

// warmTree warms every lock below a directory, and does not stop at the first one that
// fails.
//
// A monorepo is what this is for — the tree this was written against has nineteen locks
// across uv and npm — and there, stopping on the third would leave sixteen unwarmed and
// the person re-running the command to work out which. So every lock is attempted, the
// failures are named together at the end, and the command still exits non-zero so a CI
// job notices. Cancellation is the one thing that does stop the walk: a Ctrl-C means
// stop, not "fail the remaining sixteen as fast as possible".
func warmTree(
	ctx context.Context, snap *config.Snapshot, root string, opts uvOptions,
) error {
	locks, err := findLocks(root)
	if err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	if len(locks) == 0 {
		return fmt.Errorf("warmlock: no lock files under %s", root)
	}
	fmt.Printf("%d lock file(s) under %s\n", len(locks), root)
	var failed []string
	for _, lockPath := range locks {
		fmt.Printf("\n== %s\n", lockPath)
		err := warmOne(ctx, snap, lockPath, opts)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		failed = append(failed, lockPath)
	}
	if len(failed) > 0 {
		return fmt.Errorf("warmlock: %d of %d lock file(s) failed:\n  %s",
			len(failed), len(locks), strings.Join(failed, "\n  "))
	}
	fmt.Printf("\n%d lock file(s) warmed\n", len(locks))
	return nil
}

// findLocks walks a tree for the lock files this command reads, in the lexical order
// WalkDir visits them.
//
// A directory it cannot read is skipped rather than fatal — one unreadable corner of a
// large tree is not a reason to warm none of it — but it is said out loud, because the
// difference between "that project has no lock" and "I could not look" is exactly the
// difference somebody would want to know about.
func findLocks(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "  skipping %s: %v\n", path, walkErr)
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			if lockNames[entry.Name()] {
				found = append(found, path)
			}
			return nil
		}
		if path != root && skipTree(entry.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
	return found, err
}

// skipTree reports whether a directory is one whose locks belong to somebody else.
//
// node_modules is the reason this exists: it is full of dependencies' own lock files,
// and warming those would fetch a tree nobody is installing. The rest are the same
// mistake in other ecosystems — an installed virtualenv, a vendored copy — plus dot
// directories, which hold caches and checkouts rather than projects.
func skipTree(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "venv", "site-packages", "__pycache__":
		return true
	}
	return false
}

// uvOptions is what the flags decided for a uv.lock, gathered so the steps below read
// as steps rather than as a nine-argument call.
type uvOptions struct {
	workers  int
	warmOnly bool
	suffix   string
	eager    bool
	target   string
	retarget bool
}

// warmUVLock warms and rewrites a uv.lock.
//
// Two phases, and the default is only the first. Warming every index page is cheap — one
// small document per package — and it is what the file route reads to turn a filename
// into an upstream URL, so it is the prerequisite for fetching anything at all. Having
// answered for all of them, the cache has proved it can serve this lock, which is what
// the rewrite is waiting on; the distributions themselves then arrive the first time
// something asks, through the same pull-through path warming would have used.
//
// -eager adds the second phase for the case the first does not cover: a machine that
// will not have a network later. That one wants bytes on disk, and it is the one that
// needs -target, because a uv.lock pins one version per package but lists every file the
// index holds for it — thirteen per package on the locks in this repository, against one
// an install uses.
func warmUVLock(
	ctx context.Context, snap *config.Snapshot,
	original, lockPath string, opts uvOptions,
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
	// Two addresses, and they are not the same one. The warm functions build the whole
	// path themselves — /<project>/pypi/<index>/+f/... — so their handler is given the
	// daemon's root. Rewrite is handed the project-scoped base, because what goes into
	// the lock file has to be the URL a client fetches. Conflating them makes every warm
	// a 404.
	root := state.BaseURL()
	base := root + "/" + project + "/pypi"

	upstreams := pypi.New().Descriptor().DefaultUpstreams
	indexes := lockwarm.NewIndexMap(upstreams)
	// A lock this command has already rewritten names a cache, not the internet, and
	// would otherwise look like a registry nothing serves. Registering the cache's own
	// URLs against the same indexes makes such a lock warmable again — which is the point
	// on a second machine, where the lock arrives from the repository already pointing
	// here and the cache is empty.
	for index := range upstreams {
		indexes.Adopt(base+"/"+index+"/+simple", index)
	}
	// The line above only recognises the address this daemon is listening on right now,
	// and that address moves. See adoptCacheBases.
	moved, err := adoptCacheBases(packages, indexes, upstreams, opts.retarget)
	if err != nil {
		return err
	}
	for _, old := range moved {
		fmt.Printf("%s points at %s, which is not serving this cache now — re-pointing it at %s\n",
			lockPath, old, base)
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

	selected := packages
	if opts.eager {
		target, err := resolveTarget(ctx, opts.target, lockPath)
		if err != nil {
			return fmt.Errorf("warmlock: %w", err)
		}
		selected = lockwarm.Select(packages, target)
		fmt.Printf("warming %d index page(s) and %d of %d file(s) for %s into %s\n",
			countIndexPages(packages), lockwarm.Count(selected),
			lockwarm.Count(packages), target, project)
	} else {
		fmt.Printf("warming %d index page(s) into %s; "+
			"the %d file(s) this lock pins are fetched when something asks for them\n",
			countIndexPages(packages), project, lockwarm.Count(packages))
	}

	var tally warmTally
	handler := daemonHandler(root)
	if err := lockwarm.WarmIndex(ctx, handler, project, packages, indexes,
		opts.workers, tally.record); err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	if opts.eager {
		if err := lockwarm.Warm(ctx, handler, project, selected, indexes,
			opts.workers, tally.record); err != nil {
			return fmt.Errorf("warmlock: %w", err)
		}
	}
	fmt.Printf("warmed %d, failed %d\n", tally.done.Load(), tally.failed.Load())
	if tally.failed.Load() > 0 {
		// The lock is not rewritten when the cache could not answer for all of it: a
		// file pointing at a cache that 404s is worse than one pointing at the internet,
		// because the failure arrives later and further from here.
		return fmt.Errorf(
			"warmlock: %d request(s) did not warm, so %s was left alone",
			tally.failed.Load(), lockPath)
	}
	if opts.warmOnly {
		return nil
	}

	// The whole package set, not the selected one: every URL in the file has to move, or
	// the lock ends up half cache and half internet, and the half nobody warmed is the
	// half that silently leaves the cache out.
	rewritten := lockwarm.Rewrite(original, packages, indexes, base)
	if rewritten == original {
		fmt.Printf("%s already points at this cache\n", lockPath)
		return nil
	}
	if err := writeRewrittenLock(lockPath, original, rewritten,
		opts.suffix, base, len(moved) > 0); err != nil {
		return err
	}
	if !opts.eager {
		fmt.Printf("the files themselves are not in the cache yet; " +
			"the first install pulls them through it\n")
	}
	return nil
}

// adoptCacheBases makes a lock that already points at some cache resolve against this
// one, and reports the addresses it had to move away from.
//
// This is the stale-port case, and it is not an edge case: a pkgcache daemon takes
// whatever port it is given when it starts, so the address in a lock rewritten yesterday
// is not the address serving today. Nothing distinguishes that lock from one naming a
// registry this cache was never configured for, so without this it fails with advice —
// add an upstream — that cannot possibly help.
//
// A routable address is left alone unless -retarget says otherwise. A loopback address
// could never have meant another machine, so repairing it is the only reading; a real
// hostname is most likely a shared pkgreg somebody chose deliberately, and quietly
// dragging that lock back to this laptop would be a worse failure than refusing to.
func adoptCacheBases(
	packages []lockwarm.Package, indexes lockwarm.IndexMap,
	served map[string]string, retarget bool,
) ([]string, error) {
	var moved, foreign []string
	seen := make(map[string]bool)
	for _, pkg := range packages {
		if _, ok := indexes.Index(pkg.Registry); ok {
			continue
		}
		old, index, isCache := lockwarm.CacheRegistry(pkg.Registry, served)
		if !isCache {
			// Either not one of ours, or naming an index this cache has no upstream
			// for. Both are the unknown-registry error, which the caller reports.
			continue
		}
		if !lockwarm.LocalCache(old) && !retarget {
			if !seen[old] {
				seen[old] = true
				foreign = append(foreign, old)
			}
			continue
		}
		indexes.Adopt(pkg.Registry, index)
		if !seen[old] {
			seen[old] = true
			moved = append(moved, old)
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf(
			"warmlock: this lock points at %s, which is another machine's cache\n"+
				"  Pass -retarget to point it at this one instead",
			strings.Join(foreign, ", "))
	}
	return moved, nil
}

// countIndexPages is how many requests WarmIndex will make: one per index and project,
// however many versions of a package a lock forked across markers happens to pin.
func countIndexPages(packages []lockwarm.Package) int {
	pages := make(map[string]bool, len(packages))
	for _, pkg := range packages {
		pages[pkg.Registry+" "+pkg.Project()] = true
	}
	return len(pages)
}

// resolveTarget reads -target, and works out the interpreter when the flag left that to
// this command.
//
// Only auto probes. A target somebody wrote out is taken literally, including its silence
// about Python: linux/x86_64 asks for that platform's wheels for every interpreter, and
// narrowing it to whichever python happens to be installed here would answer a different
// question than the one that was typed.
func resolveTarget(
	ctx context.Context, flagValue, lockPath string,
) (lockwarm.Target, error) {
	target, err := lockwarm.ParseTarget(flagValue)
	if err != nil {
		return lockwarm.Target{}, err
	}
	trimmed := strings.TrimSpace(flagValue)
	if trimmed != "" && !strings.EqualFold(trimmed, "auto") {
		return target, nil
	}
	if minor := projectPythonMinor(ctx, filepath.Dir(lockPath)); minor > 0 {
		target.Python = minor
	}
	return target, nil
}

// projectPythonMinor asks the interpreter uv would use for its 3.x minor version, and
// answers 0 when nothing answers.
//
// The project's own virtualenv comes first because that is what uv syncs into, and it is
// often a different 3.x from whatever python3 is on PATH. Finding neither is not an
// error: a target with no interpreter in it still drops every wheel built for another
// platform, which is the bulk of the fan-out.
func projectPythonMinor(ctx context.Context, dir string) int {
	for _, candidate := range []string{
		filepath.Join(dir, ".venv", "bin", "python"),
		filepath.Join(dir, ".venv", "Scripts", "python.exe"),
		"python3", "python",
	} {
		// #nosec G204 -- the candidates are this fixed list; finding the interpreter is
		// the entire point of the call.
		out, err := exec.CommandContext(ctx, candidate, "-c",
			"import sys;print('%d.%d' % sys.version_info[:2])").Output()
		if err != nil {
			continue
		}
		major, minor, found := strings.Cut(strings.TrimSpace(string(out)), ".")
		if !found || major != "3" {
			continue
		}
		if value, err := strconv.Atoi(minor); err == nil {
			return value
		}
	}
	return 0
}

// warmTally counts what came back and prints the failures as they arrive.
type warmTally struct{ done, failed atomic.Int64 }

// record is the yield callback the warm functions take.
func (t *warmTally) record(result lockwarm.Result) {
	switch {
	case result.Err != nil:
		t.failed.Add(1)
		fmt.Fprintf(os.Stderr, "  %s: %v\n", result.Filename, result.Err)
	case result.Status >= 400:
		t.failed.Add(1)
		fmt.Fprintf(os.Stderr, "  %s: HTTP %d\n", result.Filename, result.Status)
	default:
		t.done.Add(1)
	}
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
			strings.TrimRight(base, "/")+r.URL.RequestURI(), http.NoBody)
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

	var tally warmTally
	if err := lockwarm.WarmNPM(ctx, daemonHandler(root), project, packages, workers,
		tally.record); err != nil {
		return fmt.Errorf("warmlock: %w", err)
	}
	fmt.Printf("warmed %d of %d, failed %d\n",
		tally.done.Load(), requests, tally.failed.Load())
	if tally.failed.Load() > 0 {
		return fmt.Errorf(
			"warmlock: %d request(s) did not warm, so %s was left alone",
			tally.failed.Load(), lockPath)
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
	return writeRewrittenLock(lockPath, original, rewritten, suffix, base, false)
}

// writeRewrittenLock replaces the lock, keeping the original beside it.
//
// The copy is written first and never over something that is already there: it is the
// way back, and silently landing on an older one would take that away.
//
// retargeted is the one case where finding a copy already there is not a problem but the
// expected thing. The lock being replaced is itself a rewrite, so the copy beside it is
// the pristine one — and overwriting that with another cache-pointing file would destroy
// the only version that still names the internet, which is precisely what the copy is
// kept for. A stale port is repaired by a second run of this command, so refusing there
// would make the repair impossible without hand-moving a file first.
func writeRewrittenLock(
	lockPath, original, rewritten, suffix, base string, retargeted bool,
) error {
	backup := lockPath + suffix
	switch _, err := os.Stat(backup); {
	case err == nil && retargeted:
		fmt.Printf("keeping %s as it is: that copy is the original, and this lock was not\n",
			backup)
	case err == nil:
		return fmt.Errorf(
			"warmlock: %s already exists, so %s was left alone.\n"+
				"  Move it aside, or pass -backup-suffix to choose another name",
			backup, lockPath)
	case !errors.Is(err, os.ErrNotExist):
		return err
	default:
		if err := os.WriteFile(backup, []byte(original), 0o600); err != nil {
			return fmt.Errorf("warmlock: write %s: %w", backup, err)
		}
	}
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o600); err != nil {
		return fmt.Errorf("warmlock: write %s: %w", lockPath, err)
	}
	fmt.Printf("%s now points at %s\n", lockPath, base)
	fmt.Printf("the original is %s — `diff %s %s` shows only URLs\n",
		backup, backup, lockPath)
	return nil
}

// lockFileHere picks the lock to use when the caller named none.
//
// Falls back to the given default so a directory with no lock at all still produces the
// familiar "no such file" naming uv.lock, rather than naming whichever candidate happened
// to be checked last.
func lockFileHere(fallback string) string {
	for _, candidate := range lockOrder {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return fallback
}

// lockOrder is every lock file this command reads, in the order a directory holding more
// than one is searched: uv first, then npm's, then the two that need a setting changed
// rather than a rewrite.
var lockOrder = []string{
	"uv.lock", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
}

// lockNames is lockOrder as a set, for the recursive walk, which takes every lock it
// finds rather than the first.
var lockNames = func() map[string]bool {
	names := make(map[string]bool, len(lockOrder))
	for _, name := range lockOrder {
		names[name] = true
	}
	return names
}()
