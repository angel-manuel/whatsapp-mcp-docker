package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
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
		Transport: config.TransportStdio,
		BindAddr:  "127.0.0.1",
		Port:      pickPort(t),
		DataDir:   t.TempDir(),
		LogLevel:  "info",
		LogFormat: "json",
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
	case <-done:
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
