// Package acceptance exercises the adapters through real package-manager clients.
//
// The origins are recorded, deterministic fixtures served on loopback. A missing
// client binary skips only that client's test, which keeps local development
// friendly while CI installs the required clients and therefore treats every skip as a setup
// error.
package acceptance

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brightskies/pkgreg/internal/eco"
	aptrepo "github.com/brightskies/pkgreg/internal/eco/apt"
	"github.com/brightskies/pkgreg/internal/eco/ecotest"
	"github.com/brightskies/pkgreg/internal/eco/files"
	"github.com/brightskies/pkgreg/internal/eco/npm"
	ocirepo "github.com/brightskies/pkgreg/internal/eco/oci"
	"github.com/brightskies/pkgreg/internal/eco/pypi"
	testupstream "github.com/brightskies/pkgreg/internal/testutil/upstream"
)

const (
	alpineClientImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	debianClientImage = "debian@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818"
)

func TestDockerPull(t *testing.T) {
	docker := requireDocker(t)
	layer, diffID := dockerLayer(t)
	configBody := dockerConfig(t, diffID)
	configDigest := sha256Hex(configBody)
	layerDigest := sha256Hex(layer)
	manifestBody := dockerManifest(t, configBody, layer)
	manifestDigest := sha256Hex(manifestBody)
	indexBody := dockerIndex(t, manifestBody)

	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		const repo = "/v2/library/pkgreg-acceptance"
		origin.Handle(repo+"/manifests/latest", testupstream.Behaviour{
			Body: indexBody, RequireBearer: true,
			ContentType: "application/vnd.oci.image.index.v1+json",
		})
		origin.Handle(repo+"/manifests/sha256:"+manifestDigest,
			testupstream.Behaviour{
				Body: manifestBody, RequireBearer: true,
				ContentType: "application/vnd.docker.distribution.manifest.v2+json",
			})
		origin.Handle(repo+"/blobs/sha256:"+configDigest,
			testupstream.Behaviour{Body: configBody, RequireBearer: true})
		origin.Handle(repo+"/blobs/sha256:"+layerDigest,
			testupstream.Behaviour{Body: layer, RequireBearer: true})
		return ocirepo.NewWithRegistries(map[string]string{"dockerhub": origin.URL})
	})

	ref := strings.TrimPrefix(h.Server.URL, "http://") +
		"/dockerhub/pkgreg-acceptance:latest"
	t.Cleanup(func() {
		_ = exec.Command(docker, "image", "rm", "--force", ref).Run()
	})
	if out, err := exec.Command(docker, "pull", ref).CombinedOutput(); err != nil {
		t.Fatalf("docker pull: %v\n%s", err, out)
	}
	out, err := exec.Command(docker, "image", "inspect",
		"--format", "{{.Os}}/{{.Architecture}}", ref).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "linux/amd64" {
		t.Fatalf("docker inspect: output=%q err=%v", out, err)
	}
}

func TestAPKAdd(t *testing.T) {
	docker := requireDocker(t)
	if out, err := exec.Command(docker, "image", "inspect", alpineClientImage).CombinedOutput(); err != nil {
		t.Skipf("pinned Alpine apk client image is unavailable: %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	index := apkIndex(t, "")
	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		origin.Handle("/v3.20/main/x86_64/APKINDEX.tar.gz", testupstream.Behaviour{
			Body: index, ETag: `"apkindex-v1"`, ContentType: "application/gzip",
		})
		return aptrepo.New()
	})

	cmd := exec.Command(docker, "run", "--rm", "--network", "host",
		"--env", "PKGREG_REPOSITORY="+h.Origin.URL+"/v3.20/main",
		"--env", "http_proxy="+h.Server.URL,
		alpineClientImage,
		"sh", "-ec",
		`printf '%s\n' "$PKGREG_REPOSITORY" > /etc/apk/repositories
apk add --no-cache --allow-untrusted busybox`,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apk add: %v\n%s", err, out)
	}
	if hits := h.Origin.Hits("/v3.20/main/x86_64/APKINDEX.tar.gz"); hits != 1 {
		t.Fatalf("apk did not fetch its recorded index through pkgreg: hits=%d", hits)
	}
}

