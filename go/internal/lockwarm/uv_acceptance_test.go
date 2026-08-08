package lockwarm

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRewrittenLockAcceptedByUVFrozenAndLocked(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv is not installed")
	}
	wheel := testWheel(t)
	sum := sha256.Sum256(wheel)
	digest := hex.EncodeToString(sum[:])
	const filename = "demo_pkg-1.0.0-py3-none-any.whl"

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/demo-pkg/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<a href="%s/files/%s#sha256=%s">%s</a>`,
				upstream.URL, filename, digest, filename)
		case "/files/" + filename:
			http.ServeContent(w, r, filename, testEpoch, bytes.NewReader(wheel))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	var cache *httptest.Server
	cache = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root/pypi/+simple/demo-pkg/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<a href="%s/root/pypi/+f/demo-pkg/%s#sha256=%s">%s</a>`,
				cache.URL, filename, digest, filename)
		case "/root/pypi/+f/demo-pkg/" + filename:
			http.ServeContent(w, r, filename, testEpoch, bytes.NewReader(wheel))
		default:
			http.NotFound(w, r)
		}
	}))
	defer cache.Close()

	project := t.TempDir()
	pyproject := `[project]
name = "lockwarm-acceptance"
version = "0.1.0"
requires-python = ">=3.11"
dependencies = ["demo-pkg==1.0.0"]
`
	if err := os.WriteFile(filepath.Join(project, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
		t.Fatal(err)
	}
	runUV(t, uv, project, nil, "lock", "--default-index", upstream.URL+"/simple")
	lockPath := filepath.Join(project, "uv.lock")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := Parse(string(lockBytes))
	if err != nil {
		t.Fatal(err)
	}
	rewritten := Rewrite(string(lockBytes), packages,
		NewIndexMap(map[string]string{"root/pypi": upstream.URL + "/simple"}),
		cache.URL)
	if err := os.WriteFile(lockPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"UV_DEFAULT_INDEX=" + cache.URL + "/root/pypi/+simple",
		"UV_NO_CACHE=1",
		"UV_PROJECT_ENVIRONMENT=" + filepath.Join(project, ".venv-frozen"),
	}
	runUV(t, uv, project, env, "sync", "--frozen", "--no-cache")
	env[len(env)-1] = "UV_PROJECT_ENVIRONMENT=" + filepath.Join(project, ".venv-locked")
	runUV(t, uv, project, env, "sync", "--locked", "--no-cache")
}

func testWheel(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	files := map[string]string{
		"demo_pkg/__init__.py":              "__version__ = '1.0.0'\n",
		"demo_pkg-1.0.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0.0\n",
		"demo_pkg-1.0.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: pkgreg-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"demo_pkg-1.0.0.dist-info/RECORD":   "",
	}
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func runUV(t *testing.T, uv, directory string, extraEnv []string, args ...string) {
	t.Helper()
	command := exec.Command(uv, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), extraEnv...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("uv %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

var testEpoch = func() time.Time {
	return time.Unix(0, 0)
}()
