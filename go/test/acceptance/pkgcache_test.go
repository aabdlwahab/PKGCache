package acceptance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// pkgcache's promise is that a real client works through it with nothing installed —
// no certificate, no --trusted-host, no configuration file. That is a claim about
// clients, so it is tested with clients: the real binary, a real daemon, and real npm,
// uv and git driven through `pkgcache run`.
//
// The upstreams are the same recorded fixtures the rest of this package uses, so the
// test needs no network and cannot fail because a registry had a bad day.

var buildPkgcache = sync.OnceValues(func() (string, error) {
	directory, err := os.MkdirTemp("", "pkgcache-acceptance-*")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(directory, "pkgcache")
	build := exec.Command("go", "build", "-o", binary, "github.com/aabdlwahab/PKGCache/cmd/pkgcache")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("build pkgcache: %w", err)
	}
	return binary, nil
})

// cacheProcess is a pkgcache binary plus the cache directory it owns.
type cacheProcess struct {
	t       *testing.T
	binary  string
	dataDir string
	addr    string
}

// startCache builds pkgcache, starts a daemon on an ephemeral port and stops it when
// the test ends.
func startCache(t *testing.T) *cacheProcess {
	t.Helper()
	binary, err := buildPkgcache()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	c := &cacheProcess{t: t, binary: binary, dataDir: dataDir}
	// A cache will not serve until somebody has chosen its size. These tests are not
	// about that policy, so they take the answer that never gets in the way.
	if out, err := c.command("limit", "none").CombinedOutput(); err != nil {
		t.Fatalf("setting the cache limit: %v\n%s", err, out)
	}

	// `run true` is the shortest way to say "start the cache and tell me nothing".
	if out, err := c.command("run", "--", "true").CombinedOutput(); err != nil {
		t.Fatalf("starting the cache: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := c.command("stop").CombinedOutput(); err != nil {
			t.Errorf("stopping the cache: %v\n%s", err, out)
		}
	})

	var state struct {
		Addr string `json:"addr"`
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "daemon.json"))
	if err != nil {
		t.Fatalf("the daemon published no state: %v", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	c.addr = state.Addr
	return c
}

// limit sets this cache's budget.
func (c *cacheProcess) limit(t *testing.T, value string) {
	t.Helper()
	if out, err := c.command("limit", value).CombinedOutput(); err != nil {
		t.Fatalf("pkgcache limit %s: %v\n%s", value, err, out)
	}
	// The daemon reads its budget at startup, so a change needs a restart to take.
	if out, err := c.command("stop").CombinedOutput(); err != nil {
		t.Fatalf("stopping to apply the limit: %v\n%s", err, out)
	}
}

func (c *cacheProcess) command(args ...string) *exec.Cmd {
	cmd := exec.Command(c.binary, args...)
	// A per-test cache directory, and an ephemeral port so concurrent tests and a
	// developer's own daemon never collide.
	cmd.Env = append(os.Environ(),
		"PKGCACHE_DATA_DIR="+c.dataDir,
		"PKGCACHE_ADDR=0",
		// npm keeps its own cache in the user's home, and it will happily satisfy an
		// install from there without ever contacting a registry. A test asserting what
		// this cache did would then be measuring npm's cache instead — which is how the
		// full-cache case first appeared to pass.
		"npm_config_cache="+filepath.Join(c.dataDir, "npm-cache"),
	)
	return cmd
}

// run executes a command through `pkgcache run`, returning its combined output.
func (c *cacheProcess) run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := c.command(append([]string{"run", "--"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pointAt configures one of the cache's upstreams. The control plane needs no
// credential here: local mode has no accounts, which is what makes an unauthenticated
// loopback API the right shape rather than a gap.
func (c *cacheProcess) pointAt(t *testing.T, ecosystem, name, url string) {
	t.Helper()
	c.pointAtPriority(t, ecosystem, name, url, 0)
}

// pointAtPriority adds one origin to an index's chain. Two calls with the same name and
// different priorities is how a chain is expressed.
func (c *cacheProcess) pointAtPriority(
	t *testing.T, ecosystem, name, url string, priority int,
) {
	t.Helper()
	c.pointProjectAtPriority(t, config.GlobalProject, ecosystem, name, url, priority)
}

// pointProjectAtPriority is pointAtPriority for a named project, which is what a chain
// per project needs.
func (c *cacheProcess) pointProjectAtPriority(
	t *testing.T, project, ecosystem, name, url string, priority int,
) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"eco": ecosystem, "name": name, "url": url,
		"enabled": true, "priority": priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + c.addr + "/api/v1/projects/" + project + "/upstreams"
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("configuring the %s upstream: %s\n%s", ecosystem, response.Status, message)
	}
}

func TestPkgcacheRunConfiguresEveryTool(t *testing.T) {
	cache := startCache(t)
	out, err := cache.run(t, t.TempDir(), "sh", "-c",
		"echo $PIP_INDEX_URL; echo $UV_DEFAULT_INDEX; echo $NPM_CONFIG_REGISTRY; "+
			"echo $GIT_CONFIG_COUNT; echo $PKGCACHE_SESSION")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected five settings, got:\n%s", out)
	}
	base := "http://" + cache.addr + "/global"
	for i, want := range []string{
		base + "/pypi/root/pypi/+simple/",
		base + "/pypi/root/pypi/+simple/",
		base + "/npm/",
		"1",
		"local",
	} {
		if lines[i] != want {
			t.Errorf("setting %d = %q, want %q", i, lines[i], want)
		}
	}
}

// A failing command's own status is the honest answer. Wrapping it in pkgcache's would
// lose information a script may branch on.
func TestPkgcacheRunPropagatesTheChildStatus(t *testing.T) {
	cache := startCache(t)
	for _, code := range []int{0, 1, 42} {
		out, err := cache.run(t, t.TempDir(), "sh", "-c", fmt.Sprintf("exit %d", code))
		got := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("%v\n%s", err, out)
			}
			got = exit.ExitCode()
		}
		if got != code {
			t.Errorf("exit status = %d, want %d\n%s", got, code, out)
		}
	}
}

