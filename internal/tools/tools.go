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
	"io"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/audio"
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
	// BuildReaction builds the reaction envelope for SendMessage. sender is
	// the *target message's* author (empty for our own message), which is
	// what whatsmeow keys the reaction off. Returns nil pre-pair.
	BuildReaction(chat, sender types.JID, id types.MessageID, emoji string) *waE2E.Message
	OwnJID() types.JID

	// Poll surface — used by send_poll and vote_poll. StoreMessageSecret is
	// part of it because whatsmeow only records a message secret for messages
	// it *receives*; a poll we send has to have its secret persisted by hand
	// or the votes on it can never be decrypted.
	BuildPollCreation(name string, options []string, selectableCount int) (*waE2E.Message, error)
	BuildPollVote(ctx context.Context, pollInfo *types.MessageInfo, optionNames []string) (*waE2E.Message, error)
	StoreMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID, secret []byte) error

	// Account-visible mutations — the surface behind set_status_message,
	// send_presence, send_chat_presence, subscribe_presence,
	// set_disappearing_timer, set_default_disappearing_timer and mark_read.
	// Every one of these is observable by other WhatsApp users, so the
	// handlers validate their inputs before getting here.
	SetStatusMessage(ctx context.Context, msg string) error
	SendPresence(ctx context.Context, state types.Presence) error
	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	SubscribePresence(ctx context.Context, jid types.JID) error
	SetDisappearingTimer(ctx context.Context, chat types.JID, timer time.Duration) error
	SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID) error

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

// MediaUploader is the narrow whatsmeow upload surface used by the media
// send tools, and the mirror of MediaDownloader. *whatsmeow.Client
// satisfies it directly; tests supply a fake through Deps.Uploader so they
// never need a live media connection.
//
// Both methods exist because the two send paths want different things:
// send_file streams a stored blob straight off disk, while the audio path
// already holds transcoded bytes in memory.
type MediaUploader interface {
	// Upload encrypts and uploads an in-memory payload.
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	// UploadReader streams plaintext, using tempFile (or its own temp file
	// when nil) for the encrypted copy.
	UploadReader(ctx context.Context, plaintext io.Reader, tempFile io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
}

// AudioTranscoder converts arbitrary audio to the Ogg/Opus WhatsApp plays.
// Satisfied by *audio.Transcoder; the interface exists so tests can drive
// both sides of the FFMPEG_PATH contract without an ffmpeg binary.
type AudioTranscoder interface {
	// Available reports whether a usable ffmpeg binary is present.
	Available() bool
	// Path is the binary that was probed, for error messages.
	Path() string
	// ToOpus transcodes in to a mono 48 kHz Ogg/Opus stream.
	ToOpus(ctx context.Context, in io.Reader) ([]byte, error)
}

// Deps is the wiring carried into each tool handler. Fields are optional
// at the struct level but individual tools document which they require.
type Deps struct {
	Cache    *cache.Store
	WA       WAClient
	Ingestor *cache.Ingestor         // optional; cache_sync_status reads its heartbeat
	Sync     *cache.SyncOrchestrator // optional; required by cache_sync, surfaced by cache_sync_status
	Media    *media.Store            // optional; required by download_media and the media send tools
	// Audio transcodes non-Opus audio for send_audio_message. Nil, or a
	// transcoder whose Available() is false, means the server refuses
	// non-Opus audio instead of sending something WhatsApp cannot play.
	Audio AudioTranscoder
	// Uploader overrides the media-upload surface. Nil means "use
	// WA.Whatsmeow()", resolved per call for the same reason Downloader is.
	Uploader MediaUploader
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

// uploader resolves the media-upload surface for a single call, with the
// same nil-handling as downloader: nil means whatsmeow has no client yet,
// which callers surface as not_paired.
func (d Deps) uploader() MediaUploader {
	if d.Uploader != nil {
		return d.Uploader
	}
	if d.WA == nil {
		return nil
	}
	if wm := d.WA.Whatsmeow(); wm != nil {
		return wm
	}
	return nil
}

// Compile-time proof that the production transcoder satisfies the
// interface the tools depend on.
var _ AudioTranscoder = (*audio.Transcoder)(nil)
