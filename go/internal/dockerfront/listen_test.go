// A socket file's mode and its half-close are Unix semantics; the front serves Unix daemons.

//go:build !windows

package dockerfront

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socketPath is a socket path short enough for macOS, whose limit of 104 bytes a
// t.TempDir() under /var/folders can exceed on its own.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "front")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "docker.sock")
}

// The socket is the daemon's API, root on the machine to whoever can open it, so it has
// the daemon socket's own mode rather than whatever the umask leaves.
func TestSocketIsReadableOnlyByOwnerAndGroup(t *testing.T) {
	path := socketPath(t)
	listener, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o660 {
		t.Errorf("mode %o, want 660", mode)
	}
}

// A front that was killed leaves its socket file behind. A service restarted after that
// has to start, and one started twice must not take the socket from the first.
func TestStaleSocketIsReplacedAndALiveOneIsNot(t *testing.T) {
	path := socketPath(t)
	first, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(context.Background(), path); err == nil ||
		!strings.Contains(err.Error(), "already answering") {
		t.Errorf("a second front took a live socket: %v", err)
	}

	// Closing a unix listener removes its file; a crash does not. Leave one behind.
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = first.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no stale socket to test with: %v", err)
	}
	second, err := Listen(context.Background(), path)
	if err != nil {
		t.Fatalf("a stale socket kept the front from starting: %v", err)
	}
	_ = second.Close()
}

func TestAFileThatIsNotASocketIsLeftAlone(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("somebody's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(context.Background(), path); err == nil {
		t.Fatal("listened over a regular file")
	}
	if content, _ := os.ReadFile(path); string(content) != "somebody's file" {
		t.Error("the file was replaced")
	}
}

// The front over a real unix socket, end to end, with the daemon on one too: the shape it
// runs in, where a half-close is a unix one.
func TestFrontAnswersOverUnixSockets(t *testing.T) {
	daemonPath := socketPath(t)
	daemonListener, err := Listen(context.Background(), daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{}
	daemonServer := &http.Server{Handler: http.HandlerFunc(d.serve)}
	go func() { _ = daemonServer.Serve(daemonListener) }()
	defer daemonServer.Close()

	dial, err := Dialer("unix://" + daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	frontPath := socketPath(t)
	frontListener, err := Listen(context.Background(), frontPath)
	if err != nil {
		t.Fatal(err)
	}
	frontServer := &http.Server{Handler: New(Options{Registry: cache, Project: "ci", Dial: dial})}
	go func() { _ = frontServer.Serve(frontListener) }()
	defer frontServer.Close()

	conn, err := net.Dial("unix", frontPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST /v1.43/containers/job/attach?stdin=1 HTTP/1.1\r\n"+
		"Host: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\nexit 0")
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(conn)
	if !strings.HasSuffix(string(answer), "got: exit 0") {
		t.Errorf("answer %q", answer)
	}
}

func TestDialerReadsDockerHostTheWayDockerDoes(t *testing.T) {
	for host, ok := range map[string]bool{
		"unix:///var/run/docker.sock": true,
		"/var/run/docker.sock":        true,
		"tcp://127.0.0.1:2375":        true,
		"ssh://builder":               false,
		"npipe:////./pipe/docker":     false,
		"unix://":                     false,
	} {
		if _, err := Dialer(host); (err == nil) != ok {
			t.Errorf("Dialer(%q): %v", host, err)
		}
	}
	if SocketPath("unix:///run/docker.sock") != "/run/docker.sock" || SocketPath("tcp://h:1") != "" {
		t.Error("SocketPath")
	}
}
