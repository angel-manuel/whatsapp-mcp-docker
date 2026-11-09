package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/audio"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcptools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// Status is a session's coarse lifecycle state, as reported by GET /sessions.
type Status string

// Session states.
const (
	// StatusUnpaired: no device credentials yet; pair with pairing_start.
	StatusUnpaired Status = "unpaired"
	// StatusConnected: paired and the socket is authenticated.
	StatusConnected Status = "connected"
	// StatusDisconnected: paired, but the socket is down (reconnecting, or
	// replaced by another client). Reported separately so a dead socket is
	// never shown as connected.
	StatusDisconnected Status = "disconnected"
	// StatusLoggedOut: the phone (or WhatsApp) unlinked this device. The
	// token stays valid; pairing_start links a device again under it.
	StatusLoggedOut Status = "logged_out"
	// StatusError: the session is registered but failed to open at startup.
	// Its token is answered with 503 until a restart opens it.
	StatusError Status = "error"
)

// sweeperDrainTimeout bounds how long Close waits for the media retention
// loop. It only ever blocks on a ticker, so this is a backstop.
const sweeperDrainTimeout = 2 * time.Second

// Options configures every session the same way; only the id and directory
// differ between sessions.
type Options struct {
	// Name and Version identify the MCP server in initialize responses.
	Name    string
	Version string
	// Transport is the MCP transport the session is served over.
	Transport mcp.TransportMode

	PairDeviceName string
	FFmpegPath     string

	// Media limits apply per session: each has its own media/ directory.
	MediaMaxBytes       int64
	MediaTTL            time.Duration
	MediaSweepInterval  time.Duration
	MediaMaxUploadBytes int64

	Logger *slog.Logger
}

// Session is one WhatsApp account: a whatsmeow client, its cache, its media
// store and its MCP server, all rooted in one directory.
type Session struct {
	id  string
	dir string
	log *slog.Logger

	cancel      context.CancelFunc
	sweeperDone chan struct{}

	cache   *cache.Store
	wa      *wa.Client
	mcp     *mcp.Server
	handler http.Handler
}

// Open brings up the session rooted at dir, creating its files if needed.
// The session's background work runs until Close; it is deliberately not
// tied to any caller context, because a session is usually created from an
// HTTP request that ends long before the session should.
func Open(id, dir string, opts Options) (_ *Session, err error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	log := opts.Logger.With(slog.String("session", id))
	s := &Session{id: id, dir: dir, log: log}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	// Cache migrations are schema-only; Open either succeeds or fails on
	// its own terms. Opened before wa so the ingestor is wired into the wa
	// EventHook from the very first whatsmeow event.
	if s.cache, err = cache.Open(ctx, dir); err != nil {
		return nil, fmt.Errorf("cache open: %w", err)
	}

	ingestor := cache.NewIngestor(s.cache, applog.WithEvent(log, "cache.ingest"))
	syncOrch := cache.NewSyncOrchestrator(s.cache, ingestor, applog.WithEvent(log, "cache.sync")).
		WithBaseContext(ctx)

	// DeferConnect: the socket must not go live until the ingestor is fully
	// wired (see SetPollDecrypter below). WhatsApp flushes its offline queue
	// immediately after login, so an event handler that is still missing a
	// dependency at that moment misses the burst, not a stray event.
	// Logger: without it whatsmeow's reconnect and stream-error records are
	// dropped, leaving a socket that dies with no trace in the logs.
	if s.wa, err = wa.Open(ctx, wa.Config{
		DataDir:        dir,
		PairDeviceName: opts.PairDeviceName,
		EventHook:      ingestor.HandleEvent,
		DeferConnect:   true,
		Logger:         wa.SlogAdapter(applog.WithEvent(log, "wa.whatsmeow")),
	}); err != nil {
		return nil, fmt.Errorf("wa open: %w", err)
	}

	// Poll votes arrive encrypted against the poll creation message, so the
	// ingestor needs a way back into whatsmeow to read them. A vote that
	// lands before the decrypter does is lost for good — hence DeferConnect.
	ingestor.SetPollDecrypter(s.wa)

	// Media blobs: downloaded by download_media, uploaded by POST /media
	// for the send tools to reference, and served by GET /media/{sha256}.
	mediaStore, err := media.Open(filepath.Join(dir, "media"), media.Options{
		MaxBytes:       opts.MediaMaxBytes,
		TTL:            opts.MediaTTL,
		MaxUploadBytes: opts.MediaMaxUploadBytes,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("media open: %w", err)
	}

	if s.mcp, err = buildMCP(opts, log, s.wa, s.cache, mediaStore); err != nil {
		return nil, fmt.Errorf("build mcp server: %w", err)
	}
	if err = tools.Register(s.mcp.Registry(), tools.Deps{
		Cache:    s.cache,
		WA:       s.wa,
		Ingestor: ingestor,
		Sync:     syncOrch,
		Media:    mediaStore,
		// Probed per call, not at startup: the distroless image ships no
		// ffmpeg, and an operator may bind-mount one into a running
		// container. Absent ffmpeg means non-Opus audio is refused, never
		// silently sent (REQUIREMENTS.md, FFMPEG_PATH).
		Audio: audio.NewTranscoder(opts.FFmpegPath),
	}); err != nil {
		return nil, fmt.Errorf("register tools: %w", err)
	}
	if opts.Transport == mcp.TransportHTTP {
		s.handler = s.mcp.HTTPHandler()
	}

	// Media retention: one pass now (the session may have been down for
	// longer than MEDIA_TTL, and expired bytes must not stay readable),
	// then every MEDIA_SWEEP_INTERVAL until Close.
	s.sweeperDone = make(chan struct{})
	go func() {
		defer close(s.sweeperDone)
		mediaStore.RunSweeper(ctx, opts.MediaSweepInterval)
	}()

	// Every event consumer is wired; dial WhatsApp. Failure is not fatal:
	// auto-reconnect retries. An unpaired session stays offline until
	// pairing_start.
	if cerr := s.wa.Connect(ctx); cerr != nil && !errors.Is(cerr, wa.ErrNotPaired) {
		log.Warn("initial connect failed; auto-reconnect will retry",
			slog.String("err", cerr.Error()))
	}
	return s, nil
}

// ID returns the session id.
func (s *Session) ID() string { return s.id }

// Handler serves /mcp and the /media routes for this session. It performs
// no authentication: the caller has already resolved the session from the
// request's token. Nil unless the session was opened for HTTP.
func (s *Session) Handler() http.Handler { return s.handler }

// MCP returns the session's MCP server, for serving it over stdio.
func (s *Session) MCP() *mcp.Server { return s.mcp }

// Status maps the whatsmeow lifecycle onto the coarse admin-API states.
func (s *Session) Status() Status {
	switch {
	case s.wa.State() == wa.StateLoggedOut:
		return StatusLoggedOut
	case !s.wa.IsPaired():
		return StatusUnpaired
	case s.wa.Status().LoggedIn:
		return StatusConnected
	default:
		return StatusDisconnected
	}
}

// Logout unlinks the device from the phone, best-effort. See wa.Client.Logout.
func (s *Session) Logout(ctx context.Context) error { return s.wa.Logout(ctx) }

// Close stops background work and closes every store, in the reverse of
// the order Open built them. Safe on a partially opened Session.
func (s *Session) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	var errs []error
	if s.sweeperDone != nil {
		select {
		case <-s.sweeperDone:
		case <-time.After(sweeperDrainTimeout):
			s.log.Warn("media sweeper drain timed out")
		}
	}
	if s.wa != nil {
		if err := s.wa.Close(); err != nil {
			errs = append(errs, fmt.Errorf("wa close: %w", err))
		}
	}
	if s.cache != nil {
		if err := s.cache.Close(); err != nil {
			errs = append(errs, fmt.Errorf("cache close: %w", err))
		}
	}
	return errors.Join(errs...)
}

