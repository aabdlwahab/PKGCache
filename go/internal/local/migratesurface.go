package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	controlapi "github.com/aabdlwahab/PKGCache/internal/control/api"
	"github.com/aabdlwahab/PKGCache/internal/diskusage"
)

// Moving the cache from the window, as the daemon does it.
//
// The daemon cannot perform the move: it holds the lock on the directory being moved, and
// the move begins by stopping it. What it can do is everything before that — say where the
// cache is, answer whether a destination will work, and start a process that outlives it
// to do the rest. That process is `pkgcache migrate -from-window`, the same command a
// terminal runs, so there is one implementation of the move and this file is only the
// hand-off and the note it leaves for the window to read when the cache comes back.

const (
	// migrationRecordName is how a window-started move reports how it went. It is
	// written into whichever directory the cache is in when the move is over, because
	// that is the directory the next daemon — the one the window will be talking to —
	// is serving.
	migrationRecordName = "migration.json"
	// migrationLogName is where the move's own output goes when nothing else collects it.
	migrationLogName = "migrate.log"
	// staleMove is how old a "moving" record can be while a daemon is answering before it
	// is read as a move that died. A live move stops the daemon within seconds of
	// starting, so a daemon that is up to read the record minutes later is proof the move
	// is not running.
	staleMove = 2 * time.Minute
)

func migrationRecordPath(dataDir string) string {
	return filepath.Join(dataDir, migrationRecordName)
}

func migrationLogPath(dataDir string) string { return filepath.Join(dataDir, migrationLogName) }

// Migrator implements the control plane's migration surface for a local cache.
type Migrator struct {
	// DataDir is the cache this daemon serves.
	DataDir string
	// Supervised means systemd or launchd started this daemon through socket activation.
	Supervised bool
	// Executable is the pkgcache binary the move runs as. Empty means this process's.
	Executable string
	// launch starts the move. Nil means the real thing; tests replace it to see what
	// would have been run without running it.
	launch func(ctx context.Context, name string, args, env []string, log string) error
}

var _ controlapi.LocalMigration = (*Migrator)(nil)

// Location says where the cache is and whether this window may move it.
func (m *Migrator) Location(context.Context) (controlapi.MigrationLocation, error) {
	location := controlapi.MigrationLocation{DataDir: m.DataDir}
	if base, err := config.LocalDefaultDataDir(); err == nil {
		if target, moved := config.MovedTo(base); moved && target == filepath.Clean(m.DataDir) {
			location.MovedFrom = base
		}
	}
	if reason := m.immovable(); reason != "" {
		location.Reason = reason
	} else {
		location.Movable = true
	}
	if record, found := readMigrationRecord(m.DataDir); found {
		if record.State == controlapi.MigrationMoving && time.Since(record.Started) > staleMove {
			// See staleMove. Said as a failure with the one fact that matters — nothing
			// was lost — because the alternative is a window waiting forever on a move
			// no process is performing.
			record.State = controlapi.MigrationFailed
			record.Error = "the move stopped without finishing, and the cache is still here. " +
				"Its output is in " + migrationLogPath(m.DataDir)
		}
		location.Last = &record
	}
	return location, nil
}

// Plan answers whether the cache can move to the directory somebody chose.
func (m *Migrator) Plan(_ context.Context, chosen string) (controlapi.MigrationPlan, error) {
	plan, err := PlanMigration(m.DataDir, chosen)
	if err != nil {
		return plan, err
	}
	if reason := m.immovable(); reason != "" {
		plan.Problems = append([]string{reason}, plan.Problems...)
	}
	return plan, nil
}

// Start hands the move to a process that outlives this daemon, and returns.
func (m *Migrator) Start(ctx context.Context, chosen string) (controlapi.MigrationPlan, error) {
	plan, err := m.Plan(ctx, chosen)
	if err != nil {
		return plan, err
	}
	if len(plan.Problems) > 0 {
		return plan, control.NewError(http.StatusConflict, "migration_refused",
			"%s", strings.Join(plan.Problems, " "))
	}
	if record, found := readMigrationRecord(m.DataDir); found &&
		record.State == controlapi.MigrationMoving && time.Since(record.Started) < staleMove {
		return plan, control.NewError(http.StatusConflict, "migration_running",
			"a move of this cache is already under way")
	}

	name, args, env, err := m.command(plan)
	if err != nil {
		return plan, err
	}
	// Written before the process starts, so a window that reloads in the second before
	// this daemon is stopped sees a move in progress rather than an idle cache.
	if err := writeMigrationRecord(m.DataDir, controlapi.MigrationRecord{
		State: controlapi.MigrationMoving, From: plan.From, To: plan.To,
		Bytes: plan.Bytes, Files: plan.Files, Started: time.Now(),
	}); err != nil {
		return plan, err
	}
	launch := m.launch
	if launch == nil {
		launch = launchMove
	}
	if err := launch(ctx, name, args, env, migrationLogPath(m.DataDir)); err != nil {
		_ = clearMigrationRecord(m.DataDir)
		return plan, err
	}
	return plan, nil
}

