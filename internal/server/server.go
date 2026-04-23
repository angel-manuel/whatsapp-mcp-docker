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
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
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

	waCli, err := wa.Open(ctx, wa.Config{
		DataDir:        s.cfg.DataDir,
		PairDeviceName: s.cfg.PairDeviceName,
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

	mcpSrv, err := s.buildMCP(waCli)
	if err != nil {
		// Shut the admin listener down before bailing.
		_ = httpSrv.Shutdown(context.Background())
		return fmt.Errorf("build mcp server: %w", err)
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

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	mcpCancel()

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		applog.WithEvent(s.log, "admin.shutdown").Warn("graceful shutdown failed",
			slog.String("err", err.Error()))
	}

	applog.WithEvent(s.log, "server.stop").Info("server stopping")
	return runErr
}

// buildMCP constructs the MCP subsystem and binds its pairing gate to
// the whatsmeow client: tools fail with not_paired until the device is
// both paired AND logged in.
func (s *Server) buildMCP(waCli *wa.Client) (*mcp.Server, error) {
	pairing := mcp.PairingStateFunc(func() bool {
		st := waCli.Status()
		return st.LoggedIn
	})
	return mcp.New(mcp.Config{
		Transport: mcp.TransportMode(s.cfg.Transport),
		BindAddr:  s.cfg.BindAddr,
		Port:      s.cfg.Port,
		AuthToken: s.cfg.AuthToken,
		Name:      "whatsapp-mcp",
		Version:   Version,
	}, s.log, nil, pairing)
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
