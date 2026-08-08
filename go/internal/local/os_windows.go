//go:build windows

package local

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code a running process reports. x/sys/windows does not
// export it, and the value is fixed by the API contract (STATUS_PENDING).
const stillActive = 259

// The Windows half of the lifecycle. Written to the same contract as the Unix half and
// NOT verified on a Windows host — see docs/local-cache-plan.md, which records exactly
// which platforms were run and which were only compiled.

func lockFile(file *os.File, wait bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	overlapped := new(windows.Overlapped)
	// A whole-file lock, expressed as "the largest range there is", which is how
	// LockFileEx is asked for one.
	err := windows.LockFileEx(
		windows.Handle(file.Fd()), flags, 0, ^uint32(0), ^uint32(0), overlapped)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

func unlockFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()), 0, ^uint32(0), ^uint32(0), overlapped)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}

func detach(cmd *exec.Cmd) {
	// syscall's SysProcAttr, not x/sys/windows's: os/exec takes the former, and the two
	// are different types with the same shape.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// terminate has no graceful form here worth relying on. Windows has no SIGTERM, and
// GenerateConsoleCtrlEvent does not reach a process that was deliberately detached from
// any console — which this one is, so that it survives the shell that started it.
//
// The cost is bounded by an ordering the store already guarantees: a blob is durable
// before the catalog row that references it, so a killed daemon loses at worst some
// recently batched rows, and a lost row costs a re-fetch rather than corruption.
func terminate(pid int) error {
	return kill(pid)
}

func kill(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windows.TerminateProcess(handle, 1)
}
