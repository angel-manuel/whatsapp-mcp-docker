//go:build unix

package wa

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLockHeld is returned when the data-dir lockfile is already held by
// another process.
var ErrLockHeld = errors.New("data dir lockfile is already held")

type lockfile struct {
	f *os.File
}

func acquireLock(path string) (*lockfile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return &lockfile{f: f}, nil
}

func (l *lockfile) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	ferr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if ferr != nil {
		return ferr
	}
	return cerr
}

// Lock is an exclusive advisory lock on a file, held until Release.
type Lock struct{ l *lockfile }

// AcquireLock takes the same exclusive, non-blocking flock that Open takes
// on {DataDir}/.lock, for callers guarding a directory above any single
// Client (the multi-session data root). It fails with ErrLockHeld when
// another process holds it.
func AcquireLock(path string) (*Lock, error) {
	l, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	return &Lock{l: l}, nil
}

// Release drops the lock. Safe to call on a nil Lock.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return l.l.release()
}
