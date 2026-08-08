package local

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
)

// The lifecycle is processes, file locks and sockets, so the tests use real ones. The
// test binary doubles as the daemon: `spawn` re-executes its own executable with a
// `serve` argument, and TestMain answers to that before the testing package ever sees
// the arguments. That buys real flock semantics and real process death — the two things
// a fake would model wrongly and exactly where this code can be subtly broken.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(serveForTest(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func serveForTest(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", "", "")
	addr := fs.String("addr", "", "")
	idle := fs.Duration("idle-timeout", 0, "")
	offline := fs.Bool("offline", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	snap, err := config.LoadLocal(config.LocalFlags{
		DataDir: *dataDir, Addr: *addr, IdleTimeout: idle, Offline: offline,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, RunOptions{Snapshot: snap, Notes: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// testSnapshot builds a profile pointed at a throwaway cache on an ephemeral port, so
// tests never collide with each other or with a developer's own daemon.
func testSnapshot(t *testing.T, idle time.Duration) *config.Snapshot {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.LocalEnvPrefix+"DATA_DIR", dir)
	t.Setenv("PKGCACHE_LIMIT", "")
	// Every cache needs a budget before it serves; these tests are about the lifecycle,
	// so they choose the one that never interferes with it.
	if err := WriteBudget(dir, Budget{LimitBytes: NoLimit, MinFreeBytes: 1}); err != nil {
		t.Fatal(err)
	}
	flags := config.LocalFlags{DataDir: dir, Addr: "0"}
	if idle > 0 {
		flags.IdleTimeout = &idle
	}
	snap, err := config.LoadLocal(flags)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func stopDaemon(t *testing.T, dataDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := Stop(ctx, dataDir, 15*time.Second); err != nil {
		t.Errorf("stopping the daemon: %v", err)
	}
}

// The Makefile case: many commands start at once, none of them find a cache, and every
// one of them tries to start it. Without the start lock this spawns one daemon per
// caller, all but one of which pay for two database opens before failing on the
// directory lock.
func TestConcurrentEnsureStartsExactlyOneDaemon(t *testing.T) {
	snap := testSnapshot(t, 0)
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })

	const callers = 20
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	states := make([]State, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i], errs[i] = Ensure(ctx, EnsureOptions{
				Snapshot: snap, Executable: os.Args[0],
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	first := states[0]
	if first.PID == 0 || first.Addr == "" {
		t.Fatalf("no daemon was published: %+v", first)
	}
	for i, state := range states {
		if state.PID != first.PID || state.Addr != first.Addr {
			t.Fatalf("caller %d reached a different daemon: %+v, want %+v",
				i, state, first)
		}
	}
	if !first.Alive(ctx) {
		t.Fatal("the daemon every caller agreed on is not answering")
	}
}

// A daemon that is SIGKILLed, or whose machine loses power, leaves its state file
// behind. Recovering from that must not need the user to know the file exists.
func TestKilledDaemonIsReplacedWithoutOperatorAction(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, first)

	// The state file is still there, naming a dead process. Nothing has been cleaned up.
	if _, err := os.Stat(StatePath(snap.DataDir)); err != nil {
		t.Fatalf("expected the stale state file to survive the kill: %v", err)
	}
	stale, err := ReadState(snap.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Alive(ctx) {
		t.Fatal("a killed daemon still reports as alive")
	}

	second, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatalf("Ensure did not recover from a stale state file: %v", err)
	}
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })
	if second.PID == first.PID {
		t.Fatal("Ensure returned the dead daemon")
	}
	if !second.Alive(ctx) {
		t.Fatal("the replacement daemon is not answering")
	}
}

// The kernel drops a flock when the process holding it dies, which is why the lock is
// a lock rather than a pid file: there is nothing left behind for a user to delete.
func TestLockIsReleasedWhenTheHolderDies(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(LockPath(snap.DataDir), false); !errors.Is(err, ErrLocked) {
		t.Fatalf("the running daemon's lock was not exclusive: %v", err)
	}
	if err := syscall.Kill(state.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, state)

	lock, err := Acquire(LockPath(snap.DataDir), false)
	if err != nil {
		t.Fatalf("the lock outlived the process that held it: %v", err)
	}
	_ = lock.Close()
}

// Idle exit is what makes an on-demand daemon reasonable on a laptop, and restarting
// after it is what makes it invisible.
//
// Waiting on the state file rather than on Alive, because Alive probes: polling it
// would be the thing keeping the daemon awake if probes counted as use, and this test
// would then pass for the wrong reason on the day that regressed.
func TestIdleExitThenRestart(t *testing.T) {
	snap := testSnapshot(t, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// A clean exit removes its own state file, unlike the killed case above.
		if _, err := os.Stat(StatePath(snap.DataDir)); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(StatePath(snap.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the daemon did not exit after its idle timeout: %v", err)
	}
	if first.Alive(ctx) {
		t.Fatal("the daemon is still answering after it published its exit")
	}

	second, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatalf("could not restart after an idle exit: %v", err)
	}
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })
	if second.PID == first.PID || !second.Alive(ctx) {
		t.Fatal("the restart did not produce a new, live daemon")
	}
}

// Traffic has to hold the daemon open, or a long build would lose its cache halfway
// through. Real traffic, not a liveness probe: watching the cache must not be
// indistinguishable from using it, or nothing that is monitored ever idles out.
func TestActivityDefersIdleExit(t *testing.T) {
	snap := testSnapshot(t, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })

	// Longer than the idle timeout, kept alive only by requests.
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		if err := useCache(ctx, state); err != nil {
			t.Fatalf("the daemon exited while it was still being used: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !state.Alive(ctx) {
		t.Fatal("the daemon exited despite continuous use")
	}
}

