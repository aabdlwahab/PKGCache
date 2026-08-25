package lockwarm

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewrittenYarnClassicLockAcceptedByYarn asks of yarn what the npm test asks of npm:
// is the URL in the lock the one it actually fetches?
//
// Answered the same way, by shutting the upstream registry down before installing, so a
// pass means every byte came from the rewritten URLs. Yarn adds a wrinkle npm does not:
// it appends "#<sha1>" to each URL and checks it, so this also proves the fragment
// survived the rewrite — a rewrite that dropped it would fail here rather than later on
// somebody else's machine.
func TestRewrittenYarnClassicLockAcceptedByYarn(t *testing.T) {
	yarnBin, err := exec.LookPath("yarn")
	if err != nil {
		t.Skip("yarn is not installed")
	}
	if out, err := exec.Command(yarnBin, "--version").Output(); err != nil ||
		!strings.HasPrefix(strings.TrimSpace(string(out)), "1.") {
		t.Skip("yarn on PATH is not the v1 line")
	}
	tarball := testTarball(t)
	sha512sum := sha512.Sum512(tarball)
	sha1sum := sha1.Sum(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sha512sum[:])
	shasum := hex.EncodeToString(sha1sum[:])
	const pkgName = "demo-pkg"
	const filename = pkgName + "-1.0.0.tgz"

	packument := func(tarballURL string) []byte {
		body, marshalErr := json.Marshal(map[string]any{
			"name":      pkgName,
			"dist-tags": map[string]string{"latest": "1.0.0"},
			"versions": map[string]any{
				"1.0.0": map[string]any{
					"name":    pkgName,
					"version": "1.0.0",
					"dist": map[string]any{
						"tarball":   tarballURL,
						"integrity": integrity,
						// Yarn v1 uses this for the fragment it appends and verifies.
						"shasum": shasum,
					},
				},
			},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body
	}

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + pkgName:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(packument(upstream.URL + "/" + pkgName + "/-/" + filename))
		case "/" + pkgName + "/-/" + filename:
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	upstreamURL := upstream.URL

	var cache *httptest.Server
	cache = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/npm/" + pkgName:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(packument(cache.URL + "/global/npm/" + pkgName + "/-/" + filename))
		case "/global/npm/" + pkgName + "/-/" + filename:
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cache.Close()

	project := t.TempDir()
	home := t.TempDir()
	manifest := `{"name":"acceptance","version":"1.0.0","license":"MIT","dependencies":{"` +
		pkgName + `":"1.0.0"}}`
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	runYarn(t, yarnBin, project, home, "install", "--registry", upstreamURL)

	lockPath := filepath.Join(project, "yarn.lock")
	locked, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := ParseYarnClassic(string(locked))
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0].Filename != filename {
		t.Fatalf("parsed %+v", packages)
	}
	if packages[0].Fragment == "" {
		t.Fatal("yarn wrote no checksum fragment, so this test proves less than it should")
	}
	rewritten := RewriteYarnClassic(string(locked), packages, cache.URL+"/global/npm")
	if !strings.Contains(rewritten, cache.URL+"/global/npm/"+pkgName+"/-/"+filename+packages[0].Fragment) {
		t.Fatalf("rewrite did not keep the fragment:\n%s", rewritten)
	}
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	// Gone. Anything still reaching for it now fails.
	upstream.Close()
	if err := os.RemoveAll(filepath.Join(project, "node_modules")); err != nil {
		t.Fatal(err)
	}

	runYarn(t, yarnBin, project, home, "install", "--frozen-lockfile",
		"--registry", "http://127.0.0.1:9")

	installed := filepath.Join(project, "node_modules", pkgName, "package.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("yarn reported success but installed nothing: %v", err)
	}
}

// runYarn runs yarn with its cache and state inside the test's own directories, so a
// developer's real yarn cache can neither hide a missing fetch nor be written to.
func runYarn(t *testing.T, yarnBin, directory, home string, args ...string) {
	t.Helper()
	command := exec.Command(yarnBin, append(args, "--no-progress", "--non-interactive")...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"HOME="+home,
		"YARN_CACHE_FOLDER="+filepath.Join(home, "yarn-cache"),
		"npm_config_cache="+filepath.Join(home, "npm-cache"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("yarn %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
