// Package server wires the loaded configuration and logger into a runnable
// application. It owns the whatsmeow client, the admin HTTP listener, and
// the MCP transport (stdio or HTTP/SSE).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/admin"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcptools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// shutdownTimeout bounds graceful HTTP server shutdown so we exit promptly
// even if a slow SSE consumer ignores the context cancellation.
const shutdownTimeout = 10 * time.Second

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
		slog.Int("admin_port", s.cfg.AdminPort),
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

	adminAddr := s.resolveAdminAddr()
	adminSrv := admin.New(admin.Config{
		BindAddr:    adminAddr.host,
		Port:        adminAddr.port,
		AuthToken:   s.cfg.AuthToken,
		RequireAuth: s.cfg.Transport == config.TransportHTTP,
	}, s.log, waCli)

	httpSrv := &http.Server{
		Addr:              net.JoinHostPort(adminAddr.host, strconv.Itoa(adminAddr.port)),
		Handler:           adminSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("admin listen %s: %w", httpSrv.Addr, err)
	}

	applog.WithEvent(s.log, "admin.listen").Info("admin http listening",
		slog.String("addr", httpSrv.Addr))

	errCh := make(chan error, 2)
	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin http: %w", err)
			return
		}
		errCh <- nil
	}()

	mcpSrv, err := s.buildMCP(waCli, cacheStore)
	if err != nil {
		// Shut the admin listener down before bailing.
		_ = httpSrv.Shutdown(context.Background())
		return fmt.Errorf("build mcp server: %w", err)
	}
	if err := tools.Register(mcpSrv.Registry(), tools.Deps{
		Cache:    cacheStore,
		WA:       waCli,
		Ingestor: ingestor,
	}); err != nil {
		_ = httpSrv.Shutdown(context.Background())
		return fmt.Errorf("register tools: %w", err)
	}
	mcpCtx, mcpCancel := context.WithCancel(ctx)
	defer mcpCancel()
	go func() {
		if err := mcpSrv.Run(mcpCtx); err != nil {
			errCh <- fmt.Errorf("mcp: %w", err)
			return
		}
		errCh <- nil
	}()

	// Two producers on errCh: the admin HTTP goroutine and the MCP
	// goroutine. We wait until both have signalled before returning so
	// callers cannot observe partial shutdown (important for tests that
	// inspect log sinks).
	var runErr error
	pending := 2

	select {
	case <-ctx.Done():
	case err := <-errCh:
		pending--
		if err != nil {
			runErr = err
		}
	}
	mcpCancel()

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		applog.WithEvent(s.log, "admin.shutdown").Warn("graceful shutdown failed",
			slog.String("err", err.Error()))
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer drainCancel()
	for pending > 0 {
		select {
		case err := <-errCh:
			pending--
			if runErr == nil && err != nil {
				runErr = err
			}
		case <-drainCtx.Done():
			applog.WithEvent(s.log, "server.stop").Warn("subsystem drain timed out",
				slog.Int("pending", pending))
			pending = 0
		}
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

type hostPort struct {
	host string
	port int
}

// resolveAdminAddr applies the "local-only bind when TRANSPORT=stdio unless
// explicitly configured" rule. Operators on stdio who want a non-local admin
// surface must set BIND_ADDR explicitly.
func (s *Server) resolveAdminAddr() hostPort {
	host := s.cfg.BindAddr
	if s.cfg.Transport == config.TransportStdio && !s.cfg.BindAddrExplicit {
		host = "127.0.0.1"
	}
	return hostPort{host: host, port: s.cfg.AdminPort}
}
