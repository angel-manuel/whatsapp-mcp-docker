// Package server wires the loaded configuration and logger into a runnable
// application. Over HTTP it hosts many WhatsApp sessions, each selected by
// its own bearer token, plus the /sessions admin API; over stdio it serves
// exactly one session.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/sessions"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// shutdownTimeout bounds how long we wait for the transport to exit after
// context cancellation so we don't block indefinitely on a slow consumer.
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

// sessionOptions is the configuration every session shares.
func (s *Server) sessionOptions() sessions.Options {
	return sessions.Options{
		Name:                "whatsapp-mcp",
		Version:             Version,
		Transport:           mcp.TransportMode(s.cfg.Transport),
		PairDeviceName:      s.cfg.PairDeviceName,
		FFmpegPath:          s.cfg.FFmpegPath,
		MediaMaxBytes:       s.cfg.MediaMaxBytes,
		MediaTTL:            s.cfg.MediaTTL,
		MediaSweepInterval:  s.cfg.MediaSweepInterval,
		MediaMaxUploadBytes: s.cfg.MediaMaxUploadBytes,
		Logger:              s.log,
	}
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

	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	// One process per volume. Each session also locks its own directory,
	// but the registry and the sessions/ tree are shared state.
	rootLock, err := wa.AcquireLock(filepath.Join(s.cfg.DataDir, ".lock"))
	if err != nil {
		return fmt.Errorf("lock data dir: %w", err)
	}
	defer func() { _ = rootLock.Release() }()

	s.warnLegacyLayout()

	var runErr error
	if s.cfg.Transport == config.TransportStdio {
		runErr = s.runStdio(ctx)
	} else {
		runErr = s.runHTTP(ctx)
	}
	applog.WithEvent(s.log, "server.stop").Info("server stopping")
	return runErr
}

// warnLegacyLayout spots a 0.7 volume — session.db directly under DATA_DIR —
// and says exactly how to move it into a session. Files are never moved
// automatically: a wrong guess here would unlink someone's phone.
func (s *Server) warnLegacyLayout() {
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "session.db")); err != nil {
		return
	}
	target := filepath.Join(s.cfg.SessionsDir(), s.cfg.StdioSession)
	applog.WithEvent(s.log, "server.legacy_layout").Warn(
		"found a pre-multi-session layout: "+filepath.Join(s.cfg.DataDir, "session.db")+
			" is not served. Stop the container, move session.db*, cache.db* and media/ into "+
			target+"/ (owned by uid 1000), start it again, then POST /sessions {\"id\":\""+
			s.cfg.StdioSession+"\"} to adopt it under a new token. See \"Upgrading from 0.7\" in README.md",
		slog.String("target", target))
}

// runStdio serves the single STDIO_SESSION session over stdin/stdout. There
// is no admin API in stdio mode, so the session is created on first start
// and needs no registry entry or token.
func (s *Server) runStdio(ctx context.Context) error {
	id := s.cfg.StdioSession
	sess, err := sessions.Open(id, filepath.Join(s.cfg.SessionsDir(), id), s.sessionOptions())
	if err != nil {
		return fmt.Errorf("open session %s: %w", id, err)
	}
	defer func() {
		if err := sess.Close(); err != nil {
			applog.WithEvent(s.log, "server.stop").Warn("session close",
				slog.String("err", err.Error()))
		}
	}()

	mcpCtx, mcpCancel := context.WithCancel(ctx)
	defer mcpCancel()
	errCh := make(chan error, 1)
	go func() {
		if err := sess.MCP().Run(mcpCtx); err != nil {
			errCh <- fmt.Errorf("mcp: %w", err)
			return
		}
		errCh <- nil
	}()

	// The stdio transport ending (client disconnected, stdin at EOF) is a
	// normal shutdown signal, so take its result directly rather than
	// draining again after cancel — errCh carries exactly one value.
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	mcpCancel()
	select {
	case err := <-errCh:
		return err
	case <-time.After(shutdownTimeout):
		applog.WithEvent(s.log, "server.stop").Warn("mcp goroutine drain timed out")
		return nil
	}
}

// runHTTP hosts every registered session behind one listener.
func (s *Server) runHTTP(ctx context.Context) error {
	reg, err := sessions.OpenRegistry(context.Background(), s.cfg.RegistryPath())
	if err != nil {
		return err
	}
	defer func() { _ = reg.Close() }()

	opts := s.sessionOptions()
	mgr := sessions.NewManager(reg, s.cfg.SessionsDir(), s.cfg.MaxSessions,
		func(id, dir string) (sessions.Instance, error) { return sessions.Open(id, dir, opts) },
		s.log)
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if err := mgr.Close(); err != nil {
			applog.WithEvent(s.log, "server.stop").Warn("sessions close",
				slog.String("err", err.Error()))
		}
	}()

	if s.cfg.AdminToken == "" {
		applog.WithEvent(s.log, "server.start").Warn(
			"ADMIN_TOKEN is unset: the /sessions admin API is anonymous. " +
				"Anyone who can reach it can create sessions and delete them. " +
				"Restrict it at the ingress, or set ADMIN_TOKEN")
	}

	addr := net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	httpSrv := &http.Server{
		Handler:           NewHandler(mgr, s.cfg.AdminToken, s.log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	applog.WithEvent(s.log, "mcp.start").Info("http server starting",
		slog.String("addr", ln.Addr().String()),
		slog.Int("sessions", mgr.Count()),
		slog.Int("max_sessions", s.cfg.MaxSessions),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	applog.WithEvent(s.log, "mcp.stop").Info("http server stopped")
	// Serve reports ErrServerClosed (sent as nil) after Shutdown; anything
	// else is a real failure that raced the cancellation.
	return <-errCh
}