func TestAPTGetUpdateAndInstall(t *testing.T) {
	docker := requireDocker(t)
	if out, err := exec.Command(docker, "image", "inspect", debianClientImage).CombinedOutput(); err != nil {
		t.Skipf("pinned Debian apt client image is unavailable: %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	packageBody := debPackage(t)
	packageDigest := sha256Hex(packageBody)
	packages := []byte(fmt.Sprintf(`Package: demo-pkg
Version: 1.0-1
Architecture: amd64
Maintainer: pkgreg acceptance
Filename: pool/main/d/demo-pkg/demo-pkg_1.0-1_amd64.deb
Size: %d
SHA256: %s
Description: recorded fixture

`, len(packageBody), packageDigest))
	packagesGZ := gzipBytes(t, packages)
	release := []byte(fmt.Sprintf(`Origin: pkgreg acceptance
Label: pkgreg acceptance
Suite: stable
Codename: stable
Date: Mon, 27 Jul 2026 00:00:00 UTC
Architectures: amd64
Components: main
SHA256:
 %s %d main/binary-amd64/Packages
 %s %d main/binary-amd64/Packages.gz
`, sha256Hex(packages), len(packages), sha256Hex(packagesGZ), len(packagesGZ)))

	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		origin.Handle("/dists/stable/Release", testupstream.Behaviour{
			Body: release, ETag: `"release-v1"`, ContentType: "text/plain",
		})
		origin.Handle("/dists/stable/main/binary-amd64/Packages", testupstream.Behaviour{
			Body: packages, ETag: `"packages-v1"`, ContentType: "text/plain",
		})
		origin.Handle("/dists/stable/main/binary-amd64/Packages.gz", testupstream.Behaviour{
			Body: packagesGZ, ETag: `"packages-gz-v1"`, ContentType: "application/gzip",
		})
		origin.Serve("/pool/main/d/demo-pkg/demo-pkg_1.0-1_amd64.deb", packageBody)
		return aptrepo.New()
	})

	cmd := exec.Command(docker, "run", "--rm", "--network", "host",
		"--env", "PKGREG_REPOSITORY="+h.Origin.URL,
		"--env", "PKGREG_PROXY="+h.Server.URL,
		debianClientImage,
		"sh", "-ec",
		`printf 'deb [trusted=yes] %s stable main\n' "$PKGREG_REPOSITORY" > /etc/apt/sources.list
rm -f /etc/apt/sources.list.d/*
apt-get -o Acquire::Languages=none -o Acquire::http::Proxy="$PKGREG_PROXY" update
apt-get -o Acquire::Languages=none -o Acquire::http::Proxy="$PKGREG_PROXY" \
  --allow-unauthenticated install -y --no-install-recommends demo-pkg=1.0-1
test "$(cat /usr/share/pkgreg-acceptance.txt)" = "installed through pkgreg"`,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apt-get update && install: %v\n%s", err, out)
	}
	if hits := h.Origin.Hits("/pool/main/d/demo-pkg/demo-pkg_1.0-1_amd64.deb"); hits != 1 {
		t.Fatalf("apt package origin hits = %d, want 1", hits)
	}
}

func TestNPMCI(t *testing.T) {
	npmBin := requireBinary(t, "npm")
	tarball := npmTarball(t)
	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		doc := map[string]any{
			"name":      "left-pad",
			"dist-tags": map[string]string{"latest": "1.0.0"},
			"versions": map[string]any{
				"1.0.0": map[string]any{
					"name": "left-pad", "version": "1.0.0", "main": "index.js",
					"dist": map[string]string{
						"tarball": origin.URLFor("/packages/left-pad-1.0.0.tgz"),
					},
				},
			},
		}
		body, _ := json.Marshal(doc)
		origin.Handle("/left-pad", testupstream.Behaviour{
			Body: body, ContentType: "application/json",
		})
		origin.Serve("/packages/left-pad-1.0.0.tgz", tarball)
		return npm.NewWithOrigin(origin.URL)
	})

	work := t.TempDir()
	writeTestFile(t, filepath.Join(work, "package.json"),
		[]byte(`{"name":"client-test","private":true}`))
	cmd := exec.Command(npmBin,
		"install", "--ignore-scripts", "--no-audit", "--no-fund",
		"--registry", h.Server.URL+"/global/npm/",
		"--cache", filepath.Join(work, ".npm-cache"),
		"--userconfig", filepath.Join(work, ".npmrc"),
		"left-pad@1.0.0",
	)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "NPM_CONFIG_UPDATE_NOTIFIER=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	// npm install above creates the lockfile fixture. Remove the materialized tree
	// and prove the CI path can recreate it exclusively through pkgreg.
	if err := os.RemoveAll(filepath.Join(work, "node_modules")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(npmBin,
		"ci", "--ignore-scripts", "--no-audit", "--no-fund",
		"--registry", h.Server.URL+"/global/npm/",
		"--cache", filepath.Join(work, ".npm-cache"),
		"--userconfig", filepath.Join(work, ".npmrc"),
	)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "NPM_CONFIG_UPDATE_NOTIFIER=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm ci: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(work, "node_modules", "left-pad", "index.js"))
	if err != nil || !strings.Contains(string(body), "leftPad") {
		t.Fatalf("npm did not install the fixture: body=%q err=%v", body, err)
	}
	if hits := h.Origin.Hits("/packages/left-pad-1.0.0.tgz"); hits != 1 {
		t.Fatalf("npm fetched the tarball %d times, want 1", hits)
	}
}

