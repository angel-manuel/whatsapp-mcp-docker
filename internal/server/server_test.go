package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
)

// TestMain pins os.Stdin to a pipe nobody ever writes to. The stdio transport
// reads os.Stdin, so without this these tests depend on what the environment
// hands the test binary: a terminal blocks (transport lives until cancel),
// while CI's closed stdin hits EOF immediately (transport exits on its own).
// Pinning it keeps the cancellation tests below testing cancellation; the one
// test that cares about the EOF path opts into it via withEOFStdin.
func TestMain(m *testing.M) {
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "os.Pipe: %v\n", err)
		os.Exit(1)
	}
	orig := os.Stdin
	os.Stdin = r

	code := m.Run()

	os.Stdin = orig
	// Keeps the write end alive for the whole run: an unreferenced *os.File
	// gets closed by its finalizer, which would turn the pipe into an EOF.
	_ = w.Close()
	_ = r.Close()
	os.Exit(code)
}

// withEOFStdin points os.Stdin at an already-closed pipe so the stdio
// transport reads EOF and returns on its own, the way it does when a stdio
// client disconnects. Safe to mutate the global because tests run
// sequentially and every Run started by an earlier test has returned.
func withEOFStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

func pickPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickPort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestServer(t *testing.T, w io.Writer) *Server {
	t.Helper()
	cfg := &config.Config{
		Transport:    config.TransportStdio,
		BindAddr:     "127.0.0.1",
		Port:         pickPort(t),
		DataDir:      t.TempDir(),
		LogLevel:     "info",
		LogFormat:    "json",
		StdioSession: "default",
		MaxSessions:  1,
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(cfg, logger)
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	srv := newTestServer(t, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// The stdio transport ending is a normal shutdown signal, not something to
// wait out: Run must return as soon as the MCP goroutine reports it, rather
// than sitting on the drain path for shutdownTimeout.
func TestRun_ReturnsWhenTransportExits(t *testing.T) {
	withEOFStdin(t)
	srv := newTestServer(t, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the stdio transport exited")
	}
}

func TestRun_LogsStartAndStopEvents(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServer(t, &buf)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	out := buf.String()
	if !strings.Contains(out, "event_type=server.start") {
		t.Errorf("missing start event in log output: %q", out)
	}
	if !strings.Contains(out, "event_type=server.stop") {
		t.Errorf("missing stop event in log output: %q", out)
	}
}

// Two processes must never share a data root: the registry and the
// sessions/ tree are shared state, and each session's own lock only
// covers its own directory.
func TestRun_HTTPRefusesSecondProcessOnSameDataDir(t *testing.T) {
	dataDir := t.TempDir()
	mk := func(port int) *Server {
		return New(&config.Config{
			Transport:   config.TransportHTTP,
			BindAddr:    "127.0.0.1",
			Port:        port,
			DataDir:     dataDir,
			MaxSessions: 1,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	port := pickPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mk(port).Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("first Run returned %v after cancel, want nil", err)
		}
	}()

	// Wait until the first server is serving, i.e. holds the lock.
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	err := mk(pickPort(t)).Run(ctx2)
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("second Run: err = %v, want a lock error", err)
	}
}

// A 0.7 volume is never served silently: startup must say how to migrate.
func TestRun_WarnsAboutLegacyLayout(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServer(t, &buf)
	if err := os.WriteFile(filepath.Join(srv.cfg.DataDir, "session.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v after cancel, want nil", err)
	}
	if out := buf.String(); !strings.Contains(out, "server.legacy_layout") || !strings.Contains(out, "POST /sessions") {
		t.Fatalf("no migration warning in log:\n%s", out)
	}
}
