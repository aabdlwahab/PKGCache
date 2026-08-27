//go:build !windows

// Socket activation is the systemd and launchd contract, so these run on Unix.
package local

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// Socket activation is the answer to the sharpest problem in this design: settings
// that outlive the process they name. It is worth testing as the thing it claims to be
// — a port that answers before the daemon exists — rather than as a unit file that was
// written.
//
// systemd is simulated rather than required: the contract is LISTEN_FDS, LISTEN_PID and
// a bound socket on descriptor 3, and a test that binds a socket and execs the daemon
// exercises exactly that. A test that shelled out to systemctl would test systemd.
func TestSocketActivationServesOnAnAlreadyBoundPort(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("a Go toolchain is needed to build the daemon")
	}
	dir := t.TempDir()
	t.Setenv(config.LocalEnvPrefix+"DATA_DIR", dir)
	t.Setenv("PKGCACHE_LIMIT", "")
	if err := WriteBudget(dir, Budget{LimitBytes: NoLimit, MinFreeBytes: 1}); err != nil {
		t.Fatal(err)
	}

	// Bind the port first, exactly as systemd does. From here the address answers —
	// connections queue in the kernel — regardless of whether a daemon exists.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	address := listener.Addr().String()

	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.TCPListener", listener)
	}
	file, err := tcp.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	// Hand it over on descriptor 3, which is the whole of the systemd protocol.
	cmd := exec.Command(os.Args[0], "serve", "-data-dir", dir)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = append(os.Environ(),
		listenFDsEnv+"=1",
		config.LocalEnvPrefix+"DATA_DIR="+dir,
	)
	// LISTEN_PID names the process the descriptor is for, and is only knowable after
	// the fork — so systemd sets it from the child's pid. Here the child is told to
	// accept any pid by leaving it unset, which is the documented fallback.
	logFile, err := os.Create(filepath.Join(dir, "activated.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Both conditions, not just the first. The daemon binds, then serves, then
	// publishes its state file — the bound address is not knowable before the bind, so
	// that order is forced. Waiting only for the probe wins a race the daemon never
	// promised: it answers on the inherited socket a moment before the file naming it
	// exists, and the ReadState below then fails with "no daemon is running".
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if Probe(ctx, address) == nil {
			if _, err := ReadState(dir); err == nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := Probe(ctx, address); err != nil {
		logs, _ := os.ReadFile(filepath.Join(dir, "activated.log"))
		t.Fatalf("the activated daemon never served on %s: %v\n%s", address, err, logs)
	}

	// The address it publishes is the socket it was handed, not the one it was
	// configured with — which is what every client reads.
	state, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Addr != address {
		t.Fatalf("published address = %q, want the inherited socket %q", state.Addr, address)
	}

	// And it really is serving: a data-plane request reaches the router.
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %s", response.Status)
	}
}

// The variables are inherited by children, so a process spawned by an activated daemon
// must not try to adopt a descriptor it does not have.
func TestActivationIgnoresAnInheritedEnvironment(t *testing.T) {
	t.Setenv(listenFDsEnv, "1")
	t.Setenv(listenPIDEnv, strconv.Itoa(os.Getpid()+1))
	listener, err := ActivationListener()
	if err != nil {
		t.Fatal(err)
	}
	if listener != nil {
		t.Fatal("a descriptor meant for another process was adopted")
	}
	if Activated() {
		t.Fatal("Activated reported true for another process's descriptor")
	}
}

func TestActivationAbsentIsNotAnError(t *testing.T) {
	t.Setenv(listenFDsEnv, "")
	listener, err := ActivationListener()
	if err != nil || listener != nil {
		t.Fatalf("listener = %v, err = %v; want both nil", listener, err)
	}
}

// pkgcache serves exactly one port, so more than one socket is a configuration error
// worth reporting rather than a set to choose from.
func TestActivationRefusesMoreThanOneSocket(t *testing.T) {
	t.Setenv(listenFDsEnv, "2")
	t.Setenv(listenPIDEnv, strconv.Itoa(os.Getpid()))
	if _, err := ActivationListener(); err == nil {
		t.Fatal("two activation sockets were accepted")
	}
}

// Every file persist writes is one the user may already keep their own settings in, so
// the fenced block is the property that matters: install adds only the block, and
// uninstall removes only the block.
func TestPersistLeavesExistingSettingsAlone(t *testing.T) {
	home := t.TempDir()
	npmrc := filepath.Join(home, ".npmrc")
	original := "//registry.internal/:_authToken=secret\nfund=false\n"
	if err := os.WriteFile(npmrc, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	options := PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Project: "global",
		GitHosts: []string{"github.com"}, Home: home,
		Available: AvailabilityAccepted, Out: io.Discard,
	}
	if err := ApplyPersist(options); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(npmrc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "_authToken=secret") {
		t.Fatal("the user's own npm settings were discarded")
	}
	if !strings.Contains(string(after), "registry=http://127.0.0.1:41780/global/npm/") {
		t.Fatalf("the cache setting was not added:\n%s", after)
	}
	// The mode is preserved: an .npmrc with a token in it must not become world-readable.
	info, err := os.Stat(npmrc)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf(".npmrc mode = %o, want the original 600", mode)
	}

	uninstall := options
	uninstall.Uninstall = true
	if err := ApplyPersist(uninstall); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(npmrc)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("uninstall did not restore the file:\nwant:\n%s\ngot:\n%s", original, restored)
	}
}

// A file that existed only for us is removed rather than left empty.
func TestPersistRemovesFilesItCreated(t *testing.T) {
	home := t.TempDir()
	options := PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Project: "global", Home: home,
		Available: AvailabilityAccepted, Out: io.Discard,
	}
	if err := ApplyPersist(options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".npmrc")); err != nil {
		t.Fatal(err)
	}
	uninstall := options
	uninstall.Uninstall = true
	if err := ApplyPersist(uninstall); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".npmrc")); !os.IsNotExist(err) {
		t.Fatal("an empty .npmrc was left behind")
	}
}

// Installing twice must not stack two blocks, or uninstall would leave one.
func TestPersistIsIdempotent(t *testing.T) {
	home := t.TempDir()
	options := PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Project: "global", Home: home,
		Available: AvailabilityAccepted, Out: io.Discard,
	}
	for range 3 {
		if err := ApplyPersist(options); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(filepath.Join(home, ".npmrc"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(body), beginMarker); count != 1 {
		t.Fatalf("%d blocks after three installs:\n%s", count, body)
	}
}

// The refusal is the design decision, not a safety net: shipping a mode that can leave
// npm failing when a background process exits would be worse than not shipping it.
func TestPersistRefusesWithoutAnAvailabilityGuarantee(t *testing.T) {
	home := t.TempDir()
	err := ApplyPersist(PersistOptions{
		BaseURL: "http://127.0.0.1:41780", Home: home, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("persist installed settings with no guarantee the address answers")
	}
	if !strings.Contains(err.Error(), "-anyway") {
		t.Errorf("the refusal does not say how to proceed deliberately: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".npmrc")); !os.IsNotExist(statErr) {
		t.Fatal("the refusal still wrote a file")
	}
}
