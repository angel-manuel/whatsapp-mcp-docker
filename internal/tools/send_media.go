package tools

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"strings"
	"time"

	// Registered for their DecodeConfig side effect only: outbound images
	// advertise their pixel dimensions so recipients can lay out a
	// placeholder before the bytes arrive. Formats not listed here (webp)
	// simply go out without dimensions.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/audio"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

// mediaKind is the outbound envelope a blob is wrapped in. It is the send
// side of cache.MessageKind and maps 1:1 onto both a waE2E message type and
// a whatsmeow.MediaType (the key-derivation string the CDN object is
// encrypted with).
type mediaKind string

const (
	kindImage    mediaKind = "image"
	kindVideo    mediaKind = "video"
	kindAudio    mediaKind = "audio"
	kindDocument mediaKind = "document"
	kindSticker  mediaKind = "sticker"
)

// mediaType maps an outbound envelope onto whatsmeow's upload media type.
// Stickers ride the image type — that is how WhatsApp encrypts them, and it
// mirrors mediaTypeForKind on the download side.
func (k mediaKind) mediaType() whatsmeow.MediaType {
	switch k {
	case kindVideo:
		return whatsmeow.MediaVideo
	case kindAudio:
		return whatsmeow.MediaAudio
	case kindDocument:
		return whatsmeow.MediaDocument
	default: // image, sticker
		return whatsmeow.MediaImage
	}
}

// cacheKind maps an outbound envelope onto the cached message kind, so a
// message this server sent is indistinguishable from an inbound one to
// list_messages and download_media.
func (k mediaKind) cacheKind() cache.MessageKind {
	switch k {
	case kindVideo:
		return cache.KindVideo
	case kindAudio:
		return cache.KindAudio
	case kindDocument:
		return cache.KindDocument
	case kindSticker:
		return cache.KindSticker
	default:
		return cache.KindImage
	}
}