func TestUVPipInstall(t *testing.T) {
	uvBin := requireBinary(t, "uv")
	h := wheelHarness(t)
	target := filepath.Join(t.TempDir(), "site")
	cmd := exec.Command(uvBin,
		"pip", "install",
		"--target", target,
		"--index-url", h.Server.URL+"/global/pypi/root/pypi/+simple/",
		"--no-cache", "--no-deps",
		"demo-pkg==1.0.0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uv pip install: %v\n%s", err, out)
	}
	assertPythonFixture(t, target)
}

func TestUVSync(t *testing.T) {
	uvBin := requireBinary(t, "uv")
	python := requireBinary(t, "python3")
	h := wheelHarness(t)
	work := t.TempDir()
	writeTestFile(t, filepath.Join(work, "pyproject.toml"), []byte(`[project]
name = "pkgreg-client-test"
version = "0.0.0"
requires-python = ">=3.8"
dependencies = ["demo-pkg==1.0.0"]
`))
	cmd := exec.Command(uvBin,
		"sync",
		"--project", work,
		"--python", python,
		"--index-url", h.Server.URL+"/global/pypi/root/pypi/+simple/",
		"--no-cache", "--no-install-project",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uv sync: %v\n%s", err, out)
	}
	matches, err := filepath.Glob(filepath.Join(work, ".venv", "lib", "python*",
		"site-packages", "demo_pkg", "__init__.py"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("uv sync did not install the fixture: matches=%v err=%v", matches, err)
	}
}

func TestPipInstall(t *testing.T) {
	python := requireBinary(t, "python3")
	if err := exec.Command(python, "-m", "pip", "--version").Run(); err != nil {
		t.Skip("python3 is present but the pip module is not installed")
	}
	h := wheelHarness(t)
	target := filepath.Join(t.TempDir(), "site")
	cmd := exec.Command(python,
		"-m", "pip", "install",
		"--disable-pip-version-check", "--no-cache-dir", "--no-deps",
		"--target", target,
		"--index-url", h.Server.URL+"/global/pypi/root/pypi/+simple/",
		"demo-pkg==1.0.0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pip install: %v\n%s", err, out)
	}
	assertPythonFixture(t, target)
}

func TestWgetRecursive(t *testing.T) {
	wget := requireBinary(t, "wget")
	h := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return files.New(staticTokens{token: "acceptance-token"}, 0)
	})
	for path, body := range map[string]string{
		"/builds/app.txt":        "root artifact",
		"/builds/nested/log.txt": "nested artifact",
	} {
		req, _ := http.NewRequest(http.MethodPut, h.URL(path), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer acceptance-token")
		if resp := h.Do(req); resp.Status != http.StatusCreated {
			t.Fatalf("PUT %s = %d: %s", path, resp.Status, resp.Text())
		}
	}

	outDir := filepath.Join(t.TempDir(), "mirror")
	cmd := exec.Command(wget,
		"--quiet", "--recursive", "--no-parent", "--no-host-directories",
		"--directory-prefix", outDir,
		h.Server.URL+"/global/files/builds/",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wget -r: %v\n%s", err, out)
	}
	found := map[string]string{}
	err := filepath.WalkDir(outDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found[entry.Name()] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found["app.txt"] != "root artifact" || found["log.txt"] != "nested artifact" {
		t.Fatalf("wget mirror is incomplete: %+v", found)
	}
}

type staticTokens struct{ token string }

func (s staticTokens) HasToken(_, _, scope string) bool {
	return scope == "write" && s.token != ""
}

func (s staticTokens) VerifyToken(_, _, scope, token string) bool {
	return scope == "write" && token == s.token
}

func wheelHarness(t *testing.T) *ecotest.Harness {
	t.Helper()
	const filename = "demo_pkg-1.0.0-py3-none-any.whl"
	wheel := pythonWheel(t)
	sum := sha256.Sum256(wheel)
	digest := hex.EncodeToString(sum[:])
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		doc := map[string]any{
			"meta": map[string]string{"api-version": "1.1"},
			"name": "demo-pkg",
			"files": []any{map[string]any{
				"filename":        filename,
				"url":             origin.URLFor("/packages/" + filename),
				"hashes":          map[string]string{"sha256": digest},
				"requires-python": ">=3.8",
				"yanked":          false,
				"core-metadata":   false,
			}},
		}
		body, _ := json.Marshal(doc)
		origin.Handle("/demo-pkg/", testupstream.Behaviour{
			Body: body, ContentType: "application/vnd.pypi.simple.v1+json",
		})
		origin.Serve("/packages/"+filename, wheel)
		return pypi.NewWithIndexes(map[string]string{"root/pypi": origin.URL})
	})
}

