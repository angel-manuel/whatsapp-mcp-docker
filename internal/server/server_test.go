package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
)

func newTestServer(w io.Writer) *Server {
	cfg := &config.Config{
		Transport: config.TransportStdio,
		BindAddr:  "127.0.0.1",
		Port:      8081,
		AdminPort: 8082,
		DataDir:   "/tmp/data",
		LogLevel:  "info",
		LogFormat: "json",
	}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(cfg, logger)
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	srv := newTestServer(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_LogsStartAndStopEvents(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServer(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Give Run a tick to emit the start line, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
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
