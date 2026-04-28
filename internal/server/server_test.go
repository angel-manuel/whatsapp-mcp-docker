package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
)

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
