// Package tools wires MCP tool handlers (contacts, groups, outbound
// messaging, ...) to their backing stores. Handlers are pure functions
// that take decoded JSON arguments and return JSON-serialisable
// payloads, plumbed into the internal/mcp registry by Register.
//
// The package deliberately sits above internal/cache and internal/wa so
// that tool handlers stay side-effect-free from a transport point of view
// and can be tested with seeded stores + mocked whatsmeow clients.
package tools

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// WAClient is the subset of the whatsmeow surface this package needs. It
// is satisfied by *wa.Client in production and by test mocks. Keeping the
// interface narrow makes it trivial to stub group / user lookups and
// message sends without spinning up the full whatsmeow stack.
type WAClient interface {
	GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	UserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	ProfilePictureURL(ctx context.Context, jid types.JID) (string, error)
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error)
	OwnJID() types.JID

	// Whatsmeow exposes the raw client for surfaces this interface does
	// not wrap — today only media downloads, which need whatsmeow's
	// media-conn refresh and CDN retry logic verbatim. May return nil
	// before the client has been constructed; callers must check.
	Whatsmeow() *whatsmeow.Client

	// Authoritative reconciliation surface — used by cache_sync to refresh
	// the local cache against the WhatsApp servers. IsLoggedIn is the
	// pre-flight short-circuit; the others are the actual fan-out.
	IsLoggedIn() bool
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetSubscribedNewsletters(ctx context.Context) ([]*types.NewsletterMetadata, error)
	FetchAppState(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error

	// Pairing surface — used by the pairing_start and pairing_complete
	// MCP tools. The lifecycle is owned by *wa.Client; this interface
	// just forwards.
	StartPairing(ctx context.Context, deviceName string) (<-chan wa.PairEvent, error)
	PairPhone(ctx context.Context, phone string) (string, error)
	PairLatest() (wa.PairEvent, bool)
	PairWaitNext(ctx context.Context) (wa.PairEvent, bool, error)
	Status() wa.Status
}

// MediaDownloader is the narrow whatsmeow download surface used by
// download_media. *whatsmeow.Client satisfies it directly; tests supply a
// fake through Deps.Downloader so they never need a live media connection.
type MediaDownloader interface {
	// Download resolves the locator on a downloadable protobuf message —
	// preferring a non-web.whatsapp.net URL, then the direct path.
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
	// DownloadMediaWithPath fetches by direct path, which outlives the URL.
	DownloadMediaWithPath(ctx context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, fileLength int, mediaType whatsmeow.MediaType, mmsType string) ([]byte, error)
}

// Deps is the wiring carried into each tool handler. Fields are optional
// at the struct level but individual tools document which they require.
type Deps struct {
	Cache    *cache.Store
	WA       WAClient
	Ingestor *cache.Ingestor         // optional; cache_sync_status reads its heartbeat
	Sync     *cache.SyncOrchestrator // optional; required by cache_sync, surfaced by cache_sync_status
	Media    *media.Store            // optional; required by download_media
	// Downloader overrides the media-download surface. Nil means "use
	// WA.Whatsmeow()", which is what production wants — the raw client is
	// re-created across pair/unpair, so it must be resolved per call
	// rather than captured at registration time.
	Downloader MediaDownloader
}

// downloader resolves the media-download surface for a single call. It
// returns nil when whatsmeow has no client yet (pre-pairing), which callers
// surface as not_paired.
func (d Deps) downloader() MediaDownloader {
	if d.Downloader != nil {
		return d.Downloader
	}
	if d.WA == nil {
		return nil
	}
	// Guard the typed-nil-in-interface trap: a nil *whatsmeow.Client
	// assigned to MediaDownloader would compare non-nil at the call site.
	if wm := d.WA.Whatsmeow(); wm != nil {
		return wm
	}
	return nil
}
