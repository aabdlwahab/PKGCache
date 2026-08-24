package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/feed"
)

// doctorOn runs the real command against a data directory and reports the error it
// would have exited on. Going through runDoctor rather than the pieces is deliberate:
// the defects these tests pin were in how the command composed its checks, not in any
// single check.
func doctorOn(t *testing.T, dataDir string) error {
	t.Helper()
	// Prefer the generated configuration when one exists, because that is the file an
	// operator points doctor at and the only place an override can live: -data-dir
	// alone never reads pkgreg.yaml.
	args := []string{"-data-dir", dataDir}
	if configPath := filepath.Join(dataDir, "pkgreg.yaml"); fileExists(configPath) {
		args = []string{"-config", configPath}
	}
	// Silence the report: these tests assert on outcome, and doctor writes a page of
	// human-facing text to stdout.
	restore := swapStdout(t)
	defer restore()
	return runDoctor(context.Background(), args)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func swapStdout(t *testing.T) func() {
	t.Helper()
	original := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devNull
	return func() {
		os.Stdout = original
		_ = devNull.Close()
	}
}

// TestDoctorDoesNotCreateAnything is the regression test for a diagnostic that changed
// the system it was diagnosing. Pointed at a path that did not exist, doctor used to
// build a data directory, a catalog, a control database and a host key there and then
// report the result healthy — so a typo'd -data-dir was silently populated instead of
// reported, and the operator was told the wrong host was fine.
func TestDoctorDoesNotCreateAnything(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-initialized")

	err := doctorOn(t, missing)
	if !errors.Is(err, errNotInitialized) {
		t.Fatalf("doctor on a missing directory = %v, want errNotInitialized", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		entries, _ := os.ReadDir(missing)
		t.Fatalf("doctor created %s (%d entries)", missing, len(entries))
	}
	if got := exitCode(err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
}

// TestDoctorRefusesADirectoryThatIsNotAPkgregStore: an existing but unrelated directory
// — a home directory, a mount point — must be reported, not adopted.
func TestDoctorRefusesADirectoryThatIsNotAPkgregStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := doctorOn(t, dir); !errors.Is(err, errNotInitialized) {
		t.Fatalf("doctor on an unrelated directory = %v, want errNotInitialized", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("doctor added %d entries to an unrelated directory", len(entries)-1)
	}
}

// TestDoctorFailsOnAnUnsafePosture: an initialized host that would serve an
// unauthenticated control plane, or relay anywhere, on a non-loopback listener is not
// healthy. Reporting it as healthy is how that configuration reached a network.
func TestDoctorFailsOnAnUnsafePosture(t *testing.T) {
	// The starter configuration `init` writes, unmodified: authentication is on, but
	// the allowlist is empty and the listener answers on every interface.
	dir := initializedDataDir(t, "")

	err := doctorOn(t, dir)
	if !errors.Is(err, errUnsafePosture) {
		t.Fatalf("doctor on an open-relay instance = %v, want errUnsafePosture", err)
	}
	if got := exitCode(err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
}

// TestDoctorPassesOnASafePosture proves the check is satisfiable — a hardening gate
// that cannot be cleared is a gate people route around.
func TestDoctorPassesOnASafePosture(t *testing.T) {
	dir := initializedDataDir(t, `
server:
  single_port: true
  unified_addr: "127.0.0.1:8443"
  proxy_allowlist: ["archive.ubuntu.com"]
`)
	if err := doctorOn(t, dir); errors.Is(err, errUnsafePosture) {
		t.Fatalf("a loopback, allowlisted instance was reported unsafe: %v", err)
	}
}

// TestDoctorAcceptsAnExplicitRelayAnywhere: "*" is the operator saying they know. It
// must clear the gate, or the acknowledgement is not one.
func TestDoctorAcceptsAnExplicitRelayAnywhere(t *testing.T) {
	dir := initializedDataDir(t, `
server:
  proxy_allowlist: ["*"]
`)
	if err := doctorOn(t, dir); errors.Is(err, errUnsafePosture) {
		t.Fatalf(`["*"] did not clear the posture gate: %v`, err)
	}
}

// initializedDataDir runs the real `pkgreg init` so these tests exercise what an
// operator actually produces, then optionally replaces the generated configuration.
//
// The replacement is written as YAML rather than by mutating a Snapshot and calling
// writeStarterConfig, because that function emits a fixed template: it substitutes
// paths and addresses but always writes `proxy_allowlist: []`. Going through the file
// is also what doctor itself does, so the test exercises the real load path.
func initializedDataDir(t *testing.T, overrideYAML string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	restore := swapStdout(t)
	if err := runInit(context.Background(), []string{"-data-dir", dir}); err != nil {
		restore()
		t.Fatal(err)
	}
	restore()

	if overrideYAML == "" {
		return dir
	}
	body := "data_dir: " + dir + "\n" + overrideYAML
	if err := os.WriteFile(filepath.Join(dir, "pkgreg.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Confirm the override is loadable, so a malformed fixture fails as a test bug
	// rather than as a doctor finding.
	if _, err := config.Load(config.Flags{
		ConfigFile: filepath.Join(dir, "pkgreg.yaml"),
	}); err != nil {
		t.Fatalf("test fixture config is invalid: %v", err)
	}
	return dir
}

// TestHelpIsNotAnError goes through dispatch, because the defect was in dispatch: every
// subcommand printed its usage, then "pkgreg: flag: help requested" to stderr, then
// exited 1. Shell probes, packaging checks and first-time users all read that as a
// broken command.
func TestHelpIsNotAnError(t *testing.T) {
	for _, command := range []string{
		"serve", "init", "doctor", "audit", "checkpoint", "rollback", "export",
		"import", "lockwarm", "gc", "evict", "publish-client", "publish-apt", "version",
	} {
		for _, form := range []string{"-h", "--help"} {
			t.Run(command+" "+form, func(t *testing.T) {
				restoreOut := swapStdout(t)
				defer restoreOut()
				restoreErr := swapStderr(t)
				defer restoreErr()

				originalArgs := os.Args
				os.Args = []string{"pkgreg", command, form}
				defer func() { os.Args = originalArgs }()

				if got := dispatch(); got != 0 {
					t.Errorf("`pkgreg %s %s` exited %d, want 0", command, form, got)
				}
			})
		}
	}
}

// TestTopLevelHelpAndUnknownCommand keeps the two statuses that were already right:
// asking for help succeeds, and a mistyped command is a usage error.
func TestTopLevelHelpAndUnknownCommand(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"pkgreg", "-h"}, 0},
		{[]string{"pkgreg", "--help"}, 0},
		{[]string{"pkgreg", "help"}, 0},
		{[]string{"pkgreg"}, 2},
		{[]string{"pkgreg", "notacommand"}, 2},
	}
	for _, tc := range cases {
		restoreOut := swapStdout(t)
		restoreErr := swapStderr(t)
		originalArgs := os.Args
		os.Args = tc.args
		got := dispatch()
		os.Args = originalArgs
		restoreErr()
		restoreOut()

		if got != tc.want {
			t.Errorf("%v exited %d, want %d", tc.args, got, tc.want)
		}
	}
}

func swapStderr(t *testing.T) func() {
	t.Helper()
	original := os.Stderr
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = devNull
	return func() {
		os.Stderr = original
		_ = devNull.Close()
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errNotInitialized, 3},
		{errUnsafePosture, 4},
		{errUnhealthy, 1},
		{errors.New("something else"), 1},
	}
	for _, tc := range cases {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// The apt repository check answers, on the server, the question apt asks on every client.
// These are the two failures worth catching: a signature that does not verify, and an
// index that has been rewritten since the Release covering it was signed.

// publishTestRepo writes a small signed repository into a data directory.
func publishTestRepo(t *testing.T, dataDir string) {
	t.Helper()
	control := "Package: pkgcache\nVersion: 1.2.0\nArchitecture: amd64\n" +
		"Maintainer: pkgreg <root@localhost>\nDescription: test\n"

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
	member("data.tar.gz", []byte("payload"))

	source := t.TempDir()
	path := filepath.Join(source, "pkgcache_1.2.0_amd64.deb")
	if err := os.WriteFile(path, deb.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := feed.GenerateKey(feed.KeyOptions{
		Name: "test", Email: "t@example.invalid", Algorithm: feed.KeyEd25519,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := feed.WriteRepository(feed.RepoOptions{
		Root: feed.RepoDir(dataDir), Debs: []string{path}, Key: key,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorAcceptsAFreshlyPublishedRepository(t *testing.T) {
	dataDir := t.TempDir()
	publishTestRepo(t, dataDir)

	d := &diagnosis{}
	d.checkAptRepository(dataDir)
	if d.failures != 0 || d.warnings != 0 {
		t.Errorf("a good repository should pass cleanly:\n%s",
			strings.Join(d.lines, "\n"))
	}
	if !strings.Contains(strings.Join(d.lines, "\n"), "verifies") {
		t.Errorf("the check should say what it verified:\n%s",
			strings.Join(d.lines, "\n"))
	}
}

func TestDoctorIsQuietWhenNothingIsPublished(t *testing.T) {
	// The normal state of most instances, and not a fault.
	d := &diagnosis{}
	d.checkAptRepository(t.TempDir())
	if d.failures != 0 || d.warnings != 0 {
		t.Errorf("an instance with no repository is healthy:\n%s",
			strings.Join(d.lines, "\n"))
	}
}

func TestDoctorCatchesARewrittenIndex(t *testing.T) {
	// This is the failure that would otherwise surface as a hash mismatch on somebody
	// else's laptop, with a message that names neither end of the problem.
	dataDir := t.TempDir()
	publishTestRepo(t, dataDir)

	index := filepath.Join(feed.RepoDir(dataDir),
		"dists", "stable", "main", "binary-amd64", "Packages")
	if err := os.WriteFile(index, []byte("Package: something-else\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &diagnosis{}
	d.checkAptRepository(dataDir)
	if d.failures == 0 {
		t.Fatalf("a rewritten index must fail the check:\n%s", strings.Join(d.lines, "\n"))
	}
}

func TestDoctorCatchesAMissingKeyring(t *testing.T) {
	// A signed repository nobody can verify is one nobody can install from.
	dataDir := t.TempDir()
	publishTestRepo(t, dataDir)
	if err := os.Remove(filepath.Join(feed.RepoDir(dataDir),
		"pkgcache-archive-keyring.asc")); err != nil {
		t.Fatal(err)
	}

	d := &diagnosis{}
	d.checkAptRepository(dataDir)
	if d.failures == 0 {
		t.Fatalf("a missing keyring must fail the check:\n%s", strings.Join(d.lines, "\n"))
	}
}