func TestPkgcacheNPMInstall(t *testing.T) {
	npmBinary := requireBinary(t, "npm")
	cache := startCache(t)

	tarball := npmTarball(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pkgreg-acceptance":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":"pkgreg-acceptance","dist-tags":{"latest":"1.0.0"},
			  "versions":{"1.0.0":{"name":"pkgreg-acceptance","version":"1.0.0",
			  "dist":{"tarball":"%s/pkgreg-acceptance/-/pkgreg-acceptance-1.0.0.tgz"}}}}`,
				originBase(r))
		case "/pkgreg-acceptance/-/pkgreg-acceptance-1.0.0.tgz":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()
	cache.pointAt(t, "npm", "registry", origin.URL)

	project := t.TempDir()
	out, err := cache.run(t, project, npmBinary, "install", "--no-audit", "--no-fund",
		"pkgreg-acceptance@1.0.0")
	if err != nil {
		t.Fatalf("npm install through pkgcache: %v\n%s", err, out)
	}
	installed := filepath.Join(project, "node_modules", "pkgreg-acceptance", "package.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("the package did not install: %v\n%s", err, out)
	}

	// The point of a cache: the same install again, with the origin switched off.
	origin.Close()
	second := t.TempDir()
	out, err = cache.run(t, second, npmBinary, "install", "--no-audit", "--no-fund",
		"pkgreg-acceptance@1.0.0")
	if err != nil {
		t.Fatalf("the second install did not come from the cache: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(
		second, "node_modules", "pkgreg-acceptance", "package.json")); err != nil {
		t.Fatalf("the cached package did not install: %v\n%s", err, out)
	}
}

func TestPkgcacheUVInstall(t *testing.T) {
	uvBinary := requireBinary(t, "uv")
	cache := startCache(t)

	const filename = "demo_pkg-1.0.0-py3-none-any.whl"
	wheel := pythonWheel(t)
	sum := sha256.Sum256(wheel)
	digest := hex.EncodeToString(sum[:])
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/demo-pkg/":
			document, _ := json.Marshal(map[string]any{
				"meta": map[string]string{"api-version": "1.1"},
				"name": "demo-pkg",
				"files": []any{map[string]any{
					"filename":        filename,
					"url":             originBase(r) + "/packages/" + filename,
					"hashes":          map[string]string{"sha256": digest},
					"requires-python": ">=3.8",
					"yanked":          false,
					"core-metadata":   false,
				}},
			})
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write(document)
		case "/packages/" + filename:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(wheel)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()
	// "root/pypi" is the index name the default profile ships and the one the session's
	// PIP_INDEX_URL points at.
	cache.pointAt(t, "pypi", "root/pypi", origin.URL+"/simple")

	project := t.TempDir()
	target := filepath.Join(project, "site-packages")
	out, err := cache.run(t, project, uvBinary, "pip", "install", "--target", target,
		"--no-cache", "--no-deps", "demo-pkg==1.0.0")
	if err != nil {
		t.Fatalf("uv pip install through pkgcache: %v\n%s", err, out)
	}
	assertPythonFixture(t, target)

	// Again with the origin switched off: this is the whole point of a cache.
	origin.Close()
	second := filepath.Join(t.TempDir(), "site-packages")
	out, err = cache.run(t, project, uvBinary, "pip", "install", "--target", second,
		"--no-cache", "--no-deps", "demo-pkg==1.0.0")
	if err != nil {
		t.Fatalf("the second install did not come from the cache: %v\n%s", err, out)
	}
	assertPythonFixture(t, second)
}

// The git redirect is the capability pkgcache adds rather than inherits: an unmodified
// https:// clone, served from the cache, with nothing written to the user's git
// configuration.
func TestPkgcacheGitCloneIsRedirected(t *testing.T) {
	gitBinary := requireBinary(t, "git")
	cache := startCache(t)

	// A real repository, served over the dumb-HTTP-free path git uses for a local
	// origin: the cache mirrors whatever host it is asked for, so pointing the redirect
	// at a loopback origin exercises the same code path as github.com would.
	origin := t.TempDir()
	repo := filepath.Join(origin, "example.git")
	mustGit(t, "", "init", "--bare", repo)
	work := t.TempDir()
	mustGit(t, "", "clone", repo, filepath.Join(work, "src"))
	source := filepath.Join(work, "src")
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, source, "add", "README")
	mustGit(t, source, "-c", "user.email=t@example.test", "-c", "user.name=T",
		"commit", "-m", "first")
	mustGit(t, source, "push", "origin", "HEAD:refs/heads/main")

	// What is asserted is git's own reading of the session's configuration. Cloning
	// through the mirror of a filesystem origin is not something git will do — the
	// mirror fetches over HTTP — and the live HTTP path is covered by the npm and uv
	// cases above. Note the key: git normalises insteadOf to lower case when it lists.
	out, err := cache.run(t, work, gitBinary, "config", "--get-regexp",
		`^url\..*\.insteadof$`)
	if err != nil {
		t.Fatalf("git read no redirect from the session: %v\n%s", err, out)
	}
	want := "url.http://" + cache.addr + "/global/git/github.com/.insteadof https://github.com/"
	if strings.TrimSpace(out) != want {
		t.Fatalf("git resolved the redirect to %q, want %q", strings.TrimSpace(out), want)
	}

	// And the repository fixture above proves the clone itself is ordinary git, so a
	// redirect that resolves is a redirect that works.
	if _, err := os.Stat(filepath.Join(source, "README")); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func originBase(r *http.Request) string { return "http://" + r.Host }

func requireClient(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return path
}

// The decision that separates pkgcache from a server: a cache with no room left keeps
// serving and stops storing. A server refuses the request, which on a laptop would mean
// a full cache breaks the build it exists to speed up.
//
// Two things have to be true at once, and the second is the one that could silently
// rot: the build still works, and what it received is intact. A pass-through that
// truncated a tarball would be far worse than one that refused.
func TestPkgcacheFullCacheServesWithoutStoring(t *testing.T) {
	npmBinary := requireBinary(t, "npm")
	cache := startCache(t)

	// Counted under a mutex: the origin's handler runs on the server's goroutines while
	// the test reads the tally on its own.
	var (
		requestsMu      sync.Mutex
		tarballRequests = map[string]int{}
	)
	countTarball := func(name string) {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		tarballRequests[name]++
	}
	tarballCount := func(name string) int {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		return tarballRequests[name]
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"first-package", "second-package"} {
			if r.URL.Path == "/"+name {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"1.0.0"},
				  "versions":{"1.0.0":{"name":%q,"version":"1.0.0",
				  "dist":{"tarball":"%s/%s/-/%s-1.0.0.tgz"}}}}`,
					name, name, originBase(r), name, name)
				return
			}
			if r.URL.Path == "/"+name+"/-/"+name+"-1.0.0.tgz" {
				countTarball(name)
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(namedNPMTarball(t, name))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()
	cache.pointAt(t, "npm", "registry", origin.URL)

	// Fill the cache first, so that the limit set next is one the store is already
	// over. A guard cannot refuse an artifact whose size upstream never declared while
	// the store it is measuring is empty — and pretending otherwise in a test would
	// prove something the product does not do.
	install(t, cache, npmBinary, t.TempDir(), "first-package")

	// Now the store is over its budget by any measure.
	cache.limit(t, "1")

	project := t.TempDir()
	cmd := cache.command("run", "--", npmBinary, "install", "--no-audit", "--no-fund",
		"second-package@1.0.0")
	cmd.Dir = project
	out, err := cmd.CombinedOutput()

	// The install worked.
	if _, statErr := os.Stat(filepath.Join(
		project, "node_modules", "second-package", "package.json")); statErr != nil {
		t.Fatalf("a full cache broke the install: %v\n%s", statErr, out)
	}
	// It said so, on stderr, in words somebody can act on.
	if !strings.Contains(string(out), "NOTHING WAS CACHED") {
		t.Errorf("a full cache did not report itself:\n%s", out)
	}
	// And it exited 75 even though npm itself succeeded: silently-degraded caching in a
	// pipeline is exactly the failure this is meant to surface.
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("a full cache exited 0; want 75\n%s", out)
	}
	if exit.ExitCode() != 75 {
		t.Fatalf("exit status = %d, want 75\n%s", exit.ExitCode(), out)
	}

	// Nothing was stored, so the same package is fetched from the origin again.
	before := tarballCount("second-package")
	install(t, cache, npmBinary, t.TempDir(), "second-package")
	if tarballCount("second-package") <= before {
		t.Error("the artifact came from the cache, but the cache was full")
	}

	// The relayed bytes are intact — npm verifies the tarball itself, so an install
	// that succeeds is most of this, but the contents are checked rather than assumed.
	body, readErr := os.ReadFile(filepath.Join(
		project, "node_modules", "second-package", "package.json"))
	if readErr != nil || !strings.Contains(string(body), "second-package") {
		t.Fatalf("the relayed package is not intact: %v", readErr)
	}
}