func TestStopIsCleanAndIdempotent(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	state, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := Stop(ctx, snap.DataDir, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("Stop reported nothing was running")
	}
	if state.Alive(ctx) {
		t.Fatal("Stop returned before the daemon was gone")
	}
	if _, err := os.Stat(StatePath(snap.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stop left the state file behind: %v", err)
	}

	stopped, err = Stop(ctx, snap.DataDir, 5*time.Second)
	if err != nil {
		t.Fatalf("stopping an already-stopped cache is an error: %v", err)
	}
	if stopped {
		t.Fatal("Stop claimed to stop a daemon that was not running")
	}
}

// Asking whether the cache is running must not be the thing that starts it.
func TestNoStartDoesNotStart(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Ensure(ctx, EnsureOptions{
		Snapshot: snap, Executable: os.Args[0], NoStart: true,
	})
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v, want ErrNoDaemon", err)
	}
	if _, err := os.Stat(StatePath(snap.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a status check started a daemon")
	}
}

// An upgraded binary talking to a daemon from the previous version is the one way this
// design could serve yesterday's behaviour indefinitely, because nothing else restarts
// it.
func TestVersionMismatchReplacesTheDaemon(t *testing.T) {
	snap := testSnapshot(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := Ensure(ctx, EnsureOptions{Snapshot: snap, Executable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the published version as though the daemon were an older build.
	stale := first
	stale.Version = "0.0.1-previous"
	if err := WriteState(snap.DataDir, stale); err != nil {
		t.Fatal(err)
	}

	var notes strings.Builder
	second, err := Ensure(ctx, EnsureOptions{
		Snapshot: snap, Executable: os.Args[0], Notes: &notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopDaemon(t, snap.DataDir) })
	if second.PID == first.PID {
		t.Fatal("a daemon from another version was reused")
	}
	if !strings.Contains(notes.String(), "0.0.1-previous") {
		t.Errorf("the replacement was not explained: %q", notes.String())
	}
}

// A busy fixed port must not be fatal, and must not be silent either: settings written
// by `pkgcache persist` name the fixed port and cannot follow a daemon that moved.
func TestPortFallbackIsAnnounced(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	taken := occupied.Addr().String()

	dir := t.TempDir()
	t.Setenv(config.LocalEnvPrefix+"DATA_DIR", dir)
	snap, err := config.LoadLocal(config.LocalFlags{DataDir: dir, Addr: taken})
	if err != nil {
		t.Fatal(err)
	}
	var notes strings.Builder
	if err := resolveAddr(snap, &notes); err != nil {
		t.Fatal(err)
	}
	if snap.LocalAddr() == taken {
		t.Fatal("resolveAddr kept a port that is already in use")
	}
	if !strings.Contains(notes.String(), "in use") ||
		!strings.Contains(notes.String(), "persist") {
		t.Errorf("the fallback did not warn about persistent settings: %q", notes.String())
	}
	// Whatever it chose must still satisfy the loopback invariant.
	if err := snap.Validate(); err != nil {
		t.Fatalf("the fallback address is not valid: %v", err)
	}
}

// A malformed state file means a process died between creating and writing it. The
// useful response is to start a daemon, not to make somebody delete a file.
func TestMalformedStateReadsAsNoDaemon(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"truncated": "{",
		"empty":     "",
		"no addr":   `{"pid": 1234}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(StatePath(dir), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadState(dir); !errors.Is(err, ErrNoDaemon) {
				t.Fatalf("err = %v, want ErrNoDaemon", err)
			}
		})
	}
}

func TestStateIsWrittenAtomicallyAndPrivately(t *testing.T) {
	dir := t.TempDir()
	want := State{PID: 42, Addr: "127.0.0.1:41780", Version: "test", Started: time.Now()}
	if err := WriteState(dir, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("state file mode = %o, want 600", mode)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != want.PID || got.Addr != want.Addr || got.Version != want.Version {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// No temporary files are left where a later reader could mistake one for state.
	entries, err := filepath.Glob(filepath.Join(dir, ".daemon-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("write left temporary files behind: %v", entries)
	}
}

// A port answering is not proof it is answering for us.
func TestProbeRejectsSomethingElseOnThePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			body, _ := json.Marshal(map[string]string{"status": "ok"})
			fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n"+
				"Content-Type: application/json\r\n\r\n%s", len(body), body)
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Probe(ctx, listener.Addr().String()); err == nil {
		t.Fatal("Probe accepted a program that is not a package cache")
	}
	state := State{PID: os.Getpid(), Addr: listener.Addr().String()}
	if state.Alive(ctx) {
		t.Fatal("Alive accepted a live pid answering on a port it does not own")
	}
}

// waitGone waits for a daemon to stop serving.
//
// Deliberately not a pid check. These tests are the daemon's parent and never reap it,
// so an exited daemon lingers as a zombie whose pid still answers a signal — which is
// precisely the condition Stop and Alive have to see through, and testing it with the
// check it is meant to replace would prove nothing.
// useCache makes a request the cache treats as use rather than as observation.
// An unknown project 404s, which is fine: it reached the data plane, which is the point.
func useCache(ctx context.Context, state State) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, state.BaseURL()+"/global/npm/", nil)
	if err != nil {
		return err
	}
	response, err := probeClient.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Body.Close()
}

func waitGone(t *testing.T, state State) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !state.Alive(ctx) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the daemon on %s did not stop", state.Addr)
}
