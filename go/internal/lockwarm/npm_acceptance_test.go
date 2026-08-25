package lockwarm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewrittenNPMLockAcceptedByNPMCI is the question the rest of the npm support rests
// on: does npm actually fetch the URL in "resolved", or does it rebuild one from the
// configured registry and ignore what the lock says?
//
// It is answered the only way worth trusting — the upstream registry is shut down before
// `npm ci` runs, so an install that reaches for it fails. A pass means every byte came
// from the rewritten URLs, which is exactly the machine-with-no-internet case the rewrite
// exists to serve.
func TestRewrittenNPMLockAcceptedByNPMCI(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed")
	}
	tarball := testTarball(t)
	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
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

	// The cache serves the shape the npm adapter really serves: <base>/<name>/-/<file>,
	// where base carries the project and the ecosystem and no index segment.
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
	manifest := `{"name":"acceptance","version":"1.0.0","dependencies":{"` + pkgName + `":"1.0.0"}}`
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// A lock produced against the upstream, which is what a developer would commit.
	runNPM(t, npmBin, project, home, "install", "--package-lock-only",
		"--registry", upstreamURL, "--no-audit", "--no-fund")

	lockPath := filepath.Join(project, "package-lock.json")
	locked, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := ParseNPM(string(locked))
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0].Filename != filename {
		t.Fatalf("parsed %+v", packages)
	}
	if packages[0].Registry != upstreamURL {
		t.Fatalf("registry = %q, want %q", packages[0].Registry, upstreamURL)
	}
	rewritten := RewriteNPM(string(locked), packages, cache.URL+"/global/npm")
	if !strings.Contains(rewritten, cache.URL+"/global/npm/"+pkgName+"/-/"+filename) {
		t.Fatalf("rewrite did not point at the cache:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, integrity) {
		t.Fatal("the rewrite dropped the integrity hash")
	}
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	// The upstream is gone. Anything that still reaches for it now fails, so a
	// successful install is proof the rewritten URLs were the ones used.
	upstream.Close()

	runNPM(t, npmBin, project, home, "ci", "--registry", cache.URL+"/global/npm",
		"--no-audit", "--no-fund")

	installed := filepath.Join(project, "node_modules", pkgName, "package.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("npm ci reported success but installed nothing: %v", err)
	}
}

// testTarball builds the smallest thing npm will accept as a package: a gzipped tar
// whose entries live under package/.
func testTarball(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	zip := gzip.NewWriter(&out)
	archive := tar.NewWriter(zip)
	entries := []struct{ name, body string }{
		{"package/package.json", `{"name":"demo-pkg","version":"1.0.0","main":"index.js"}`},
		{"package/index.js", "module.exports = 1;\n"},
	}
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Mode: 0o644,
			Size: int64(len(entry.body)), Typeflag: tar.TypeReg, ModTime: testEpoch,
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(archive, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// runNPM runs npm with its state confined to the test's own directories, so a developer's
// real npm cache can neither hide a missing fetch nor be written to by the test.
func runNPM(t *testing.T, npmBin, directory, home string, args ...string) {
	t.Helper()
	command := exec.Command(npmBin, args...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"HOME="+home,
		"npm_config_cache="+filepath.Join(home, "npm-cache"),
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	fmt.Fprintf(os.Stderr, "npm %s ok\n", strings.Join(args, " "))
}