// SendMediaResult is the structured output of the media send tools. It
// echoes what was actually sent rather than what was asked for: `mime` and
// `sha256` describe the bytes on the wire, which for a transcoded voice note
// are not the bytes that were uploaded.
type SendMediaResult struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	SentTS    int64  `json:"sent_ts"`
	MediaType string `json:"media_type"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

// mediaDigestFromArg resolves whatever a caller passed as `media_path` to a
// stored digest. All of these are accepted, because all of them are things
// a caller plausibly has in hand:
//
//	/media/<sha256>                     what download_media returned
//	https://gateway.example/media/<..>  the same, as a gateway URL
//	<sha256>                            the bare digest
func mediaDigestFromArg(in string) (string, bool) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", false
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	// A descriptor path never carries an extension, but a caller who
	// hand-built one from an on-disk filename might.
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return media.NormalizeDigest(s)
}

// openMediaArg resolves media_path and opens the stored blob. The returned
// file is the caller's to close. A non-nil *mcpgo.CallToolResult is a ready
// structured error the handler should return as-is.
func openMediaArg(deps Deps, arg string) (*os.File, media.Descriptor, *mcpgo.CallToolResult) {
	if deps.Media == nil {
		return nil, media.Descriptor{}, mcp.InternalError("media store is not configured")
	}
	digest, ok := mediaDigestFromArg(arg)
	if !ok {
		return nil, media.Descriptor{}, mcp.InvalidArgumentError(fmt.Sprintf(
			"media_path %q is not a stored media reference; pass the media_path a "+
				"download_media call returned, or the /media/<sha256> path a POST /media upload answered with",
			arg))
	}
	f, desc, _, err := deps.Media.Open(digest)
	if err != nil {
		if errors.Is(err, media.ErrNotFound) {
			return nil, media.Descriptor{}, mcp.NotFoundError(fmt.Sprintf(
				"no stored media for %s; upload the bytes with POST /media (same port and bearer "+
					"token as /mcp) and pass the media_path it returns. Stored media is also evicted "+
					"by the retention sweep (MEDIA_TTL / MEDIA_MAX_BYTES)", digest))
		}
		return nil, media.Descriptor{}, mcp.InternalError(err.Error())
	}
	return f, desc, nil
}

// envelopeOpts carries everything the protobuf builders need that does not
// come from the upload response itself.
type envelopeOpts struct {
	Mime     string
	Caption  string
	Filename string
	Context  *waE2E.ContextInfo
	// Seconds is the audio duration; zero omits it.
	Seconds uint32
	// PTT marks an audio envelope as a voice note rather than an
	// attachment. Ignored for every other kind.
	PTT bool
	// Width and Height are image dimensions; zero omits them.
	Width, Height uint32
	// Animated marks a sticker as animated (WebP with an ANIM chunk).
	Animated bool
}

// buildMediaEnvelope wraps an upload response in the protobuf message for
// kind. The locator fields are identical across every envelope type —
// whatsmeow derives the media type from the concrete struct, which is why
// each kind needs its own rather than one generic carrier.
func buildMediaEnvelope(kind mediaKind, up whatsmeow.UploadResponse, opts envelopeOpts) *waE2E.Message {
	switch kind {
	case kindVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(opts.Mime),
			Caption: optionalString(opts.Caption), ContextInfo: opts.Context,
		}}
	case kindAudio:
		msg := &waE2E.AudioMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(opts.Mime),
			ContextInfo: opts.Context,
		}
		if opts.PTT {
			msg.PTT = proto.Bool(true)
		}
		if opts.Seconds > 0 {
			msg.Seconds = proto.Uint32(opts.Seconds)
		}
		return &waE2E.Message{AudioMessage: msg}
	case kindDocument:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(opts.Mime),
			FileName: proto.String(opts.Filename), Title: optionalString(opts.Filename),
			Caption: optionalString(opts.Caption), ContextInfo: opts.Context,
		}}
	case kindSticker:
		msg := &waE2E.StickerMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(opts.Mime),
			ContextInfo: opts.Context,
		}
		if opts.Animated {
			msg.IsAnimated = proto.Bool(true)
		}
		if opts.Width > 0 && opts.Height > 0 {
			msg.Width, msg.Height = proto.Uint32(opts.Width), proto.Uint32(opts.Height)
		}
		return &waE2E.Message{StickerMessage: msg}
	default:
		msg := &waE2E.ImageMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(opts.Mime),
			Caption: optionalString(opts.Caption), ContextInfo: opts.Context,
		}
		if opts.Width > 0 && opts.Height > 0 {
			msg.Width, msg.Height = proto.Uint32(opts.Width), proto.Uint32(opts.Height)
		}
		return &waE2E.Message{ImageMessage: msg}
	}
}

// optionalString returns nil for the empty string, so an unset caption is
// absent from the protobuf rather than present-and-empty.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// sendUploaded is the tail shared by every media send: hand the envelope to
// whatsmeow, mirror the result into the cache so the message is visible to
// list_messages and downloadable by download_media, and shape the result.
func sendUploaded(ctx context.Context, deps Deps, to types.JID, kind mediaKind,
	up whatsmeow.UploadResponse, opts envelopeOpts, replyTo string, desc media.Descriptor,
) (any, error) {
	msg := buildMediaEnvelope(kind, up, opts)

	resp, err := deps.WA.SendMessage(ctx, to, msg)
	if err != nil {
		if errors.Is(err, wa.ErrNotLoggedIn) {
			return mcp.NotConnectedError(), nil
		}
		return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("send %s message: %v", kind, err)), nil
	}

	ts := resp.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	chatJID := to.String()

	row := cache.Message{
		ID:        string(resp.ID),
		ChatJID:   chatJID,
		SenderJID: ownSenderJID(deps.WA.OwnJID()),
		Timestamp: ts,
		Kind:      kind.cacheKind(),
		Body:      opts.Caption,
		ReplyToID: replyTo,
		IsFromMe:  true,
		Media: &cache.Media{
			Mime:       opts.Mime,
			Filename:   opts.Filename,
			URL:        up.URL,
			DirectPath: up.DirectPath,
			Key:        up.MediaKey,
			SHA256:     up.FileSHA256,
			EncSHA256:  up.FileEncSHA256,
			Length:     up.FileLength,
		},
	}
	if err := mirrorOutboundRow(ctx, deps.Cache, to, row); err != nil {
		return mcp.ErrorResult(mcp.ErrInternal, fmt.Sprintf("cache outbound: %v", err)), nil
	}

	return SendMediaResult{
		MessageID: string(resp.ID),
		ChatJID:   chatJID,
		SentTS:    ts.Unix(),
		MediaType: string(kind),
		Mime:      opts.Mime,
		Size:      desc.Size,
		SHA256:    desc.SHA256,
	}, nil
}

// imageDimensions decodes just the header of an image to read its pixel
// size, then rewinds f so the very same handle can be uploaded. Failure is
// not an error: dimensions are a rendering hint, and WebP (the sticker
// format) is not decodable by the standard library at all.
func imageDimensions(f *os.File) (width, height uint32) {
	defer func() { _, _ = f.Seek(0, io.SeekStart) }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	return uint32(cfg.Width), uint32(cfg.Height)
}

// audioPayload is what an audio send ended up putting on the wire, after the
// codec rules in prepareAudio had their say. It exists because both audio
// tools need the same three answers — which bytes, described how, and was a
// conversion involved — and only differ in what they do with them.
type audioPayload struct {
	// opus holds the converted bytes when Transcoded; the stored blob is
	// streamed straight off disk otherwise.
	opus []byte
	// Desc describes the bytes actually sent, which after a transcode is a
	// different blob than the caller referenced.
	Desc media.Descriptor
	// Mime and Seconds go onto the envelope.
	Mime    string
	Seconds uint32
	// Transcoded reports whether ffmpeg ran.
	Transcoded bool
}

// prepareAudio resolves which bytes an audio send should upload. The two
// audio tools differ only in what counts as already-sendable — a voice note
// demands Opus, an attachment accepts anything WhatsApp plays — so that test
// is the caller's, passed in as sendable, and everything downstream of it is
// shared.
//
// Bytes that are not sendable as they stand are transcoded when ffmpeg is
// available and refused when it is not (see transcodeToOpus): the one thing
// neither tool may do is upload audio the recipient cannot play.
func prepareAudio(ctx context.Context, deps Deps, f *os.File, desc media.Descriptor,
	sendable func(audio.Info, media.Descriptor) bool,
) (audioPayload, *mcpgo.CallToolResult) {
	// The stored mimetype is a hint from whoever uploaded the bytes; the
	// magic number is the fact. A probe failure here is a failure to read
	// the blob at all, which is fatal for both tools — never a silently
	// duration-less send.
	info, err := audio.Probe(f)
	if err != nil {
		return audioPayload{}, mcp.InternalError(fmt.Sprintf("probe audio %s: %v", desc.SHA256, err))
	}

	if sendable(info, desc) {
		return audioPayload{Desc: desc, Mime: desc.Mime, Seconds: info.Seconds()}, nil
	}

	opus, converted, errRes := transcodeToOpus(ctx, deps, f, desc)
	if errRes != nil {
		return audioPayload{}, errRes
	}
	out := audioPayload{opus: opus, Desc: converted, Mime: audio.OpusMime, Transcoded: true}
	if probed, err := audio.ProbeBytes(opus); err == nil {
		out.Seconds = probed.Seconds()
	}
	return out, nil
}

// uploadAudio pushes the prepared payload to the CDN, picking the entry
// point that matches where the bytes are: transcoded audio is already in
// memory, while a stored blob streams off disk.
func uploadAudio(ctx context.Context, up MediaUploader, f *os.File, p audioPayload) (whatsmeow.UploadResponse, error) {
	if p.Transcoded {
		return up.Upload(ctx, p.opus, kindAudio.mediaType())
	}
	return up.UploadReader(ctx, f, nil, kindAudio.mediaType())
}
