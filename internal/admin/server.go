// Package admin exposes the HTTP surface used by external orchestrators to
// broker pairing, observe session lifecycle, and drive unpair/reset.
//
// The endpoints are a stable contract documented in REQUIREMENTS.md
// ("Pairing", "Session lifecycle events", "Observability" sections).
package admin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// WAService is the subset of wa.Client that the admin HTTP handlers depend on.
// Tests substitute a fake; production wires *wa.Client.
type WAService interface {
	Status() wa.Status
	Subscribe() (<-chan wa.Event, func())
	StartPairing(ctx context.Context, deviceName string) (<-chan wa.PairEvent, error)
	PairPhone(ctx context.Context, phone string) (string, error)
	Unpair(ctx context.Context) error
}

// Config controls the admin HTTP server.
type Config struct {
	// BindAddr is the interface the admin listener binds to. The caller is
	// responsible for resolving the stdio-vs-http defaulting — the admin
	// package takes the address as authoritative.
	BindAddr string
	// Port is the admin HTTP port.
	Port int
	// AuthToken, when non-empty, is required on every route except /admin/health.
	AuthToken string
	// RequireAuth forces the bearer-token check even when AuthToken is empty
	// (in which case all requests are rejected). Typically true in HTTP
	// transport mode.
	RequireAuth bool
}

// Server is the admin HTTP surface.
type Server struct {
	cfg   Config
	log   *slog.Logger
	wa    WAService
	start time.Time
	mux   *http.ServeMux
}

// New constructs a Server. The returned value is immediately ready to serve
// via Handler() or ListenAndServe.
func New(cfg Config, log *slog.Logger, svc WAService) *Server {
	s := &Server{
		cfg:   cfg,
		log:   log,
		wa:    svc,
		start: time.Now(),
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the composed http.Handler with auth middleware applied.
// /admin/health is exempt from auth so container liveness probes work
// regardless of credential configuration.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// routes registers all admin handlers on the server's mux.
func (s *Server) routes() {
	// Health is always unauthenticated.
	s.mux.HandleFunc("GET /admin/health", s.handleHealth)

	// All other routes go through bearer-auth middleware.
	s.mux.Handle("GET /admin/ready", s.authed(http.HandlerFunc(s.handleReady)))
	s.mux.Handle("GET /admin/status", s.authed(http.HandlerFunc(s.handleStatus)))
	s.mux.Handle("GET /admin/events", s.authed(http.HandlerFunc(s.handleEvents)))
	s.mux.Handle("POST /admin/pair/start", s.authed(http.HandlerFunc(s.handlePairStart)))
	s.mux.Handle("POST /admin/pair/phone", s.authed(http.HandlerFunc(s.handlePairPhone)))
	s.mux.Handle("POST /admin/unpair", s.authed(http.HandlerFunc(s.handleUnpair)))
}
