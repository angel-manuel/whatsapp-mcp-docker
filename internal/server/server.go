// Package server wires the loaded configuration and logger into a runnable
// application. It owns the whatsmeow client and the MCP transport (stdio or
// HTTP/SSE).
package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcptools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// Version is baked into the MCP server identity. main overrides this via
// ldflags at build time.
var Version = "0.0.0-dev"

// Server is the top-level application container.
type Server struct {
	cfg *config.Config
	log *slog.Logger
}

// New constructs a Server from an already-loaded config and logger.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// Run blocks until ctx is cancelled or a subsystem errors fatally. It
// orchestrates startup and graceful shutdown of all owned subsystems.
func (s *Server) Run(ctx context.Context) error {
	log := applog.WithEvent(s.log, "server.start")
	log.Info("server starting",
		slog.String("transport", string(s.cfg.Transport)),
		slog.String("bind_addr", s.cfg.BindAddr),
		slog.Int("port", s.cfg.Port),
		slog.String("data_dir", s.cfg.DataDir),
	)

	// Cache migrations are schema-only; don't let a fast ctx cancel
	// leave us with a half-applied schema. Use a detached background
	// context so Open either succeeds or fails on its own terms.
	// Opened before wa so the ingestor can be wired into the wa
	// EventHook from the very first whatsmeow event.
	cacheStore, err := cache.Open(context.Background(), s.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("cache open: %w", err)
	}
	defer func() {
		if err := cacheStore.Close(); err != nil {
			applog.WithEvent(s.log, "server.stop").Warn("cache close",
				slog.String("err", err.Error()))
		}
	}()

	ingestor := cache.NewIngestor(cacheStore, applog.WithEvent(s.log, "cache.ingest"))
	syncOrch := cache.NewSyncOrchestrator(cacheStore, ingestor, applog.WithEvent(s.log, "cache.sync"))

	// Like cache.Open above, wa session-store bringup runs sqlite migrations
	// that should not be aborted mid-flight by a fast ctx cancel. Detach
	// during Open; runtime cancellation is honored via Close/Disconnect.
	waCli, err := wa.Open(context.Background(), wa.Config{
		DataDir:        s.cfg.DataDir,
		PairDeviceName: s.cfg.PairDeviceName,
		EventHook:      ingestor.HandleEvent,
	})
	if err != nil {
		return fmt.Errorf("wa open: %w", err)
	}
	defer func() {
		if err := waCli.Close(); err != nil {
			applog.WithEvent(s.log, "server.stop").Warn("wa close",
				slog.String("err", err.Error()))
		}
	}()

	mcpSrv, err := s.buildMCP(waCli, cacheStore)
	if err != nil {
		return fmt.Errorf("build mcp server: %w", err)
	}
	if err := tools.Register(mcpSrv.Registry(), tools.Deps{
		Cache:    cacheStore,
		WA:       waCli,
		Ingestor: ingestor,
		Sync:     syncOrch,
	}); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}

	mcpCtx, mcpCancel := context.WithCancel(ctx)
	defer mcpCancel()

	errCh := make(chan error, 1)
	go func() {
		if err := mcpSrv.Run(mcpCtx); err != nil {
			errCh <- fmt.Errorf("mcp: %w", err)
			return
		}
		errCh <- nil
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		runErr = err
	}
	mcpCancel()

	// Drain the MCP goroutine if it hasn't exited yet.
	select {
	case err := <-errCh:
		if runErr == nil && err != nil {
			runErr = err
		}
	default:
	}

	applog.WithEvent(s.log, "server.stop").Info("server stopping")
	return runErr
}

// buildMCP constructs the MCP subsystem, binds its pairing gate to the
// whatsmeow client (tools fail with not_paired until the device is both
// paired AND logged in), and registers the read-side cache-backed tools
// against its registry.
func (s *Server) buildMCP(waCli *wa.Client, cacheStore *cache.Store) (*mcp.Server, error) {
	pairing := mcp.PairingStateFunc(func() bool {
		st := waCli.Status()
		return st.LoggedIn
	})
	reg := mcp.NewRegistry()
	if err := mcptools.Register(reg, cacheStore); err != nil {
		return nil, fmt.Errorf("register cache tools: %w", err)
	}
	return mcp.New(mcp.Config{
		Transport: mcp.TransportMode(s.cfg.Transport),
		BindAddr:  s.cfg.BindAddr,
		Port:      s.cfg.Port,
		AuthToken: s.cfg.AuthToken,
		Name:      "whatsapp-mcp",
		Version:   Version,
	}, s.log, reg, pairing)
}