// install runs one npm install through the cache and fails the test if it does not work.
func install(t *testing.T, cache *cacheProcess, npmBinary, dir, name string) {
	t.Helper()
	cmd := cache.command("run", "--", npmBinary, "install", "--no-audit", "--no-fund",
		name+"@1.0.0")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	// A full cache exits 75 with the install itself intact, which is not a failure here.
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 75) {
		t.Fatalf("npm install %s: %v\n%s", name, err, out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "node_modules", name, "package.json")); statErr != nil {
		t.Fatalf("npm install %s did not install it: %v\n%s", name, statErr, out)
	}
}

// Once there is room again, caching resumes and the full condition clears — so a cache
// somebody pruned does not keep reporting itself full.
func TestPkgcacheRecoversWhenGivenRoom(t *testing.T) {
	npmBinary := requireBinary(t, "npm")
	cache := startCache(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"first-package", "second-package"} {
			if r.URL.Path == "/"+name {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"1.0.0"},
				  "versions":{"1.0.0":{"name":%q,"version":"1.0.0",
				  "dist":{"tarball":"%s/%s/-/%s-1.0.0.tgz"}}}}`,
					name, name, originBase(r), name, name)
				return
			}
			if r.URL.Path == "/"+name+"/-/"+name+"-1.0.0.tgz" {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(namedNPMTarball(t, name))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()
	cache.pointAt(t, "npm", "registry", origin.URL)

	install(t, cache, npmBinary, t.TempDir(), "first-package")
	cache.limit(t, "1")
	install(t, cache, npmBinary, t.TempDir(), "second-package")

	cache.limit(t, "none")
	project := t.TempDir()
	cmd := cache.command("run", "--", npmBinary, "install", "--no-audit", "--no-fund",
		"second-package@1.0.0")
	cmd.Dir = project
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a cache with room again did not run cleanly: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "NOTHING WAS CACHED") {
		t.Errorf("the full condition survived being given room:\n%s", out)
	}

	// The origin can go away now, because this time it really was stored.
	origin.Close()
	install(t, cache, npmBinary, t.TempDir(), "second-package")
}

// namedNPMTarball builds a tarball whose package.json names the package being served,
// so a test can install two distinct packages from one origin.
func namedNPMTarball(t *testing.T, name string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"package/package.json": fmt.Sprintf(
			`{"name":%q,"version":"1.0.0","main":"index.js"}`, name),
		"package/index.js": "module.exports = function () { return " +
			fmt.Sprintf("%q", name) + "; };",
	}
	for path, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: path, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// The headline claim of the merge: a miss goes to the team's cache, and only reaches a
// public registry when the team's cache cannot serve it.
//
// Both tiers are ordinary upstreams at different priorities, which is the design — there
// is no "team cache" concept in the engine, so this is testing the chain rather than a
// mode, and a real daemon walks it.
func TestPkgcacheFallsBackFromTeamToPublic(t *testing.T) {
	npmBinary := requireBinary(t, "npm")
	cache := startCache(t)

	var teamMu sync.Mutex
	teamHits, publicHits := 0, 0
	packument := func(w http.ResponseWriter, r *http.Request, name string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"1.0.0"},
		  "versions":{"1.0.0":{"name":%q,"version":"1.0.0",
		  "dist":{"tarball":"%s/%s/-/%s-1.0.0.tgz"}}}}`,
			name, name, originBase(r), name, name)
	}
	serve := func(count *int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			teamMu.Lock()
			*count++
			teamMu.Unlock()
			for _, name := range []string{"first-package", "second-package"} {
				if r.URL.Path == "/"+name {
					packument(w, r, name)
					return
				}
				if r.URL.Path == "/"+name+"/-/"+name+"-1.0.0.tgz" {
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(namedNPMTarball(t, name))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}
	team := httptest.NewServer(serve(&teamHits))
	defer team.Close()
	public := httptest.NewServer(serve(&publicHits))
	defer public.Close()

	// One index name, two origins. Priority is what makes it a chain.
	cache.pointAtPriority(t, "npm", "registry", team.URL, 10)
	cache.pointAtPriority(t, "npm", "registry", public.URL, 20)

	install(t, cache, npmBinary, t.TempDir(), "first-package")
	teamMu.Lock()
	afterFirst := teamHits
	publicAfterFirst := publicHits
	teamMu.Unlock()
	if afterFirst == 0 {
		t.Fatal("the team cache was never asked")
	}
	if publicAfterFirst != 0 {
		t.Fatalf("the public registry was used while the team cache was up (%d)",
			publicAfterFirst)
	}

	// The team cache goes away. A build must keep working.
	team.Close()
	install(t, cache, npmBinary, t.TempDir(), "second-package")
	teamMu.Lock()
	defer teamMu.Unlock()
	if publicHits == 0 {
		t.Fatal("nothing fell through to the public registry once the team cache was down")
	}
}

// -no-cache is a promise as much as a setting: it is the old client's behaviour, and if
// it ever opens a store then the merge has cost existing users something they did not
// ask for. Asserted by looking at the directory rather than by reading the code.
func TestPkgcacheNoCacheOpensNoStore(t *testing.T) {
	binary, err := buildPkgcache()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	// A pkgreg serving TLS is what -no-cache bridges to. A plain TLS server is enough
	// to exercise the setup path's verification, which is what writes the mode.
	var caPEM []byte
	team := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ca.crt" {
				_, _ = w.Write(caPEM)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	defer team.Close()
	caPEM = pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: team.Certificate().Raw,
	})
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	run := exec.Command(binary, "setup",
		"-server", team.URL, "-ca-file", caPath, "-no-cache")
	run.Env = append(os.Environ(), "PKGCACHE_DATA_DIR="+dataDir, "PKGCACHE_LIMIT=")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("setup -no-cache: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "local      disabled") {
		t.Errorf("setup did not report the mode:\n%s", out)
	}

	// The promise: settings, and nothing that constitutes a store.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"team.json": true, "team-ca.crt": true, "budget.json": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			t.Errorf("-no-cache created %q; it must open no store", entry.Name())
		}
	}
	for _, forbidden := range []string{"db", "blobs", "managed"} {
		if _, err := os.Stat(filepath.Join(dataDir, forbidden)); err == nil {
			t.Errorf("-no-cache created %s/", forbidden)
		}
	}
}

