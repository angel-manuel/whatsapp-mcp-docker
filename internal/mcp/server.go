package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
)

// TransportMode selects between stdio and HTTP/SSE serving.
type TransportMode string

// Supported transports.
const (
	TransportHTTP  TransportMode = "http"
	TransportStdio TransportMode = "stdio"
)

// Config is the minimum set of knobs the MCP server needs. It is
// deliberately smaller than the top-level app config so tests can
// construct it without pulling in the env-var loader.
type Config struct {
	// Transport is either TransportHTTP or TransportStdio.
	Transport TransportMode
	// BindAddr is the interface for the HTTP listener (HTTP only).
	BindAddr string
	// Port is the TCP port for the HTTP listener (HTTP only).
	Port int
	// AuthToken, when non-empty, is required as a bearer token on every
	// HTTP request. Must be set when Transport==TransportHTTP.
	AuthToken string
	// Name and Version identify the server in MCP initialize responses.
	Name    string
	Version string
}

// Server is a transport-agnostic MCP server wrapping mcp-go. It holds
// the tool registry and the pairing-state gate, and exposes Run which
// blocks until ctx is cancelled.
type Server struct {
	cfg     Config
	log     *slog.Logger
	reg     *Registry
	pairing PairingState

	// mu guards the single-Run invariant.
	mu      sync.Mutex
	started bool
}

// New builds a Server. The registry and pairing-state arguments may be
// nil; a nil registry means "no tools" (the ping stub is always added)
// and a nil pairing state is treated as "never paired".
func New(cfg Config, log *slog.Logger, reg *Registry, pairing PairingState) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if reg == nil {
		reg = NewRegistry()
	}
	if pairing == nil {
		pairing = NeverPaired
	}
	if err := registerBuiltins(reg, pairing); err != nil {
		return nil, fmt.Errorf("register builtins: %w", err)
	}
	return &Server{cfg: cfg, log: log, reg: reg, pairing: pairing}, nil
}

func (c Config) validate() error {
	switch c.Transport {
	case TransportHTTP:
		if c.AuthToken == "" {
			// Defence in depth: the top-level config loader rejects
			// this case too, but the MCP layer MUST NOT come up on
			// HTTP without auth even if invoked directly.
			return errors.New("mcp: HTTP transport requires AuthToken")
		}
		if c.Port <= 0 || c.Port > 65535 {
			return fmt.Errorf("mcp: invalid port %d", c.Port)
		}
	case TransportStdio:
	default:
		return fmt.Errorf("mcp: unknown transport %q", c.Transport)
	}
	if c.Name == "" {
		return errors.New("mcp: server name must not be empty")
	}
	return nil
}

// Registry exposes the server's tool registry so wiring code can add
// additional tools between New and Run.
func (s *Server) Registry() *Registry { return s.reg }

// Run starts the configured transport and blocks until ctx is
// cancelled or the transport errors. Stdio mode returns when stdin
// closes; HTTP mode shuts the listener down gracefully on cancel.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("mcp: server already started")
	}
	s.started = true
	s.mu.Unlock()

	core := s.buildCore()

	switch s.cfg.Transport {
	case TransportStdio:
		return s.runStdio(ctx, core)
	case TransportHTTP:
		return s.runHTTP(ctx, core)
	default:
		return fmt.Errorf("mcp: unknown transport %q", s.cfg.Transport)
	}
}

// buildCore constructs the underlying mcp-go server with the registry
// and pairing middleware applied. Split out so tests can drive it
// without spinning up a transport.
func (s *Server) buildCore() *mcpserver.MCPServer {
	core := mcpserver.NewMCPServer(
		s.cfg.Name, s.cfg.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithToolHandlerMiddleware(pairingMiddleware(s.pairing)),
		mcpserver.WithRecovery(),
	)
	s.reg.apply(core)
	return core
}

// HTTPHandler returns the HTTP/SSE transport handler with bearer auth
// applied. The returned handler expects requests at "/mcp". Intended
// for tests that need to exercise the HTTP plumbing via httptest.
func (s *Server) HTTPHandler() http.Handler {
	streamable := mcpserver.NewStreamableHTTPServer(s.buildCore(),
		mcpserver.WithStateLess(true),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerAuth(s.cfg.AuthToken, streamable))
	return mux
}

// ListenStdio runs the stdio transport against caller-supplied pipes.
// Intended for tests; production code should call Run.
func (s *Server) ListenStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	core := s.buildCore()
	srv := mcpserver.NewStdioServer(core)
	return srv.Listen(ctx, in, out)
}

func (s *Server) runStdio(ctx context.Context, core *mcpserver.MCPServer) error {
	applog.WithEvent(s.log, "mcp.start").Info("mcp stdio server starting",
		slog.String("name", s.cfg.Name),
		slog.String("version", s.cfg.Version),
	)

	srv := mcpserver.NewStdioServer(core)
	err := srv.Listen(ctx, os.Stdin, os.Stdout)

	applog.WithEvent(s.log, "mcp.stop").Info("mcp stdio server stopped")
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (s *Server) runHTTP(ctx context.Context, core *mcpserver.MCPServer) error {
	streamable := mcpserver.NewStreamableHTTPServer(core,
		mcpserver.WithStateLess(true),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearerAuth(s.cfg.AuthToken, streamable))

	addr := net.JoinHostPort(s.cfg.BindAddr, fmt.Sprintf("%d", s.cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp: listen %s: %w", addr, err)
	}

	applog.WithEvent(s.log, "mcp.start").Info("mcp http server starting",
		slog.String("name", s.cfg.Name),
		slog.String("version", s.cfg.Version),
		slog.String("addr", ln.Addr().String()),
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
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		applog.WithEvent(s.log, "mcp.stop").Info("mcp http server stopped")
		<-errCh
		return nil
	case err := <-errCh:
		applog.WithEvent(s.log, "mcp.stop").Info("mcp http server stopped")
		return err
	}
}

// bearerAuth wraps next with a constant-time bearer-token check.
// Requests without a matching Authorization header receive 401.
func bearerAuth(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		got := []byte(strings.TrimPrefix(auth, prefix))
		if !strings.HasPrefix(auth, prefix) || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
