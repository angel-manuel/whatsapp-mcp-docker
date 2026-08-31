package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
)

var downloadMediaSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "chat_jid": {
      "type": "string",
      "description": "JID of the chat the message belongs to (e.g. 34600111222@s.whatsapp.net or 1234567890-1600000000@g.us)."
    },
    "message_id": {
      "type": "string",
      "description": "Stanza id of the message carrying the attachment, as returned by list_messages / get_conversation."
    }
  },
  "required": ["chat_jid", "message_id"],
  "additionalProperties": false
}`)

// downloadMediaArgs is the decoded argument object.
type downloadMediaArgs struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
}

// DownloadMediaResult is a *descriptor*, never the bytes. MCP is a poor
// carrier for binary payloads and an agent has no use for base64 in its
// context window, so the tool returns a pointer and the caller (or the
// gateway fronting this container) fetches MediaPath over HTTP with the same
// bearer token it uses for /mcp.
type DownloadMediaResult struct {
	MediaPath string `json:"media_path"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
}

// staleLocatorHint explains the one failure mode operators will actually
// hit: media_direct_path was added by migration 004 and cannot be
// backfilled, because the direct path only exists on the live protobuf at
// ingest time. Rows older than that migration have nothing but a media_url
// that WhatsApp expires, so re-ingesting is the only fix.
//
// No trailing period: this is appended to error strings, and ST1005 wants
// those to end without punctuation.
const staleLocatorHint = "this message was cached before media_direct_path was recorded (migration 004), " +
	"so only its expiring media_url is available and that URL is no longer valid; " +
	"the direct path cannot be backfilled — run the cache_sync tool to re-ingest the message, then retry"

// downloadMedia fetches the attachment bytes for one message, stores them
// content-addressed under {DATA_DIR}/media, and returns a descriptor. It is
// idempotent: a second call for the same message is a cache hit and performs
// no network I/O.
func downloadMedia(deps Deps) mcp.Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var args downloadMediaArgs
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return mcp.InvalidArgumentError(fmt.Sprintf("invalid arguments: %v", err)), nil
			}
		}
		chatJID := strings.TrimSpace(args.ChatJID)
		messageID := strings.TrimSpace(args.MessageID)
		if chatJID == "" {
			return mcp.InvalidArgumentError("chat_jid is required"), nil
		}
		if messageID == "" {
			return mcp.InvalidArgumentError("message_id is required"), nil
		}
		if deps.Media == nil {
			return mcp.InternalError("media store is not configured"), nil
		}

		row, err := lookupMediaRow(ctx, deps, chatJID, messageID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mcp.NotFoundError(fmt.Sprintf(
					"no cached message %s in chat %s; run cache_sync if it is older than the local cache",
					messageID, chatJID)), nil
			}
			return mcp.InternalError(err.Error()), nil
		}

		if !row.HasMedia() {
			if row.Kind == cache.KindText || row.Kind == cache.KindPoll || row.Kind == cache.KindOther {
				return mcp.NoMediaError(fmt.Sprintf(
					"message %s is a %s message and carries no attachment", messageID, row.Kind)), nil
			}
			return mcp.NoMediaError(fmt.Sprintf(
				"message %s is a %s message but no media key was cached for it, so its payload cannot be decrypted; run cache_sync to re-ingest it",
				messageID, row.Kind)), nil
		}

		// Cache hit short-circuit: the stored digest is the plaintext
		// SHA-256, which is exactly what the messages row already carries.
		if len(row.SHA256) == 32 {
			digest := fmt.Sprintf("%x", row.SHA256)
			if desc, err := deps.Media.Lookup(digest); err == nil {
				return toDownloadResult(desc), nil
			} else if !errors.Is(err, media.ErrNotFound) {
				return mcp.InternalError(err.Error()), nil
			}
		}

		dl := deps.downloader()
		if dl == nil {
			return mcp.NotPairedError(), nil
		}

		data, err := fetchMedia(ctx, dl, row)
		if err != nil {
			return mcp.MediaUnavailableError(err.Error()), nil
		}

		desc, err := deps.Media.Put(data, row.Mime, downloadFilename(row))
		if err != nil {
			return mcp.InternalError(err.Error()), nil
		}
		return toDownloadResult(desc), nil
	}
}

// lookupMediaRow finds the message by (chat_jid, id), retrying against the
// chat's linked identities. A contact addressed by phone JID and by privacy
// LID is one conversation to the user, so a message_id copied out of a
// merged view must resolve regardless of which side it came from.
func lookupMediaRow(ctx context.Context, deps Deps, chatJID, messageID string) (cache.MediaRow, error) {
	row, err := deps.Cache.GetMessageMedia(ctx, chatJID, messageID)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return row, err
	}
	linked, lerr := deps.Cache.ResolveLinkedJIDs(ctx, chatJID)
	if lerr != nil {
		return cache.MediaRow{}, err
	}
	for _, alt := range linked {
		if alt == chatJID {
			continue
		}
		row, aerr := deps.Cache.GetMessageMedia(ctx, alt, messageID)
		if aerr == nil {
			return row, nil
		}
		if !errors.Is(aerr, sql.ErrNoRows) {
			return cache.MediaRow{}, aerr
		}
	}
	return cache.MediaRow{}, sql.ErrNoRows
}