// Acknowledge forgets the last move's outcome.
func (m *Migrator) Acknowledge() error { return clearMigrationRecord(m.DataDir) }

// immovable says why a window must not move this cache, or nothing if it may.
func (m *Migrator) immovable() string {
	// The daemon is always started with PKGCACHE_DATA_DIR set, by the client that spawned
	// it and by the socket unit alike, so the variable being present proves nothing. What
	// matters is whether the directory is the one this user's cache would be found at
	// without it — the default, or wherever a signpost there points. Anything else was
	// chosen explicitly somewhere this process cannot see, and a move that left that
	// setting naming the old place would strand the cache on the next start.
	base, err := config.LocalDefaultDataDir()
	if err != nil {
		return err.Error()
	}
	resolved := base
	if target, moved := config.MovedTo(base); moved {
		resolved = target
	}
	if filepath.Clean(resolved) != filepath.Clean(m.DataDir) {
		return "This cache's location was chosen explicitly, with " + config.LocalEnvPrefix +
			"DATA_DIR or -data-dir, and moving it from here would leave that setting naming the " +
			"old place. Move it from a terminal: pkgcache migrate <directory>."
	}
	if m.Supervised && runtime.GOOS == "linux" {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return "systemd starts this cache, and moving it from the window needs systemd-run, " +
				"which is not installed. Move it from a terminal: pkgcache migrate <directory>."
		}
	}
	return ""
}

// command is the process that performs the move.
//
// The same migrate a terminal runs, with the source named rather than inherited and
// PKGCACHE_DATA_DIR removed from its environment: the variable is set on this daemon by
// whoever started it, and left in place it would tell the move that the location had been
// fixed explicitly, which is the one case it refuses to signpost.
func (m *Migrator) command(plan controlapi.MigrationPlan) (name string, args, env []string, err error) {
	executable := m.Executable
	if executable == "" {
		if executable, err = os.Executable(); err != nil {
			return "", nil, nil, fmt.Errorf("local: locate this program: %w", err)
		}
	}
	move := []string{"migrate", "-from-window", "-data-dir", plan.From}
	// The address this daemon is actually on, so the one started after the move is where
	// the window already points — which is not always the fixed port.
	if state, err := ReadState(m.DataDir); err == nil && state.Addr != "" {
		move = append(move, "-addr", state.Addr)
	}
	move = append(move, plan.To)

	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, config.LocalEnvPrefix+"DATA_DIR=") {
			env = append(env, entry)
		}
	}
	if !m.Supervised || runtime.GOOS != "linux" {
		return executable, move, env, nil
	}
	// A transient unit of its own. See the comment where the daemon builds this: a child
	// of a socket-activated daemon is in pkgcache.service's control group, and the move
	// stops that service.
	run := []string{
		"--user", "--collect", "--quiet",
		"--unit", fmt.Sprintf("pkgcache-migrate-%d", time.Now().Unix()),
	}
	// systemd-run starts the unit with the service manager's environment, not ours. These
	// two decide where the default location is, which is where the signpost goes.
	for _, key := range []string{"HOME", "XDG_DATA_HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			run = append(run, "--setenv="+key+"="+value)
		}
	}
	return "systemd-run", append(append(run, executable), move...), env, nil
}

// launchMove starts the move so that it outlives this daemon.
func launchMove(ctx context.Context, name string, args, env []string, log string) error {
	if name == "systemd-run" {
		// systemd-run returns once the unit has started, so this waits only for that.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("local: hand the move to systemd: %w: %s",
				err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	logFile, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("local: open %s: %w", log, err)
	}
	defer func() { _ = logFile.Close() }()
	// #nosec G204 -- the executable is this program and every argument was resolved above.
	command := exec.Command(name, args...) //nolint:noctx // the move must outlive the request that started it, and this daemon
	command.Stdout = logFile
	command.Stderr = logFile
	command.Env = env
	detach(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("local: start the move: %w", err)
	}
	// Reaped for the reason spawn gives, although here the parent is about to be stopped
	// by its own child and the question rarely arises.
	go func() { _ = command.Wait() }()
	return nil
}

