package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shim, run by a real shell in front of a stand-in uv: uv's project commands lose the
// session's index and nothing else does, an index somebody chose is kept, and uv's exit
// status is the shim's.
func TestUVShimWithholdsTheSessionIndexFromProjectCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shims on Windows")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\necho \"args=$* index=${UV_INDEX_URL-unset}\"\nexit ${FAKE_UV_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	shims, err := WriteShims(filepath.Join(root, "shims"), bin)
	if err != nil || shims == "" {
		t.Fatalf("WriteShims = %q, %v", shims, err)
	}
	sessionIndex := "http://127.0.0.1:41780/work/pypi/root/pypi/+simple/"
	run := func(index, exit string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(shims, "uv"), args...)
		cmd.Env = []string{
			"PATH=" + shims + ":" + bin + ":/usr/bin:/bin",
			"PKGCACHE_BRIDGE_URL=http://127.0.0.1:41780", "PKGCACHE_PROJECT=work",
			"UV_INDEX_URL=" + index, "FAKE_UV_EXIT=" + exit,
		}
		output, err := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("uv %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output)), code
	}

	for _, args := range [][]string{
		{"sync", "--locked"}, {"--directory", "app", "lock"}, {"add", "requests"}, {"run", "pytest"},
	} {
		if got, _ := run(sessionIndex, "0", args...); !strings.HasSuffix(got, "index=unset") {
			t.Errorf("uv %v kept the session's index: %s", args, got)
		}
	}
	if got, _ := run(sessionIndex, "0", "pip", "install", "six"); !strings.HasSuffix(got, "index="+sessionIndex) {
		t.Errorf("uv pip lost the session's index: %s", got)
	}
	mirror := "https://mirror.example/simple/"
	if got, _ := run(mirror, "0", "sync"); !strings.HasSuffix(got, "index="+mirror) {
		t.Errorf("an index the user chose was withheld: %s", got)
	}
	if got, code := run(sessionIndex, "4", "sync", "--frozen"); code != 4 || !strings.Contains(got, "args=sync --frozen") {
		t.Errorf("uv's arguments or status were not passed through: %d %s", code, got)
	}
}

// A uv removed after the shim was written: the shim says so the way a shell would, rather
// than finding itself.
func TestUVShimWithoutUVSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shims on Windows")
	}
	shims := filepath.Join(t.TempDir(), "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shims, "uv"), []byte(UVShim(shims)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(shims, "uv"), "sync")
	cmd.Env = []string{"PATH=" + shims + ":/nonexistent"}
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 127 || !strings.Contains(string(output), "uv: command not found") {
		t.Fatalf("err = %v, output = %q; want exit 127 and not found", err, output)
	}
}

// A session puts its shims first on PATH once, however many sessions it is nested in.
func TestSessionPutsShimsFirstOnPathOnce(t *testing.T) {
	separator := string(os.PathListSeparator)
	base := []string{"PATH=/shims" + separator + "/usr/bin" + separator + "/bin", "HOME=/home/x"}
	env := Environment(base, Options{BaseURL: "http://127.0.0.1:41780", Shims: "/shims"})
	if got, _ := lookup(env, "PATH"); got != "/shims"+separator+"/usr/bin"+separator+"/bin" {
		t.Fatalf("PATH = %q", got)
	}
	env = Environment([]string{"HOME=/home/x"}, Options{BaseURL: "http://127.0.0.1:41780", Shims: "/shims"})
	if got, _ := lookup(env, "PATH"); got != "/shims" {
		t.Fatalf("PATH with none before = %q", got)
	}
	env = Environment([]string{"PATH=/usr/bin"}, Options{BaseURL: "http://127.0.0.1:41780"})
	if got, _ := lookup(env, "PATH"); got != "/usr/bin" {
		t.Fatalf("PATH without shims = %q", got)
	}
}

// Only a program that is installed gets a shim. A script that asks `command -v uv` before
// choosing between uv and pip found the shim on a machine with no uv, chose uv, and
// failed; the same would go for wget and curl. A shim an earlier session wrote for a
// program that has since gone is removed.
func TestShimsOnlyForProgramsThatAreInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shims on Windows")
	}
	root := t.TempDir()
	bin, shims := filepath.Join(root, "bin"), filepath.Join(root, "shims")
	for _, dir := range []string{bin, shims} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not executable, so not installed, whatever its name.
	if err := os.WriteFile(filepath.Join(bin, "wget"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shims, "uv"), []byte(UVShim(shims)), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim directory is on PATH too, and its own uv must not count as installed.
	if _, err := WriteShims(shims, shims+string(os.PathListSeparator)+bin); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"curl": true, "uv": false, "wget": false} {
		_, err := os.Stat(filepath.Join(shims, name))
		if got := err == nil; got != want {
			t.Errorf("shim for %s present = %v, want %v", name, got, want)
		}
	}
}

