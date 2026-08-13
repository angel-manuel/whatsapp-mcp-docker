// Package server wires the loaded configuration and logger into a runnable
// application. It owns the whatsmeow client and the MCP transport (stdio or
// HTTP/SSE).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcptools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// shutdownTimeout bounds how long we wait for the MCP goroutine to exit after
// context cancellation so we don't block indefinitely on a slow consumer.
const shutdownTimeout = 10 * time.Second

// sweeperDrainTimeout bounds how long shutdown waits for the media
// retention loop. It only ever blocks on a ticker, so this is a backstop
// rather than a budget.
const sweeperDrainTimeout = 2 * time.Second

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

	// Poll votes arrive encrypted against the poll creation message, so the
	// ingestor needs a way back into whatsmeow to read them. It is installed
	// here rather than passed to NewIngestor because the client above needs
	// the ingestor's HandleEvent as its own event hook.
	ingestor.SetPollDecrypter(waCli)

	// Media blobs downloaded by download_media and served by the
	// GET /media/{sha256} byte route. Opened before the MCP server so its
	// handler can be mounted on the same mux, behind the same bearer auth.
	mediaStore, err := media.Open(s.cfg.MediaDir(), media.Options{
		MaxBytes: s.cfg.MediaMaxBytes,
		TTL:      s.cfg.MediaTTL,
		Logger:   s.log,
	})
	if err != nil {
		return fmt.Errorf("media open: %w", err)
	}

	mcpSrv, err := s.buildMCP(waCli, cacheStore, mediaStore)
	if err != nil {
		return fmt.Errorf("build mcp server: %w", err)
	}
	if err := tools.Register(mcpSrv.Registry(), tools.Deps{
		Cache:    cacheStore,
		WA:       waCli,
		Ingestor: ingestor,
		Sync:     syncOrch,
		Media:    mediaStore,
	}); err != nil {
		return fmt.Errorf("register tools: %w", err)
	}

	mcpCtx, mcpCancel := context.WithCancel(ctx)
	defer mcpCancel()

	// Media retention: one pass now (the container may have been down for
	// longer than MEDIA_TTL, and expired bytes must not stay readable),
	// then every MEDIA_SWEEP_INTERVAL until shutdown.
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		mediaStore.RunSweeper(mcpCtx, s.cfg.MediaSweepInterval)
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := mcpSrv.Run(mcpCtx); err != nil {
			errCh <- fmt.Errorf("mcp: %w", err)
			return
		}
		errCh <- nil
	}()

	var runErr error
	// Tracks whether the select below already took the MCP goroutine's result.
	// errCh carries exactly one value, so draining again would block for the
	// full shutdownTimeout — which is what happens whenever the transport
	// exits on its own (stdio client disconnects, stdin at EOF).
	mcpExited := false
	select {
	case <-ctx.Done():
	case err := <-errCh:
		runErr = err
		mcpExited = true
	}
	mcpCancel()

	// Wait for the MCP goroutine to exit so callers observe a clean shutdown.
	if !mcpExited {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer drainCancel()
		select {
		case err := <-errCh:
			if runErr == nil && err != nil {
				runErr = err
			}
		case <-drainCtx.Done():
			applog.WithEvent(s.log, "server.stop").Warn("mcp goroutine drain timed out")
		}
	}

	// The sweeper only ever waits on a ticker, so it exits promptly once
	// mcpCtx is cancelled. Bound the wait anyway, and keep it short: a
	// stuck retention pass must not hold up process exit.
	sweeperDrain := time.NewTimer(sweeperDrainTimeout)
	defer sweeperDrain.Stop()
	select {
	case <-sweeperDone:
	case <-sweeperDrain.C:
		applog.WithEvent(s.log, "server.stop").Warn("media sweeper drain timed out")
	}

	applog.WithEvent(s.log, "server.stop").Info("server stopping")
	return runErr
}

// buildMCP constructs the MCP subsystem, binds its pairing gate to the
// whatsmeow client (tools fail with not_paired until the device is both
// paired AND logged in), and registers the read-side cache-backed tools
// against its registry.
func (s *Server) buildMCP(waCli *wa.Client, cacheStore *cache.Store, mediaStore *media.Store) (*mcp.Server, error) {
	pairing := mcp.PairingStateFunc(func() bool {
		st := waCli.Status()
		return st.LoggedIn
	})
	reg := mcp.NewRegistry()
	if err := mcptools.Register(reg, cacheStore); err != nil {
		return nil, fmt.Errorf("register cache tools: %w", err)
	}
	// The media byte route rides the MCP listener: same port, same
	// AUTH_TOKEN, same bearer middleware. It exists for the one thing MCP
	// cannot do — transfer bytes — and deliberately does not reopen the
	// :8082 admin API removed in 99b0ce7.
	routes := map[string]http.Handler{
		media.RoutePrefix + "{sha256}": mediaStore.Handler(),
	}
	return mcp.New(mcp.Config{
		Transport: mcp.TransportMode(s.cfg.Transport),
		BindAddr:  s.cfg.BindAddr,
		Port:      s.cfg.Port,
		AuthToken: s.cfg.AuthToken,
		Name:      "whatsapp-mcp",
		Version:   Version,
		Routes:    routes,
	}, s.log, reg, pairing)
}
