package dockerfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/eco/pypi"
)

func uvOptions() Options {
	return Options{Mode: Bridge, Registry: "127.0.0.1:41999", Base: "http://127.0.0.1:41999",
		Project: "global"}
}

const uvSource = `FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim AS builder
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv \
    ulimit -n "$(ulimit -Hn)" && uv sync --frozen --no-install-project --no-dev
RUN uv pip install -r requirements.txt
RUN ["uv", "sync", "--locked"]
FROM python:3.12-slim
SHELL ["pwsh", "-Command"]
RUN uv sync --locked
`

// A shell-form RUN that runs a uv project command is wrapped after its own flags; the rest
// of the file is not.
func TestUVProjectCommandsAreWrapped(t *testing.T) {
	result, err := Rewrite([]byte(uvSource), uvOptions())
	if err != nil {
		t.Fatal(err)
	}
	out := string(result.Content)
	wrapper := UVWrapper(uvOptions())
	if n := strings.Count(out, wrapper); n != 1 {
		t.Fatalf("wrapper appears %d times, want once:\n%s", n, out)
	}
	want := "RUN --mount=type=cache,target=/root/.cache/uv \\\n    " + wrapper +
		` ulimit -n "$(ulimit -Hn)" && uv sync --frozen`
	if !strings.Contains(out, want) {
		t.Fatalf("the sync RUN was not wrapped after its flags:\n%s", out)
	}
	for _, untouched := range []string{
		"\nRUN uv pip install -r requirements.txt\n",
		"\nRUN [\"uv\", \"sync\", \"--locked\"]\n",
		"\nRUN uv sync --locked\n",
	} {
		if !strings.Contains(out, untouched) {
			t.Errorf("%q was changed:\n%s", strings.TrimSpace(untouched), out)
		}
	}
	reported := false
	for _, change := range result.Changes {
		reported = reported || strings.Contains(change.To, "/global/pypi/+files/")
	}
	if !reported {
		t.Errorf("the wrap was not reported: %+v", result.Changes)
	}
}

// uv's documented Docker pattern bind-mounts the lock, read-only unless told otherwise; the
// wrapper can lend it to the cache only once the mount is writable. Only that mount changes.
func TestBindMountedLockIsMadeWritable(t *testing.T) {
	source := "FROM python:3.12-slim\n" +
		"RUN --mount=type=cache,target=/root/.cache/uv \\\n" +
		"    --mount=type=bind,source=pyproject.toml,target=/build/pyproject.toml \\\n" +
		"    --mount=type=bind,source=uv.lock,target=/build/uv.lock \\\n" +
		"    cd /build && uv sync --locked --no-install-project\n" +
		"RUN --mount=type=bind,source=uv.lock,target=uv.lock,ro uv sync --frozen\n"
	result, err := Rewrite([]byte(source), uvOptions())
	if err != nil {
		t.Fatal(err)
	}
	out := string(result.Content)
	for _, want := range []string{
		"--mount=type=bind,source=uv.lock,target=/build/uv.lock,rw \\",
		"--mount=type=bind,source=pyproject.toml,target=/build/pyproject.toml \\",
		"--mount=type=cache,target=/root/.cache/uv \\",
		"--mount=type=bind,source=uv.lock,target=uv.lock,ro ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "target=uv.lock,ro,rw") || strings.Contains(out, "pyproject.toml,rw") {
		t.Errorf("a mount that is not a writable lock was changed:\n%s", out)
	}
}