// PlanMigration answers whether a cache can move to the directory somebody chose, and
// changes nothing.
func PlanMigration(from, chosen string) (controlapi.MigrationPlan, error) {
	plan := controlapi.MigrationPlan{FreeBytes: -1, Problems: []string{}}
	problem := func(format string, args ...any) {
		plan.Problems = append(plan.Problems, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(chosen) == "" {
		problem("Choose a directory to move the cache to.")
		return plan, nil
	}
	target, err := migrationTarget(chosen)
	if err != nil {
		problem("%s.", err)
		return plan, nil
	}
	source, destination, err := migratePaths(from, target)
	plan.From, plan.To = filepath.Clean(from), target
	if err != nil {
		problem("%s.", strings.TrimPrefix(err.Error(), "local: "))
		return plan, nil
	}
	plan.From, plan.To = source, destination

	held, err := measure(source)
	if err != nil {
		return plan, err
	}
	plan.Bytes, plan.Files = held.bytes, held.files

	if err := prepareDestination(destination); err != nil {
		problem("%s already holds other files.", destination)
	}
	anchor := existingAncestor(destination)
	if anchor == "" {
		problem("Nothing on the way to %s exists.", destination)
		return plan, nil
	}
	if writable, err := probeDirectory(anchor); err != nil {
		if !writable {
			problem("%s cannot be written to.", anchor)
		} else {
			problem("%s cannot hardlink, which a cache needs. exFAT, FAT32 and most network "+
				"shares behave this way; an ext4, xfs or btrfs disk does not.", anchor)
		}
	}
	if here, there := deviceKey(source), deviceKey(anchor); here != "" && here == there {
		plan.SameDisk = true
	}
	if free, _, err := diskusage.Usage(anchor); err == nil {
		plan.FreeBytes = free
		// A rename needs no room, so a move within one disk is never refused for space.
		if !plan.SameDisk && free < held.bytes {
			problem("That disk has %s free and the cache is %s.", FormatSize(free), FormatSize(held.bytes))
		}
	}
	return plan, nil
}

// migrationTarget is the directory a cache moves into, given the one somebody picked.
//
// A picker is pointed at a disk, not at a directory that does not exist yet: choosing
// /mnt/big means "onto that disk", and /mnt/big has lost+found in it. So an empty choice —
// or one holding nothing but a signpost, which is how moving back to the default location
// looks — is used as it is, and anything else gets a pkgcache directory of its own inside.
func migrationTarget(chosen string) (string, error) {
	target, err := filepath.Abs(chosen)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return target, nil
	case err != nil:
		return "", fmt.Errorf("%s cannot be read: %w", target, err)
	case len(entries) == 0,
		len(entries) == 1 && entries[0].Name() == config.MovedToName:
		return target, nil
	default:
		return filepath.Join(target, "pkgcache"), nil
	}
}

// existingAncestor is the nearest directory at or above path that exists, which is where
// the questions about a destination that is yet to be created have to be asked.
func existingAncestor(path string) string {
	for current := filepath.Clean(path); ; {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// FinishMigration leaves the outcome of a window-started move where the window will read
// it: in the new location when the cache got there, and in the old one when it did not —
// in both cases, the directory the next daemon serves.
func FinishMigration(from, to string, started time.Time, result MigrateResult, moveErr error) error {
	finished := time.Now()
	record := controlapi.MigrationRecord{
		From: filepath.Clean(from), To: filepath.Clean(to),
		Bytes: result.Bytes, Files: result.Files, Renamed: result.Renamed,
		Started: started, Finished: &finished,
	}
	if moveErr != nil {
		record.State = controlapi.MigrationFailed
		// The first line only, without the package prefix. The rest of a terminal error
		// is advice written for a terminal.
		message := strings.TrimPrefix(moveErr.Error(), "local: ")
		record.Error, _, _ = strings.Cut(message, "\n")
		return writeMigrationRecord(from, record)
	}
	record.State = controlapi.MigrationDone
	if result.Kept {
		// The old copy still carries the "moving" note it was started with, and a daemon
		// pointed at it later would report a move that died.
		_ = clearMigrationRecord(from)
	}
	return writeMigrationRecord(result.To, record)
}

func readMigrationRecord(dataDir string) (controlapi.MigrationRecord, bool) {
	data, err := os.ReadFile(migrationRecordPath(dataDir))
	if err != nil {
		return controlapi.MigrationRecord{}, false
	}
	var record controlapi.MigrationRecord
	if err := json.Unmarshal(data, &record); err != nil || record.State == "" {
		return controlapi.MigrationRecord{}, false
	}
	return record, true
}

// writeMigrationRecord replaces the record in one rename, so a window reading it while it
// is written sees the old outcome or the new one and never half of each.
func writeMigrationRecord(dataDir string, record controlapi.MigrationRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dataDir, ".migration-*.json")
	if err != nil {
		return fmt.Errorf("local: record the move: %w", err)
	}
	name := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(name)
		return fmt.Errorf("local: record the move: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("local: record the move: %w", err)
	}
	if err := os.Rename(name, migrationRecordPath(dataDir)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("local: record the move: %w", err)
	}
	return nil
}

func clearMigrationRecord(dataDir string) error {
	if err := os.Remove(migrationRecordPath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