// fetchMedia downloads the attachment described by row.
//
// The direct path is preferred whenever it is present: it is the locator
// whatsmeow re-signs against a fresh media connection, whereas media_url is
// a pre-signed URL that expires. The URL is only used as a fallback for rows
// ingested before migration 004 — and then only when it is not a
// web.whatsapp.net URL, which whatsmeow's own Download treats as
// unusable-without-a-direct-path.
func fetchMedia(ctx context.Context, dl MediaDownloader, row cache.MediaRow) ([]byte, error) {
	mediaType, ok := mediaTypeForKind(row.Kind)
	if !ok {
		return nil, fmt.Errorf("message kind %q has no downloadable media type", row.Kind)
	}

	if row.DirectPath != "" {
		// mmsType empty: DownloadMediaWithPath derives it from mediaType.
		// allowNoHash=true keeps the pre-bump behaviour: whatsmeow now
		// substitutes a 32-byte zero hash when fileHash is nil, which would
		// turn a cache row with no stored SHA256 into a guaranteed hash
		// mismatch. Passing the nil through preserves the old semantics.
		// (row.Length is gone: upstream dropped the length argument and the
		// ErrFileLengthMismatch retry that consumed it.)
		data, err := dl.DownloadMediaWithPath(ctx, row.DirectPath,
			row.EncSHA256, row.SHA256, row.Key, mediaType, "", true)
		if err != nil {
			return nil, fmt.Errorf("download by direct path failed: %w", err)
		}
		return data, nil
	}

	if row.URL == "" || strings.HasPrefix(row.URL, "https://web.whatsapp.net") {
		return nil, errors.New("no usable media locator for this message: " + staleLocatorHint)
	}

	data, err := dl.Download(ctx, downloadableFor(row))
	if err != nil {
		return nil, fmt.Errorf("download by url failed: %w; %s", err, staleLocatorHint)
	}
	return data, nil
}

// mediaTypeForKind maps a cached message kind onto the whatsmeow media type
// whose key-derivation string decrypts it. Stickers ride the image type —
// that is how WhatsApp encrypts them.
func mediaTypeForKind(kind cache.MessageKind) (whatsmeow.MediaType, bool) {
	switch kind {
	case cache.KindImage, cache.KindSticker:
		return whatsmeow.MediaImage, true
	case cache.KindVideo:
		return whatsmeow.MediaVideo, true
	case cache.KindAudio:
		return whatsmeow.MediaAudio, true
	case cache.KindDocument:
		return whatsmeow.MediaDocument, true
	default:
		return "", false
	}
}

// downloadableFor rebuilds the protobuf sub-message whatsmeow's Download
// expects from the cached columns. Only the locator fields matter; Download
// derives the media type from the concrete protobuf type, which is why each
// kind gets its own struct rather than one generic carrier.
func downloadableFor(row cache.MediaRow) whatsmeow.DownloadableMessage {
	switch row.Kind {
	case cache.KindVideo:
		return &waE2E.VideoMessage{
			URL: proto.String(row.URL), DirectPath: proto.String(row.DirectPath),
			MediaKey: row.Key, FileSHA256: row.SHA256, FileEncSHA256: row.EncSHA256,
			FileLength: proto.Uint64(row.Length), Mimetype: proto.String(row.Mime),
		}
	case cache.KindAudio:
		return &waE2E.AudioMessage{
			URL: proto.String(row.URL), DirectPath: proto.String(row.DirectPath),
			MediaKey: row.Key, FileSHA256: row.SHA256, FileEncSHA256: row.EncSHA256,
			FileLength: proto.Uint64(row.Length), Mimetype: proto.String(row.Mime),
		}
	case cache.KindDocument:
		return &waE2E.DocumentMessage{
			URL: proto.String(row.URL), DirectPath: proto.String(row.DirectPath),
			MediaKey: row.Key, FileSHA256: row.SHA256, FileEncSHA256: row.EncSHA256,
			FileLength: proto.Uint64(row.Length), Mimetype: proto.String(row.Mime),
		}
	case cache.KindSticker:
		return &waE2E.StickerMessage{
			URL: proto.String(row.URL), DirectPath: proto.String(row.DirectPath),
			MediaKey: row.Key, FileSHA256: row.SHA256, FileEncSHA256: row.EncSHA256,
			FileLength: proto.Uint64(row.Length), Mimetype: proto.String(row.Mime),
		}
	default:
		return &waE2E.ImageMessage{
			URL: proto.String(row.URL), DirectPath: proto.String(row.DirectPath),
			MediaKey: row.Key, FileSHA256: row.SHA256, FileEncSHA256: row.EncSHA256,
			FileLength: proto.Uint64(row.Length), Mimetype: proto.String(row.Mime),
		}
	}
}

// downloadFilename picks the name the byte route will advertise. Only
// documents carry a real filename on the wire (the ingestor stores
// DocumentMessage.FileName and nothing else), so every other kind gets a
// synthesised "<kind>_<yyyymmdd>_<hhmmss><ext>" derived from the message
// timestamp — deterministic, so a repeat download yields the same name.
func downloadFilename(row cache.MediaRow) string {
	if row.Filename != "" {
		return media.SanitizeFilename(row.Filename)
	}
	stamp := row.Timestamp.UTC().Format("20060102_150405")
	if row.Timestamp.IsZero() {
		stamp = time.Now().UTC().Format("20060102_150405")
	}
	return fmt.Sprintf("%s_%s%s", row.Kind, stamp, media.ExtensionForMime(row.Mime))
}

func toDownloadResult(d media.Descriptor) DownloadMediaResult {
	return DownloadMediaResult{
		MediaPath: d.MediaPath,
		Mime:      d.Mime,
		Size:      d.Size,
		Filename:  d.Filename,
		SHA256:    d.SHA256,
	}
}