// The wrapper itself, run by a real shell against a stand-in uv: the lock names the cache
// for the length of a sync and nothing longer, the build's index is withheld from project
// commands and only them, and uv's own exit status is the RUN's.
func TestUVWrapperLendsTheLockAndHandsItBack(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	member := filepath.Join(project, "member")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{member, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The shape uv writes: each source on its own line, each file with its hash beside it.
	lock := "[[package]]\nname = \"six\"\nversion = \"1.0\"\nsource = { registry = \"https://pypi.org/simple\" }\n" +
		"sdist = { url = \"https://files.pythonhosted.org/packages/dd/ee/ff/six-1.0.tar.gz\", hash = \"sha256:" + strings.Repeat("a", 64) + "\", size = 10, upload-time = \"2020-01-01T00:00:00Z\" }\n" +
		"wheels = [\n    { url = \"https://files.pythonhosted.org/packages/aa/bb/cc/six-1.0-py3-none-any.whl\", hash = \"sha256:" + strings.Repeat("b", 64) + "\", size = 11 },\n]\n" +
		"[[package]]\nname = \"torch\"\nsource = { registry = \"https://download.pytorch.org/whl/cu126\" }\n" +
		"wheels = [\n    { url = \"https://download-r2.pytorch.org/whl/cu126/torch-2.8.0%2Bcu126-cp312-cp312-manylinux_2_28_x86_64.whl\", hash = \"sha256:" + strings.Repeat("c", 64) + "\" },\n]\n" +
		"[package.metadata]\nrequires-dist = [{ name = \"torch\", index = \"https://download.pytorch.org/whl/cu126\" }]\n"
	lockPath := filepath.Join(project, "uv.lock")
	if err := os.WriteFile(lockPath, []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	// Records what it was called with and what it saw, then exits with $FAKE_UV_EXIT.
	record := filepath.Join(root, "seen")
	fake := "#!/bin/sh\n" +
		"{ echo \"args=$*\"; echo \"index=${UV_INDEX_URL-unset}\"; cat \"" + lockPath + "\"; } >> \"" + record + "\"\n" +
		"exit ${FAKE_UV_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	ours := "http://127.0.0.1:41999/global/pypi/root/pypi/+simple/"
	run := func(index, exit, script string) (string, int) {
		t.Helper()
		_ = os.Remove(record)
		cmd := exec.Command(sh, "-euc", UVWrapper(uvOptions())+" "+script)
		cmd.Dir = member
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin",
			"UV_INDEX_URL="+index, "FAKE_UV_EXIT="+exit)
		output, runErr := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if runErr != nil {
			t.Fatalf("%s: %v\n%s", script, runErr, output)
		}
		seen, _ := os.ReadFile(record)
		return string(seen), code
	}
	handedBack := func(when string) {
		t.Helper()
		if got, _ := os.ReadFile(lockPath); string(got) != lock {
			t.Fatalf("%s: the lock was not handed back as it was:\n%s", when, got)
		}
		if _, err := os.Stat(lockPath + ".pkgcache"); err == nil {
			t.Fatalf("%s: a sed backup was left behind", when)
		}
	}

	// A frozen sync, from a member directory: the nearest lock above is lent to the cache.
	seen, code := run(ours, "0", "ulimit -n 256 && uv sync --frozen --no-dev")
	if code != 0 ||
		!strings.Contains(seen, "http://127.0.0.1:41999/global/pypi/+files/files.pythonhosted.org/packages/aa/bb/cc/six-1.0-py3-none-any.whl?sha256="+strings.Repeat("b", 64)+`", hash = "sha256:`) ||
		!strings.Contains(seen, "http://127.0.0.1:41999/global/pypi/+files/files.pythonhosted.org/packages/dd/ee/ff/six-1.0.tar.gz?sha256="+strings.Repeat("a", 64)) ||
		!strings.Contains(seen, "http://127.0.0.1:41999/global/pypi/+files/download-r2.pytorch.org/whl/cu126/torch-2.8.0%2Bcu126-cp312-cp312-manylinux_2_28_x86_64.whl?sha256="+strings.Repeat("c", 64)) ||
		!strings.Contains(seen, `index = "https://download.pytorch.org/whl/cu126"`) ||
		strings.Contains(seen, "https://files.pythonhosted.org") || strings.Contains(seen, "https://download-r2") ||
		!strings.Contains(seen, `registry = "https://pypi.org/simple"`) ||
		!strings.Contains(seen, `registry = "https://download.pytorch.org/whl/cu126"`) ||
		!strings.Contains(seen, "index=unset") {
		t.Fatalf("sync saw (exit %d):\n%s", code, seen)
	}
	handedBack("after a sync")

	// uv failing: its status is the RUN's, and the lock still goes back.
	if _, code := run(ours, "3", "uv --no-cache sync --locked"); code != 3 {
		t.Fatalf("exit %d, want uv's 3", code)
	}
	handedBack("after a failed sync")

	// uv pip keeps the build's index and never touches a lock.
	seen, _ = run(ours, "0", "uv pip install six")
	if !strings.Contains(seen, "index="+ours) || strings.Contains(seen, "+files") {
		t.Fatalf("uv pip saw:\n%s", seen)
	}

	// Other project commands lose the build's index but keep the lock as it is.
	seen, _ = run(ours, "0", "uv lock")
	if !strings.Contains(seen, "index=unset") || strings.Contains(seen, "+files") {
		t.Fatalf("uv lock saw:\n%s", seen)
	}

	// An index the Dockerfile chose itself is its own business, and is kept.
	seen, _ = run("https://mirror.example/simple/", "0", "uv sync --frozen")
	if !strings.Contains(seen, "index=https://mirror.example/simple/") {
		t.Fatalf("a Dockerfile's own index was withheld:\n%s", seen)
	}
	handedBack("at the end")

	// A lock that can be written but not replaced, as a bind-mounted file is: sed -i's
	// rename would fail here, and the sync would have gone to PyPI.
	if err := os.Chmod(project, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(project, 0o755) })
	seen, code = run(ours, "0", "uv sync --locked")
	if code != 0 || !strings.Contains(seen, "/global/pypi/+files/files.pythonhosted.org/packages/aa/bb/cc/six-1.0") {
		t.Fatalf("a lock that cannot be replaced was not lent (exit %d):\n%s", code, seen)
	}
	handedBack("after lending a lock in place")
}

// The wrapper points a lock only at hosts the pypi adapter will fetch from; a host added to
// one list and not the other would be a lock pointed at a 404.
func TestLockFileHostsMatchThePyPIRoute(t *testing.T) {
	if !slices.Equal(lockFileHosts, pypi.LockFileHosts) {
		t.Fatalf("dockerfile lends locks on %v; the pypi route fetches from %v",
			lockFileHosts, pypi.LockFileHosts)
	}
}
