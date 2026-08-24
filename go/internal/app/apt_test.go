package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/feed"
)

// aptApp is an instance with a published repository, reached the way apt would reach it.
func aptApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Upstream.Offline = true
	})
	server := httptest.NewServer(a.UnifiedHandler())
	t.Cleanup(server.Close)
	return a, server
}

// testDeb builds a minimal but real .deb: an ar archive whose middle member is a
// gzipped tar holding ./control. The same shape packaging/deb/build.sh writes.
func testDeb(t *testing.T) []byte {
	t.Helper()
	control := "Package: pkgcache\nVersion: 1.2.0\nArchitecture: amd64\n" +
		"Maintainer: pkgreg <root@localhost>\nDescription: test package\n"

	var tarball bytes.Buffer
	zip := gzip.NewWriter(&tarball)
	archive := tar.NewWriter(zip)
	body := []byte(control)
	if err := archive.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}

	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	member := func(name string, content []byte) {
		fmt.Fprintf(&deb, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, 0o644, len(content))
		deb.Write(content)
		if len(content)%2 == 1 {
			deb.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tarball.Bytes())
	// Padded past 16 bytes so the Range test has something to slice.
	member("data.tar.gz", []byte(strings.Repeat("payload ", 64)))
	return deb.Bytes()
}

func publishInto(t *testing.T, dataDir string) {
	t.Helper()
	source := t.TempDir()
	deb := filepath.Join(source, "pkgcache_1.2.0_amd64.deb")
	if err := os.WriteFile(deb, testDeb(t), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := feed.GenerateKey(feed.KeyOptions{
		Name: "test", Email: "t@example.invalid", Algorithm: feed.KeyEd25519,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := feed.WriteRepository(feed.RepoOptions{
		Root: feed.RepoDir(dataDir), Debs: []string{deb}, Key: key,
	}); err != nil {
		t.Fatal(err)
	}
}

func aptGet(t *testing.T, server *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	response, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response, string(body)
}

func TestAptRepositoryIsServedWithoutCredentials(t *testing.T) {
	// apt cannot log in. A repository behind a session check is one nothing can install
	// from, and the signature — not the transport — is what makes it trustworthy.
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	for _, path := range []string{
		"/apt/dists/stable/InRelease",
		"/apt/dists/stable/Release",
		"/apt/dists/stable/Release.gpg",
		"/apt/dists/stable/main/binary-amd64/Packages",
		"/apt/dists/stable/main/binary-amd64/Packages.gz",
		"/apt/pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb",
		"/apt/pkgcache-archive-keyring.asc",
	} {
		response, body := aptGet(t, server, path)
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200\n%s", path, response.StatusCode, body)
		}
	}
}

func TestAptIndexIsReadableAndPackagesAreNot(t *testing.T) {
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	// An operator debugging a failed update opens these in a browser.
	response, body := aptGet(t, server, "/apt/dists/stable/main/binary-amd64/Packages")
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("Packages should be readable text, got %q",
			response.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "Package: pkgcache") {
		t.Errorf("the served index is not the index:\n%s", body)
	}

	response, _ = aptGet(t, server, "/apt/pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb")
	if got := response.Header.Get("Content-Type"); got != "application/vnd.debian.binary-package" {
		t.Errorf("a .deb should not be served as text: %q", got)
	}
}

func TestAptPoolIsCachedForeverAndIndexesAreNot(t *testing.T) {
	// A pool file never changes under a client; an index changes on every publish. That
	// difference is most of what keeps `apt update` cheap.
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	response, _ := aptGet(t, server, "/apt/pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb")
	if !strings.Contains(response.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("pool files should be immutable, got %q",
			response.Header.Get("Cache-Control"))
	}
	response, _ = aptGet(t, server, "/apt/dists/stable/InRelease")
	if strings.Contains(response.Header.Get("Cache-Control"), "immutable") {
		t.Error("InRelease must never be cached as immutable; it changes on every publish")
	}
}

func TestAptRefusesToEscapeItsRoot(t *testing.T) {
	a, server := aptApp(t)
	dataDir := a.Config.Current().DataDir
	publishInto(t, dataDir)

	// The signing key is the thing worth stealing, and it is deliberately a sibling of
	// the served tree rather than inside it. These are the requests that would reach it
	// if the handler joined paths and hoped.
	for _, attempt := range []string{
		"/apt/../signing-key.asc",
		"/apt/..%2Fsigning-key.asc",
		"/apt/dists/../../signing-key.asc",
		"/apt/%2e%2e/signing-key.asc",
		"/apt/....//signing-key.asc",
	} {
		response, body := aptGet(t, server, attempt)
		if response.StatusCode == http.StatusOK && strings.Contains(body, "PRIVATE KEY") {
			t.Fatalf("GET %s served the signing key", attempt)
		}
		if strings.Contains(body, "PRIVATE KEY") {
			t.Fatalf("GET %s leaked key material with status %d", attempt, response.StatusCode)
		}
	}
}

func TestAptRefusesToFollowASymlinkOutOfTheRoot(t *testing.T) {
	// os.Root rather than path cleaning is what makes this hold: a cleaned path can still
	// point at a symlink whose target is elsewhere.
	a, server := aptApp(t)
	dataDir := a.Config.Current().DataDir
	publishInto(t, dataDir)

	secret := filepath.Join(dataDir, "apt", "signing-key.asc")
	if err := os.WriteFile(secret, []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(feed.RepoDir(dataDir), "escape.asc")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	response, body := aptGet(t, server, "/apt/escape.asc")
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("a symlink out of the root was followed (status %d)", response.StatusCode)
	}
}

func TestAptSaysSoWhenNothingIsPublished(t *testing.T) {
	// The normal state of a fresh instance. A bare 404 sends an operator looking for a
	// routing bug they do not have.
	_, server := aptApp(t)
	response, body := aptGet(t, server, "/apt/dists/stable/InRelease")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
	if !strings.Contains(body, "publish-apt") {
		t.Errorf("the answer should name the command that fixes it:\n%s", body)
	}
}

func TestAptOffersNoDirectoryListing(t *testing.T) {
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	for _, path := range []string{"/apt/", "/apt/pool/", "/apt/dists/stable/"} {
		response, body := aptGet(t, server, path)
		if response.StatusCode == http.StatusOK && strings.Contains(body, "pkgcache_1.2.0") {
			t.Errorf("GET %s produced a listing", path)
		}
	}
}

func TestAptIsReadOnly(t *testing.T) {
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	request, err := http.NewRequest(http.MethodDelete,
		server.URL+"/apt/dists/stable/InRelease", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", response.StatusCode)
	}
}

func TestAptSupportsRangeRequests(t *testing.T) {
	// A 15 MB package over a slow link is where a resumable download matters.
	a, server := aptApp(t)
	publishInto(t, a.Config.Current().DataDir)

	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/apt/pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-15")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if len(body) != 16 {
		t.Errorf("got %d bytes, want 16", len(body))
	}
}

func TestAptPathRecognition(t *testing.T) {
	for path, want := range map[string]bool{
		"/apt":                      true,
		"/apt/dists/stable/Release": true,
		"/apt/pool/x.deb":           true,
		"/aptitude":                 false,
		"/console":                  false,
		"/global/npm/x":             false,
	} {
		if got := aptPath(path); got != want {
			t.Errorf("aptPath(%q) = %v, want %v", path, got, want)
		}
	}
}
