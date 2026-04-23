//go:build unix

package wa

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockfileExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")

	first, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.release() })

	if _, err := acquireLock(path); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second acquire: want ErrLockHeld, got %v", err)
	}

	if err := first.release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	third, err := acquireLock(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := third.release(); err != nil {
		t.Fatalf("final release: %v", err)
	}
}
