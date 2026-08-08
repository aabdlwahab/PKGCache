//go:build !windows

package local

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func lockFile(file *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(file.Fd()), how)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrLocked
		case errors.Is(err, syscall.EINTR):
			// A blocking flock is interruptible by any signal the runtime delivers,
			// including the ones Go uses for preemption. Retrying is correct: the
			// caller asked to wait.
			continue
		default:
			return err
		}
	}
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

// processAlive reports whether a pid names a process this user could signal.
//
// EPERM — the pid exists but belongs to somebody else — counts as *not* ours. On a
// shared machine the alternative is treating another user's unrelated process as our
// daemon and refusing to start, forever, with no way for the user to tell why.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// detach puts the daemon in its own session so it survives the terminal that started
// it. Without this, closing the shell that ran `pkgcache run` delivers SIGHUP to the
// cache and every later command starts a fresh one.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminate asks a daemon to shut down cleanly, so in-flight downloads finish and the
// catalog's batched writes reach disk.
func terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// kill stops a daemon that did not honour terminate.
func kill(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