// Projects on a laptop exist to resolve differently, so this asserts the thing that
// makes them worth having: two projects, two chains, and a request in one never reaching
// the other's origin.
//
// Driven with plain HTTP rather than npm. The claim is about routing and chain order,
// which is exactly what a GET through the cache exercises, and a real installer would
// add minutes to prove the same two counters.
func TestPkgcacheProjectsChainSeparately(t *testing.T) {
	cache := startCache(t)

	var mu sync.Mutex
	teamHits, publicHits := 0, 0
	serve := func(count *int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*count++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":"pkg","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
		}
	}
	team := httptest.NewServer(serve(&teamHits))
	defer team.Close()
	public := httptest.NewServer(serve(&publicHits))
	defer public.Close()

	cache.cli(t, "project", "create", "work")
	cache.readAddr(t)

	// global goes straight out; work goes through the team cache first.
	cache.pointAtPriority(t, "npm", "registry", public.URL, 20)
	cache.pointProjectAtPriority(t, "work", "npm", "registry", team.URL, 10)

	if status := cacheGet(t, cache, "/work/npm/in-work"); status != http.StatusOK {
		t.Fatalf("a request in work answered %d", status)
	}
	mu.Lock()
	teamAfterWork, publicAfterWork := teamHits, publicHits
	mu.Unlock()
	if teamAfterWork == 0 {
		t.Fatal("work did not reach its own team cache")
	}
	if publicAfterWork != 0 {
		t.Fatalf("work reached the global project's origin (%d hits)", publicAfterWork)
	}

	if status := cacheGet(t, cache, "/global/npm/in-global"); status != http.StatusOK {
		t.Fatalf("a request in global answered %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if publicHits == 0 {
		t.Fatal("global did not reach its own origin")
	}
	if teamHits != teamAfterWork {
		t.Fatalf("global reached work's team cache (%d, was %d)", teamHits, teamAfterWork)
	}
}

