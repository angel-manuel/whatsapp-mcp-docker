package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"

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
	// Transport is either TransportHTTP or TransportStdio. Run serves only
	// stdio; for HTTP the caller mounts HTTPHandler on its own listener,
	// behind its own authentication (one Server per WhatsApp session).
	Transport TransportMode
	// Name and Version identify the server in MCP initialize responses.
	Name    string
	Version string
	// Routes are additional HTTP handlers mounted on the same mux as
	// /mcp, keyed by http.ServeMux pattern. They sit behind whatever
	// authentication the caller wraps HTTPHandler in — there is
	// deliberately no second auth system and no second port.
	//
	// This exists for the one thing MCP structurally cannot do: transfer
	// bytes — today GET /media/{sha256} outbound and POST /media inbound.
	// Anything that can be a tool stays a tool.
	Routes map[string]http.Handler
	// CacheOnlyTools names tools that answer purely from the local cache
	// and therefore stay callable while the device is linked but offline.
	// They are still gated on pairing; only the connection check is waived.
	//
	// The caller supplies this rather than the list living here because
	// only the wiring layer knows which registrations were handed a cache
	// store and nothing else. See internal/server.buildMCP.
	CacheOnlyTools []string
}

// Server is a transport-agnostic MCP server wrapping mcp-go. It holds
// the tool registry and the pairing-state gate, and exposes Run which
// blocks until ctx is cancelled.
type Server struct {
	cfg     Config
	log     *slog.Logger
	reg     *Registry
	pairing PairingState
	// cacheOnly is Config.CacheOnlyTools as a set, built once in New.
	cacheOnly map[string]struct{}

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
	cacheOnly := make(map[string]struct{}, len(cfg.CacheOnlyTools))
	for _, name := range cfg.CacheOnlyTools {
		cacheOnly[name] = struct{}{}
	}
	return &Server{cfg: cfg, log: log, reg: reg, pairing: pairing, cacheOnly: cacheOnly}, nil
}

func (c Config) validate() error {
	switch c.Transport {
	case TransportHTTP, TransportStdio:
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

// Run serves the stdio transport and blocks until ctx is cancelled or
// stdin closes. HTTP is not served here: the caller owns the listener and
// the per-session authentication, and mounts HTTPHandler behind them.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("mcp: server already started")
	}
	s.started = true
	s.mu.Unlock()

	if s.cfg.Transport != TransportStdio {
		return fmt.Errorf("mcp: Run serves stdio only; mount HTTPHandler for %q", s.cfg.Transport)
	}
	return s.runStdio(ctx, s.buildCore())
}

// exemptFromPairingGate names the tools that must remain callable before
// the device has been linked — and, equally, while it is linked but the
// socket is down. ping is the health check (it is itself designed to
// report paired/connected false); pairing_start and pairing_complete drive
// the link flow; cache_sync_status reports when the last event arrived,
// which is exactly the diagnostic wanted during a disconnection.
//
// Exemption is by name rather than per-tool flag so the policy lives in
// one transport-level place; tools authoring stays decoupled from it.
var exemptFromPairingGate = map[string]struct{}{
	"ping":              {},
	"pairing_start":     {},
	"pairing_complete":  {},
	"cache_sync_status": {},
}

// buildCore constructs the underlying mcp-go server with the registry
// and pairing middleware applied. Split out so tests can drive it
// without spinning up a transport.
func (s *Server) buildCore() *mcpserver.MCPServer {
	core := mcpserver.NewMCPServer(
		s.cfg.Name, s.cfg.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithToolHandlerMiddleware(pairingMiddleware(s.pairing, exemptFromPairingGate, s.cacheOnly)),
		mcpserver.WithRecovery(),
	)
	s.reg.apply(core)
	return core
}

// HTTPHandler returns the streamable-HTTP transport at "/mcp" plus every
// Config.Routes entry on one mux.
//
// It performs NO authentication. The caller must authenticate each request
// and route it to the Server belonging to the session the credential
// selects — see internal/server, which owns the listener, the bearer gate
// and /healthz. Each call builds a fresh handler, so call it once per
// session and keep the result.
func (s *Server) HTTPHandler() http.Handler {
	streamable := mcpserver.NewStreamableHTTPServer(s.buildCore(),
		mcpserver.WithStateLess(true),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamable)
	for pattern, h := range s.cfg.Routes {
		if h == nil {
			continue
		}
		mux.Handle(pattern, h)
	}
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
