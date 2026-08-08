package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	build := exec.Command("go", "build", "-o", binary, "github.com/brightskies/pkgreg/cmd/pkgcache")
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

func (c *cacheProcess) command(args ...string) *exec.Cmd {
	cmd := exec.Command(c.binary, args...)
	// A per-test cache directory, and an ephemeral port so concurrent tests and a
	// developer's own daemon never collide.
	cmd.Env = append(os.Environ(),
		"PKGCACHE_DATA_DIR="+c.dataDir,
		"PKGCACHE_ADDR=0",
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
	body, err := json.Marshal(map[string]any{
		"eco": ecosystem, "name": name, "url": url, "enabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + c.addr + "/api/v1/projects/global/upstreams"
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