// The curl and wget a session stands in front of, run by a real shell before stand-ins
// that print their arguments exactly: a release download goes to the cache's git adapter
// through the session's own insteadOf pair, and nothing else changes.
func TestDownloadShimSendsReleaseDownloadsThroughTheCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shims on Windows")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// One argument per line, each between brackets, so a changed space or newline shows.
	fake := "#!/bin/sh\nfor a do printf '[%s]\\n' \"$a\"; done\nexit ${FAKE_EXIT:-0}\n"
	for _, name := range []string{"curl", "wget"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shims, err := WriteShims(filepath.Join(root, "shims"), bin)
	if err != nil {
		t.Fatal(err)
	}
	const bridge = "http://127.0.0.1:41780"
	session := []string{
		"PATH=" + shims + ":" + bin + ":/usr/bin:/bin",
		"PKGCACHE_BRIDGE_URL=" + bridge,
		// A pair of the user's own comes first, and must not be taken for the session's.
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.https://mirror.example/github/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
		"GIT_CONFIG_KEY_1=url." + bridge + "/work/git/github.com/.insteadOf",
		"GIT_CONFIG_VALUE_1=https://github.com/",
	}
	run := func(env []string, program string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(shims, program), args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%s %v: %v\n%s", program, args, err, output)
		}
		return string(output), code
	}
	brackets := func(args ...string) string {
		var b strings.Builder
		for _, arg := range args {
			b.WriteString("[" + arg + "]\n")
		}
		return b.String()
	}

	const asset = "releases/download/20251031/cpython-3.9.25+20251031-x86_64-unknown-linux-gnu-install_only.tar.gz"
	release := "https://github.com/astral-sh/python-build-standalone/" + asset
	cached := bridge + "/work/git/github.com/astral-sh/python-build-standalone/" + asset
	// The call that project's release build makes.
	if got, _ := run(session, "curl", "-fL", "--progress-bar", "-o", "a b\nc", release); got != brackets("-fL", "--progress-bar", "-o", "a b\nc", cached) {
		t.Errorf("curl's release download was not sent through the cache:\n%s", got)
	}
	if got, _ := run(session, "wget", "-qO-", release); got != brackets("-qO-", cached) {
		t.Errorf("wget's release download was not sent through the cache:\n%s", got)
	}

	unchanged := [][]string{
		// Not a release download.
		{"curl", "-fsSL", "https://github.com/astral-sh/uv/archive/refs/tags/0.9.0.tar.gz"},
		{"curl", "-fsSL", "https://github.com/astral-sh/python-build-standalone/releases/latest"},
		// Not an owner and a repository.
		{"curl", "-fsSL", "https://github.com/a/b/c/releases/download/v1/x.tgz"},
		// Credentials meant for the forge, in each spelling.
		{"curl", "-H", "Authorization: token t", release},
		{"curl", "--header=Accept: application/octet-stream", release},
		{"curl", "-fsSLu", "me:t", release},
		{"curl", "--netrc", release},
		{"wget", "--header=Authorization: token t", release},
		{"wget", "--user=me", "--password=t", release},
		// A host the session sends nowhere.
		{"curl", "-fsSL", "https://gitlab.com/a/b/releases/download/v1/x.tgz"},
	}
	for _, call := range unchanged {
		if got, _ := run(session, call[0], call[1:]...); got != brackets(call[1:]...) {
			t.Errorf("%v was changed:\n%s", call, got)
		}
	}
	// Outside a session, the shim is the program.
	outside := []string{"PATH=" + shims + ":" + bin + ":/usr/bin:/bin", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url." + bridge + "/work/git/github.com/.insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/"}
	if got, _ := run(outside, "curl", release); got != brackets(release) {
		t.Errorf("outside a session the download was changed:\n%s", got)
	}
	if got, code := run(append(session, "FAKE_EXIT=22"), "curl", "-f", release); code != 22 || got != brackets("-f", cached) {
		t.Errorf("curl's status was not passed through: %d\n%s", code, got)
	}
}