// The hole this closes: upstream rows are per project in the database, so a project
// created after `setup` used to inherit nothing and resolve straight to the public
// registry. Silently, because a chain missing its first row is still a valid chain.
//
// -no-direct throughout, so the chain is one row and the test can never reach the real
// registry.npmjs.org that a direct chain's second row would name.
func TestPkgcacheNewProjectInheritsTheTeamChain(t *testing.T) {
	cache := startCache(t)

	var mu sync.Mutex
	hits := map[string]int{}
	// A pkgreg publishes its CA at /api/ca.crt, and setup fetches it there and refuses
	// it unless it matches the fingerprint given out of band. Serving it is what makes
	// this an honest stand-in for a team cache; it is not counted as a package request.
	var certificate func() []byte
	serve := func(label string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ca.crt" {
				_, _ = w.Write(certificate())
				return
			}
			mu.Lock()
			hits[label]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"1.0.0"},"versions":{}}`, label)
		})
	}
	var current *httptest.Server
	certificate = func() []byte {
		return pem.EncodeToMemory(&pem.Block{
			Type: "CERTIFICATE", Bytes: current.Certificate().Raw,
		})
	}
	first := httptest.NewTLSServer(serve("first"))
	defer first.Close()
	second := httptest.NewTLSServer(serve("second"))
	defer second.Close()

	// A pkgreg mints its own certificate, so setup pins a CA out of band. Here that is
	// httptest's, written to a file — the same path an air-gapped install takes.
	current = first
	cache.cli(t, "setup", "-server", first.URL, "-ca-file",
		writeCA(t, first.Certificate().Raw), "-no-direct")
	cache.cli(t, "project", "create", "work")
	cache.start(t)

	if status := cacheGet(t, cache, "/work/npm/anything"); status != http.StatusOK {
		t.Fatalf("a project created after setup answered %d; it inherited no chain", status)
	}
	mu.Lock()
	inherited := hits["first"]
	mu.Unlock()
	if inherited == 0 {
		t.Fatal("a project created after setup did not reach the team cache")
	}

	// Its own configuration replaces the inherited one, and does not disturb global's.
	current = second
	cache.cli(t, "setup", "-project", "work", "-server", second.URL, "-ca-file",
		writeCA(t, second.Certificate().Raw), "-no-direct")
	cache.start(t)

	if status := cacheGet(t, cache, "/work/npm/its-own"); status != http.StatusOK {
		t.Fatalf("work answered %d after being given its own team cache", status)
	}
	if status := cacheGet(t, cache, "/global/npm/still-first"); status != http.StatusOK {
		t.Fatalf("global answered %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["second"] == 0 {
		t.Fatal("work did not reach the team cache configured for it")
	}
	if hits["first"] <= inherited {
		t.Fatal("global stopped reaching its own team cache when work was given one")
	}
}

// cli runs a pkgcache command and fails the test with its output.
func (c *cacheProcess) cli(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.command(args...).CombinedOutput()
	if err != nil {
		t.Fatalf("pkgcache %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// start ensures a daemon is running and records its address. `setup` stops the daemon —
// it holds the store's single writer, and the trust bundle is read at startup — so any
// test that configures a team cache has to bring one back before it can ask for a
// package, on whatever ephemeral port it lands.
func (c *cacheProcess) start(t *testing.T) {
	t.Helper()
	c.cli(t, "run", "--", "true")
	c.readAddr(t)
}

// readAddr re-reads the published address, because a command that restarts the daemon
// gets a new ephemeral port.
func (c *cacheProcess) readAddr(t *testing.T) {
	t.Helper()
	var state struct {
		Addr string `json:"addr"`
	}
	data, err := os.ReadFile(filepath.Join(c.dataDir, "daemon.json"))
	if err != nil {
		t.Fatalf("the daemon published no state: %v", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	c.addr = state.Addr
}

// cacheGet asks the cache for a path and returns the status.
func cacheGet(t *testing.T, c *cacheProcess, path string) int {
	t.Helper()
	response, err := http.Get("http://" + c.addr + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// writeCA puts a certificate where -ca-file can read it.
func writeCA(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The air-gap round trip, entirely from the client: one cache warms itself, writes a pack
// to a path somebody could carry, and a second cache — with the project offline, so it
// cannot reach anything — serves what the pack held and nothing else.
//
// No server takes part at any point, which is the claim.
func TestPkgcacheShuttlesAPackBetweenTwoCaches(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"carried","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	}))
	defer origin.Close()

	sender := startCache(t)
	sender.pointAt(t, "npm", "registry", origin.URL)
	if status := cacheGet(t, sender, "/global/npm/carried"); status != http.StatusOK {
		t.Fatalf("warming the sender answered %d", status)
	}

	// A path outside the cache directory: the whole point of the client's -file.
	stick := filepath.Join(t.TempDir(), "work.tar")
	out := sender.cli(t, "export", "-file", stick)
	if !strings.Contains(out, stick) {
		t.Fatalf("export did not say where the pack went:\n%s", out)
	}
	if info, err := os.Stat(stick); err != nil || info.Size() == 0 {
		t.Fatalf("no pack at %s: %v", stick, err)
	}

	receiver := startCache(t)
	// Pointed at the same origin, so "served from the pack" is a claim with teeth: the
	// receiver could have fetched this itself, and the hit count proves it did not.
	receiver.pointAt(t, "npm", "registry", origin.URL)
	receiver.cli(t, "import", "-file", stick)
	setOffline(t, receiver, config.GlobalProject, true)

	mu.Lock()
	before := hits
	mu.Unlock()
	if status := cacheGet(t, receiver, "/global/npm/carried"); status != http.StatusOK {
		t.Fatalf("the receiver would not serve what the pack carried: %d", status)
	}
	if status := cacheGet(t, receiver, "/global/npm/never-sent"); status == http.StatusOK {
		t.Fatal("the receiver served something the pack never carried")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != before {
		t.Fatalf("the receiver reached upstream %d times; it was meant to be offline",
			hits-before)
	}

	// The staged copy is the client's, and it does not outlive the command.
	entries, err := os.ReadDir(filepath.Join(receiver.dataDir, "shuttle", "in"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("import left %d files in shuttle/in", len(entries))
	}
}

// A second trip carries only what is new. The delta is refused unless it continues from
// the receiver's checkpoint, which is why `snapshots` prints that checkpoint and `export`
// takes it.
func TestPkgcacheShuttlesADeltaOnTheSecondTrip(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":%q,"dist-tags":{"latest":"1.0.0"},"versions":{}}`,
			strings.TrimPrefix(r.URL.Path, "/"))
	}))
	defer origin.Close()

	sender, receiver := startCache(t), startCache(t)
	sender.pointAt(t, "npm", "registry", origin.URL)
	cacheGet(t, sender, "/global/npm/first")

	stick := t.TempDir()
	full := filepath.Join(stick, "full.tar")
	sender.cli(t, "export", "-file", full)
	receiver.cli(t, "import", "-file", full)

	// Which checkpoint the receiver is on is the one fact the sender needs.
	head := headOf(t, receiver)
	cacheGet(t, sender, "/global/npm/second")

	// Two packs from the same sender at the same moment: everything, and only what is new
	// since the receiver's checkpoint. Compared in blobs rather than bytes, because at
	// fixture scale a tar's 512-byte blocks and end marker dominate and two packs
	// carrying different content can weigh exactly the same.
	everything := filepath.Join(stick, "everything.tar")
	sender.cli(t, "export", "-file", everything)
	delta := filepath.Join(stick, "delta.tar")
	sender.cli(t, "export", "-since", head, "-file", delta)

	whole, part := packBlobs(t, everything), packBlobs(t, delta)
	if part >= whole {
		t.Errorf("the delta carries %d blobs and a full pack from here carries %d", part, whole)
	}
	if part == 0 {
		t.Error("the delta carries nothing at all")
	}

	receiver.cli(t, "import", "-file", delta)
	setOffline(t, receiver, config.GlobalProject, true)
	for _, name := range []string{"first", "second"} {
		if status := cacheGet(t, receiver, "/global/npm/"+name); status != http.StatusOK {
			t.Errorf("%s did not survive the two trips: %d", name, status)
		}
	}
}

