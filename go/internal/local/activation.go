package local

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// Socket activation is what makes persistent client settings safe.
//
// The tension is real and it is the sharpest design problem in pkgcache. `pkgcache
// persist` writes a .npmrc and a pip.conf naming a fixed port, and those files outlive
// every process — but the daemon is designed to exit when nobody is using it. A
// .npmrc pointing at a port nothing is listening on is worse than no .npmrc at all:
// `npm install` fails with ECONNREFUSED instead of merely running slowly.
//
// Socket activation resolves it rather than trading one failure for another. systemd
// (or launchd) binds the port itself and holds it open forever; the first connection
// starts the daemon and hands it the already-bound descriptor; the daemon still exits
// when idle, and the port stays listenable throughout. Persistent settings become
// correct by construction instead of by keeping a process alive.
//
// Where activation is unavailable, persist falls back to a resident daemon — see
// InstallService — because the alternative is shipping a mode that can leave the
// machine broken when a background process exits.

const (
	listenFDsEnv = "LISTEN_FDS"
	listenPIDEnv = "LISTEN_PID"
	// listenFDStart is the first descriptor systemd passes, by convention: 0, 1 and 2
	// are still stdin, stdout and stderr.
	listenFDStart = 3
)

// ActivationListener returns the socket this process was activated on, or nil if it
// was started normally.
//
// LISTEN_PID is checked because the variables are inherited by children: without it, a
// process spawned by an activated daemon would try to adopt a descriptor it does not
// have.
func ActivationListener() (net.Listener, error) {
	count := os.Getenv(listenFDsEnv)
	if count == "" {
		return nil, nil
	}
	if pid := os.Getenv(listenPIDEnv); pid != "" {
		wanted, err := strconv.Atoi(pid)
		if err != nil || wanted != os.Getpid() {
			// Inherited from a parent that was activated. Not ours.
			return nil, nil
		}
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return nil, fmt.Errorf("local: %s=%q is not a descriptor count", listenFDsEnv, count)
	}
	if n > 1 {
		return nil, fmt.Errorf(
			"local: activated with %d sockets; pkgcache serves exactly one port", n)
	}
	// The descriptor is inheritable so that it survives exec, which is precisely why it
	// must not leak into any child this process starts.
	syscall.CloseOnExec(listenFDStart)
	file := os.NewFile(uintptr(listenFDStart), "pkgcache-activation")
	if file == nil {
		return nil, fmt.Errorf("local: activation descriptor %d is not open", listenFDStart)
	}
	listener, err := net.FileListener(file)
	// FileListener duplicates the descriptor, so this one is no longer needed and
	// holding it would keep the socket open past shutdown.
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("local: adopt the activation socket: %w", err)
	}
	return listener, nil
}

// Activated reports whether this process was started by socket activation.
func Activated() bool {
	if os.Getenv(listenFDsEnv) == "" {
		return false
	}
	if pid := os.Getenv(listenPIDEnv); pid != "" {
		wanted, err := strconv.Atoi(pid)
		return err == nil && wanted == os.Getpid()
	}
	return true
}