// waPairingState binds the MCP gate's two predicates to the whatsmeow
// client. They must stay separate: IsPaired asks the on-disk device
// store, IsConnected asks the live socket, and collapsing both into
// LoggedIn (as this once did) reports a linked-but-offline client as
// not_paired — which sends callers to pairing_start, which then refuses
// with already_paired because the device row is still there.
type waPairingState struct{ cli *wa.Client }

// IsPaired reports whether device credentials exist on disk. This is the
// same predicate wa.StartPairing uses to reject with ErrAlreadyPaired, so
// the gate and the pairing tools can no longer disagree.
func (w waPairingState) IsPaired() bool { return w.cli.IsPaired() }

// IsConnected reports whether the socket is authenticated right now.
func (w waPairingState) IsConnected() bool { return w.cli.Status().LoggedIn }

// buildMCP constructs the session's MCP server, binds its pairing gate to
// the whatsmeow client (tools fail with not_paired until the device is
// linked, then with not_connected until the socket is live), and registers
// the read-side cache-backed tools against its registry.
func buildMCP(opts Options, log *slog.Logger, waCli *wa.Client, cacheStore *cache.Store, mediaStore *media.Store) (*mcp.Server, error) {
	reg := mcp.NewRegistry()
	if err := mcptools.Register(reg, cacheStore); err != nil {
		return nil, fmt.Errorf("register cache tools: %w", err)
	}
	// Everything registered so far came from mcptools, whose Register takes
	// a *cache.Store and nothing else — those tools have no way to reach the
	// network even in principle. Reading the names back off the registry at
	// this exact point derives the cache-only set from that structural fact
	// instead of restating it as a list that can drift.
	cacheOnly := reg.Names()
	// The media byte routes ride the session's handler: same port, same
	// session token. They exist for the one thing MCP cannot do — transfer
	// bytes. GET is the download_media half; POST is the inbound half the
	// media send tools consume. Each session has its own media store, so a
	// digest uploaded through one session's token is invisible to another.
	routes := map[string]http.Handler{
		media.RoutePrefix + "{sha256}": mediaStore.Handler(),
		media.UploadRoutePattern:       mediaStore.UploadHandler(),
	}
	return mcp.New(mcp.Config{
		Transport:      opts.Transport,
		Name:           opts.Name,
		Version:        opts.Version,
		Routes:         routes,
		CacheOnlyTools: cacheOnly,
	}, log, reg, waPairingState{cli: waCli})
}