func npmTarball(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"package/package.json": `{"name":"left-pad","version":"1.0.0","main":"index.js"}`,
		"package/index.js":     `module.exports = function leftPad(s) { return String(s); };`,
	}
	for name, body := range files {
		header := &tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)),
		}
		if err := tw.WriteHeader(header); err != nil {
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

func pythonWheel(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	files := map[string]string{
		"demo_pkg/__init__.py": "__version__ = '1.0.0'\n",
		"demo_pkg-1.0.0.dist-info/METADATA": "Metadata-Version: 2.1\n" +
			"Name: demo-pkg\nVersion: 1.0.0\n",
		"demo_pkg-1.0.0.dist-info/WHEEL": "Wheel-Version: 1.0\n" +
			"Generator: pkgreg-acceptance\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"demo_pkg-1.0.0.dist-info/RECORD": "demo_pkg/__init__.py,,\n" +
			"demo_pkg-1.0.0.dist-info/METADATA,,\n" +
			"demo_pkg-1.0.0.dist-info/WHEEL,,\n" +
			"demo_pkg-1.0.0.dist-info/RECORD,,\n",
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func dockerLayer(t *testing.T) (compressed []byte, diffID string) {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	body := []byte("pkgreg OCI acceptance\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "pkgreg-acceptance.txt", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	diffID = sha256Hex(raw.Bytes())

	var zipped bytes.Buffer
	gz := gzip.NewWriter(&zipped)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return zipped.Bytes(), diffID
}

func dockerConfig(t *testing.T, diffID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"created":      "2026-07-27T00:00:00Z",
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]any{},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + diffID},
		},
		"history": []any{map[string]any{
			"created":    "2026-07-27T00:00:00Z",
			"created_by": "pkgreg acceptance fixture",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func dockerManifest(t *testing.T, configBody, layer []byte) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      len(configBody),
			"digest":    "sha256:" + sha256Hex(configBody),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(layer),
			"digest":    "sha256:" + sha256Hex(layer),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func dockerIndex(t *testing.T, manifest []byte) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"size":      len(manifest),
			"digest":    "sha256:" + sha256Hex(manifest),
			"platform": map[string]string{
				"os": "linux", "architecture": "amd64",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func debPackage(t *testing.T) []byte {
	t.Helper()
	control := tarGzipFile(t, "control", []byte(`Package: demo-pkg
Version: 1.0-1
Architecture: amd64
Maintainer: pkgreg acceptance
Description: recorded package installed through pkgreg
`))
	data := tarGzipFile(t, "usr/share/pkgreg-acceptance.txt",
		[]byte("installed through pkgreg\n"))

	var archive bytes.Buffer
	archive.WriteString("!<arch>\n")
	for _, member := range []struct {
		name string
		body []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", control},
		{"data.tar.gz", data},
	} {
		header := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n",
			member.name+"/", 0, 0, 0, 0o100644, len(member.body))
		if len(header) != 60 {
			t.Fatalf("invalid ar header length %d for %s", len(header), member.name)
		}
		archive.WriteString(header)
		archive.Write(member.body)
		if len(member.body)%2 != 0 {
			archive.WriteByte('\n')
		}
	}
	return archive.Bytes()
}

func tarGzipFile(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func apkIndex(t *testing.T, body string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "APKINDEX", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker := requireBinary(t, "docker")
	if out, err := exec.Command(docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon is unavailable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return docker
}

func requireBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return path
}

func assertPythonFixture(t *testing.T, target string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(target, "demo_pkg", "__init__.py"))
	if err != nil || !strings.Contains(string(body), "1.0.0") {
		t.Fatalf("Python client did not install the fixture: body=%q err=%v", body, err)
	}
}

func writeTestFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(fmt.Errorf("write %s: %w", path, err))
	}
}