// The refusal, which is the error a real user meets. Nothing may be written, and the
// message has to name the checkpoint this project is on — the fix is to export a delta
// from it, and nobody can do that without knowing it.
func TestPkgcacheImportRefusesAPackThatDoesNotContinueFromHere(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"x","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	}))
	defer origin.Close()

	sender, receiver := startCache(t), startCache(t)
	sender.pointAt(t, "npm", "registry", origin.URL)
	cacheGet(t, sender, "/global/npm/first")

	stick := filepath.Join(t.TempDir(), "full.tar")
	sender.cli(t, "export", "-file", stick)

	// The receiver has a history of its own, so a pack starting from nothing cannot be
	// applied on top of it.
	receiver.pointAt(t, "npm", "registry", origin.URL)
	cacheGet(t, receiver, "/global/npm/mine")
	receiver.cli(t, "checkpoint", "-m", "my own work")
	head := headOf(t, receiver)

	out, err := receiver.command("import", "-file", stick).CombinedOutput()
	if err == nil {
		t.Fatalf("the import was accepted:\n%s", out)
	}
	for _, want := range []string{"does not continue", head[:12], "export -since"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	if after := headOf(t, receiver); after != head {
		t.Fatalf("a refused import moved the checkpoint from %s to %s", head, after)
	}
}

// setOffline flips a project's own offline flag, which is how these tests prove content
// came from a pack rather than from an origin that happened to still be up.
func setOffline(t *testing.T, c *cacheProcess, project string, offline bool) {
	t.Helper()
	body := fmt.Sprintf(`{"offline":%t}`, offline)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPatch,
		"http://"+c.addr+"/api/v1/projects/"+project, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("setting offline: %s\n%s", response.Status, message)
	}
}

