package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunSweeper_SweepsAtStartup covers the reason startup is not deferred
// to the first tick: the container may have been down for longer than
// MEDIA_TTL, and expired bytes must not stay readable in the meantime.
func TestRunSweeper_SweepsAtStartup(t *testing.T) {
	s := newTestStore(t, Options{TTL: time.Hour})
	expired, err := s.Put([]byte("stale"), "image/jpeg", "stale.jpg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	backdate(t, s, expired.SHA256, ".jpg", time.Now().Add(-2*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A long interval proves the eviction came from the startup pass
		// rather than from a tick.
		s.RunSweeper(ctx, time.Hour)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(s.Dir(), expired.SHA256+".jpg")); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup sweep did not run within 5s")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeper did not exit on context cancel")
	}
}

// TestRunSweeper_ReturnsWhenRetentionDisabled pins the early return that
// internal/server relies on: with no limits configured there is nothing to
// enforce, and an idle hourly goroutine would be pure noise. It must still
// have made its one startup pass first (orphan cleanup).
func TestRunSweeper_ReturnsWhenRetentionDisabled(t *testing.T) {
	s := newTestStore(t, Options{})
	orphan := filepath.Join(s.Dir(), digestOf([]byte("orphan"))+".bin")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o640); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Never cancelled: the only way this returns is the early exit.
		s.RunSweeper(context.Background(), 0)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeper did not return with retention disabled")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("startup pass did not clean the orphan before returning")
	}
}

// TestRunSweeper_ZeroIntervalDoesNotBusyLoop guards the interval<=0 fallback:
// a zero interval must mean DefaultSweepInterval, not a ticker panic or a
// spin.
func TestRunSweeper_ZeroIntervalDoesNotBusyLoop(t *testing.T) {
	s := newTestStore(t, Options{MaxBytes: 1 << 20})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunSweeper(ctx, 0)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeper did not exit on context cancel")
	}
}
