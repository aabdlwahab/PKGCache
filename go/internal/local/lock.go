package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked reports that another process holds the lock.
var ErrLocked = errors.New("local: lock is held by another process")

// Lock is an advisory lock on a file, released by Close or by the process exiting.
//
// Exiting is the important half. A pkgcache daemon that is SIGKILLed, or whose machine
// loses power, leaves no lock behind for a user to discover and delete — the kernel
// drops it with the file descriptor. That is why this is a file lock rather than a pid
// file with a "is that pid alive?" dance, which cannot distinguish a dead daemon from
// a live one whose pid was reused.
type Lock struct {
	file *os.File
}

// Acquire takes the lock at path. When wait is false it returns ErrLocked immediately
// if another process holds it; when true it blocks until the lock is free or the
// process is interrupted.
func Acquire(path string, wait bool) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("local: open lock %s: %w", path, err)
	}
	if err := lockFile(file, wait); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

// waitForLock takes the lock at path, trying again for up to wait while another process
// holds it, and returns ErrLocked once the wait is over.
//
// For a holder that is on its way out rather than one that is staying: a daemon that has
// stopped answering still holds its lock until its databases are closed, and both
// everything that stops a cache and everything that starts one decide it is gone at the
// first of those, not the second.
func waitForLock(ctx context.Context, path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := Acquire(path, false)
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close releases the lock.
//
// The lock file itself is left in place. Removing it would race: another process may
// already have opened it and be waiting on it, and unlinking the path it is waiting on
// would let a third process create a new file and take a lock that does not exclude
// anybody.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	l.file = nil
	return err
}
