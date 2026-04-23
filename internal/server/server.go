// Package server wires the loaded configuration and logger into a runnable
// application. It currently provides only graceful-shutdown plumbing; HTTP
// listeners and the whatsmeow client will hang off this type as those
// subsystems land.
package server

import (
	"context"
	"log/slog"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/config"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
)

// Server is the top-level application container.
type Server struct {
	cfg *config.Config
	log *slog.Logger
}

// New constructs a Server from an already-loaded config and logger.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// Run blocks until ctx is cancelled, then returns nil. Subsystems will later
// register their own lifecycles here and this loop will orchestrate their
// shutdown.
func (s *Server) Run(ctx context.Context) error {
	log := applog.WithEvent(s.log, "server.start")
	log.Info("server starting",
		slog.String("transport", string(s.cfg.Transport)),
		slog.String("bind_addr", s.cfg.BindAddr),
		slog.Int("port", s.cfg.Port),
		slog.Int("admin_port", s.cfg.AdminPort),
		slog.String("data_dir", s.cfg.DataDir),
	)

	<-ctx.Done()

	applog.WithEvent(s.log, "server.stop").Info("server stopping")
	return nil
}
