//go:build !windows

package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
)

// userHome gives the test a home of its own, so "the default location" is a directory it
// controls, and returns that location.
func userHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(config.LocalEnvPrefix+"DATA_DIR", "")
	base, err := config.LocalDefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// launched records what a Migrator would have run, without running it.
type launched struct {
	name      string
	args, env []string
	calls     int
}

func (l *launched) launch(_ context.Context, name string, args, env []string, _ string) error {
	l.name, l.args, l.env = name, args, env
	l.calls++
	return nil
}

// Somebody pointing the picker at a disk means "onto that disk", and a mounted disk is
// never empty — lost+found is on every ext4 filesystem there is.
func TestPlanMigrationGivesTheCacheADirectoryOfItsOwnOnADiskThatHoldsThings(t *testing.T) {
	from := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, from)
	disk := t.TempDir()
	if err := os.Mkdir(filepath.Join(disk, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanMigration(from, disk)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(disk, "pkgcache"); plan.To != want {
		t.Errorf("destination = %q, want %q", plan.To, want)
	}
	if len(plan.Problems) != 0 {
		t.Errorf("a writable disk with room was refused: %v", plan.Problems)
	}
	if plan.Bytes == 0 || plan.Files == 0 {
		t.Errorf("nothing was counted: %+v", plan)
	}
	if plan.FreeBytes < 0 {
		t.Error("a local filesystem did not say how much it has free")
	}
	if !plan.SameDisk {
		t.Error("two temporary directories are one disk, and the plan should say so")
	}
}

func TestPlanMigrationUsesAnEmptyChoiceAsItIs(t *testing.T) {
	from := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, from)
	empty := t.TempDir()

	plan, err := PlanMigration(from, empty)
	if err != nil {
		t.Fatal(err)
	}
	if plan.To != empty {
		t.Errorf("destination = %q, want the empty directory itself, %q", plan.To, empty)
	}
}

// Problems come back as sentences for a dialog, not as a failed request: what is wrong
// with a disk is the thing somebody needs to know to choose another one.
func TestPlanMigrationReportsWhatStandsInTheWayRatherThanFailing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory, so there is nothing to refuse")
	}
	from := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, from)
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	for _, row := range []struct{ name, chosen, wants string }{
		{"nothing chosen", "", "Choose a directory"},
		{"inside itself", filepath.Join(from, "deeper"), "inside the cache"},
		{"a directory nobody can write", filepath.Join(readOnly, "new"), "cannot be written to"},
	} {
		t.Run(row.name, func(t *testing.T) {
			plan, err := PlanMigration(from, row.chosen)
			if err != nil {
				t.Fatalf("a plan with a problem failed instead: %v", err)
			}
			if !slices.ContainsFunc(plan.Problems, func(p string) bool {
				return strings.Contains(p, row.wants)
			}) {
				t.Errorf("problems = %q, want one mentioning %q", plan.Problems, row.wants)
			}
		})
	}
}