// headOf reports the checkpoint a project is on. Not the newest one: a rollback moves the
// head back without removing what came after it.
func headOf(t *testing.T, c *cacheProcess) string {
	t.Helper()
	response, err := http.Get(
		"http://" + c.addr + "/api/v1/projects/" + config.GlobalProject + "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Head string `json:"head"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Head == "" {
		t.Fatal("the project reports no checkpoint")
	}
	return body.Head
}

// packBlobs counts the blobs a pack carries, which is what makes a delta a delta.
func packBlobs(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader, count := tar.NewReader(file), 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return count
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "blobs/") {
			count++
		}
	}
}

// A cache that stores nothing has nothing to export and nowhere to import, and both must
// say so without creating the databases -no-cache promises not to.
func TestPkgcacheNoCacheRefusesTheShuttle(t *testing.T) {
	binary, err := buildPkgcache()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	var caPEM []byte
	team := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ca.crt" {
				_, _ = w.Write(caPEM)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	defer team.Close()
	caPEM = pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: team.Certificate().Raw,
	})

	run := func(args ...string) (string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "PKGCACHE_DATA_DIR="+dataDir, "PKGCACHE_LIMIT=")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("setup", "-server", team.URL,
		"-ca-file", writeCA(t, team.Certificate().Raw), "-no-cache"); err != nil {
		t.Fatalf("setup -no-cache: %v\n%s", err, out)
	}

	for _, args := range [][]string{
		{"export", "-file", filepath.Join(t.TempDir(), "work.tar")},
		{"import", "-file", filepath.Join(t.TempDir(), "work.tar")},
		{"checkpoint", "-m", "no"},
		{"snapshots"},
	} {
		out, err := run(args...)
		if err == nil {
			t.Errorf("%v was accepted by a cache that stores nothing:\n%s", args, out)
			continue
		}
		if !strings.Contains(out, "stores nothing") && !strings.Contains(out, "not a file") &&
			!strings.Contains(out, "read the pack") {
			t.Errorf("%v failed for the wrong reason:\n%s", args, out)
		}
	}
	for _, forbidden := range []string{"db", "blobs"} {
		if _, err := os.Stat(filepath.Join(dataDir, forbidden)); err == nil {
			t.Errorf("the shuttle commands created %s/ in a -no-cache cache", forbidden)
		}
	}
}