func TestMigratorHandsTheMoveToAProcessThatOutlivesTheDaemon(t *testing.T) {
	base := userHome(t)
	cache(t, base)
	// What whoever started the daemon sets on it. It must not reach the move, which would
	// read it as the location having been fixed explicitly and refuse to leave a signpost.
	t.Setenv(config.LocalEnvPrefix+"DATA_DIR", base)

	var got launched
	m := &Migrator{DataDir: base, Executable: "/usr/bin/pkgcache", launch: got.launch}
	disk := t.TempDir()
	plan, err := m.Start(context.Background(), disk)
	if err != nil {
		t.Fatal(err)
	}

	if got.name != "/usr/bin/pkgcache" {
		t.Errorf("ran %q, want the pkgcache binary itself", got.name)
	}
	want := []string{"migrate", "-from-window", "-data-dir", base, plan.To}
	if !slices.Equal(got.args, want) {
		t.Errorf("args = %q, want %q", got.args, want)
	}
	if slices.ContainsFunc(got.env, func(entry string) bool {
		return strings.HasPrefix(entry, config.LocalEnvPrefix+"DATA_DIR=")
	}) {
		t.Error("the daemon's PKGCACHE_DATA_DIR was handed to the move")
	}

	location, err := m.Location(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if location.Last == nil || location.Last.State != controlapi.MigrationMoving {
		t.Errorf("last = %+v, want a move in progress", location.Last)
	}

	// A second click while the first move is under way.
	if _, err := m.Start(context.Background(), disk); err == nil ||
		!strings.Contains(err.Error(), "already under way") {
		t.Errorf("a second move was not refused: %v", err)
	}
	if got.calls != 1 {
		t.Errorf("the move was launched %d times", got.calls)
	}
}

// The daemon cannot see where a location was chosen, only whether it is the one this user's
// cache is found at without being told. A move from the window of one that is not would
// leave the setting that chose it naming a directory that is gone.
func TestMigratorRefusesACacheWhoseLocationWasChosenElsewhere(t *testing.T) {
	userHome(t)
	elsewhere := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, elsewhere)

	var got launched
	m := &Migrator{DataDir: elsewhere, Executable: "/usr/bin/pkgcache", launch: got.launch}
	location, err := m.Location(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if location.Movable || !strings.Contains(location.Reason, "pkgcache migrate") {
		t.Errorf("location = %+v, want it refused with the terminal command named", location)
	}
	if _, err := m.Start(context.Background(), t.TempDir()); err == nil {
		t.Error("the move was started anyway")
	}
	if got.calls != 0 {
		t.Error("the move was launched despite the refusal")
	}
}

// A cache that was moved once is movable again from where it now is.
func TestMigratorTreatsTheSignpostedLocationAsTheCachesOwn(t *testing.T) {
	base := userHome(t)
	moved := filepath.Join(t.TempDir(), "pkgcache")
	cache(t, moved)
	if err := config.WriteMovedTo(base, moved); err != nil {
		t.Fatal(err)
	}

	location, err := (&Migrator{DataDir: moved}).Location(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !location.Movable {
		t.Errorf("a signposted cache was refused: %s", location.Reason)
	}
	if location.MovedFrom != base {
		t.Errorf("moved_from = %q, want %q", location.MovedFrom, base)
	}
}

// A socket-activated daemon lives in pkgcache.service's control group, and the move stops
// that service — so a child of the daemon would be killed partway through its own copy.
func TestASocketActivatedCacheIsMovedByAUnitOfItsOwn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("control groups are the Linux case")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("no systemd-run here, and the window refuses the move without it")
	}
	base := userHome(t)
	cache(t, base)

	var got launched
	m := &Migrator{
		DataDir: base, Supervised: true, Executable: "/usr/bin/pkgcache", launch: got.launch,
	}
	if _, err := m.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got.name != "systemd-run" {
		t.Fatalf("ran %q, want systemd-run", got.name)
	}
	for _, want := range []string{"--user", "--collect", "--setenv=HOME=" + os.Getenv("HOME")} {
		if !slices.Contains(got.args, want) {
			t.Errorf("args %q lack %q", got.args, want)
		}
	}
	at := slices.Index(got.args, "/usr/bin/pkgcache")
	if at < 0 || at+1 >= len(got.args) || got.args[at+1] != "migrate" {
		t.Errorf("args %q do not run pkgcache migrate inside the unit", got.args)
	}
}

func TestFinishMigrationLeavesTheOutcomeWhereTheNextDaemonLooks(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	started := time.Now()

	if err := FinishMigration(from, to, started,
		MigrateResult{From: from, To: to, Bytes: 10, Files: 2}, nil); err != nil {
		t.Fatal(err)
	}
	record, found := readMigrationRecord(to)
	if !found || record.State != controlapi.MigrationDone || record.Bytes != 10 {
		t.Errorf("after a move, the new location holds %+v", record)
	}

	// A failure is written where the cache still is, first line only: the rest of a
	// terminal's error is advice for a terminal.
	failure := errors.New("local: /x cannot hardlink\n  advice for somebody at a prompt")
	if err := FinishMigration(from, to, started, MigrateResult{}, failure); err != nil {
		t.Fatal(err)
	}
	record, found = readMigrationRecord(from)
	if !found || record.State != controlapi.MigrationFailed || record.Error != "/x cannot hardlink" {
		t.Errorf("after a failure, the old location holds %+v", record)
	}
}

// The window waits for a record that says the move is over. A move that died writes none,
// and a daemon answering long after it started is the proof it is not running.
func TestAMoveThatDiedIsReportedAsOneWhenADaemonAnswers(t *testing.T) {
	base := userHome(t)
	cache(t, base)
	if err := writeMigrationRecord(base, controlapi.MigrationRecord{
		State: controlapi.MigrationMoving, From: base, To: "/elsewhere",
		Started: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	location, err := (&Migrator{DataDir: base}).Location(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if location.Last == nil || location.Last.State != controlapi.MigrationFailed ||
		!strings.Contains(location.Last.Error, "still here") {
		t.Errorf("last = %+v, want a failure saying the cache is still here", location.Last)
	}
}

// The move's own log is written to while the copy runs. Carried across, it would make the
// far side bigger than the count taken before, and the move would refuse its own success.
func TestMigrateLeavesTheNotesOfAWindowStartedMoveBehind(t *testing.T) {
	acrossFilesystems(t)
	from, to := filepath.Join(t.TempDir(), "pkgcache"), filepath.Join(t.TempDir(), "moved")
	cache(t, from)
	for _, name := range []string{migrationLogName, migrationRecordName} {
		if err := os.WriteFile(filepath.Join(from, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Migrate(context.Background(), MigrateOptions{From: from, To: to, Keep: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{migrationLogName, migrationRecordName} {
		if _, err := os.Stat(filepath.Join(to, name)); !os.IsNotExist(err) {
			t.Errorf("%s was carried across", name)
		}
	}
}
