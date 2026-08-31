package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/audio"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

var sendFileSchema = json.RawMessage(`{
  "type": "object",
  "required": ["recipient", "media_path"],
  "properties": {
    "recipient": {
      "type": "string",
      "description": "Destination chat: a JID ('user@s.whatsapp.net' or 'group@g.us') or a raw phone number with country code (digits only, no + or spaces)."
    },
    "media_path": {
      "type": "string",
      "description": "Reference to previously stored bytes: the '/media/<sha256>' path returned by download_media, or by a 'POST /media' upload on the same port and bearer token as /mcp. A bare <sha256> is also accepted. This tool never accepts base64 or a local filesystem path."
    },
    "media_type": {
      "type": "string",
      "enum": ["auto", "image", "video", "audio", "document", "sticker"],
      "description": "Envelope to send. 'auto' (default) picks from the stored mimetype: image/* -> image, video/* -> video, audio/* -> audio, image/webp -> sticker, anything else -> document.",
      "default": "auto"
    },
    "caption": {
      "type": "string",
      "description": "Optional caption. Supported on image, video and document envelopes. Audio and sticker envelopes cannot carry one, so passing a caption with them is rejected rather than silently dropped."
    },
    "filename": {
      "type": "string",
      "description": "Optional filename shown to the recipient on a document send. Defaults to the name recorded when the bytes were stored."
    },
    "reply_to_id": {
      "type": "string",
      "description": "Optional stanza id of the message to quote-reply to."
    }
  },
  "additionalProperties": false
}`)

// playableAudioMimes are the container/codec combinations WhatsApp plays
// in-app as an audio attachment. Anything else has to be transcoded (see
// prepareAudioAttachment) or refused — an unplayable attachment that
// silently "sent fine" is the worst outcome for the caller.
var playableAudioMimes = map[string]struct{}{
	"audio/ogg":  {},
	"audio/opus": {},
	"audio/mpeg": {},
	"audio/mp4":  {},
	"audio/aac":  {},
	"audio/amr":  {},
}

// sendFile uploads previously stored bytes and sends them as a media
// message. It is the general media send: image, video, audio attachment,
// document and sticker envelopes all come from here. Voice notes are
// deliberately elsewhere (send_audio_message) because PTT has a stricter
// codec contract.
func sendFile(deps Deps) mcp.Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Recipient string `json:"recipient"`
			MediaPath string `json:"media_path"`
			MediaType string `json:"media_type,omitempty"`
			Caption   string `json:"caption,omitempty"`
			Filename  string `json:"filename,omitempty"`
			ReplyToID string `json:"reply_to_id,omitempty"`
		}
		if err := decodeArgs(raw, &in); err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}
		if strings.TrimSpace(in.Recipient) == "" {
			return mcp.InvalidArgumentError("recipient must not be empty"), nil
		}
		to, err := resolveRecipient(strings.TrimSpace(in.Recipient))
		if err != nil {
			return mcp.InvalidArgumentError(err.Error()), nil
		}

		// Argument validation first, so a typo in media_type is reported as
		// such rather than as whatever the store says about media_path.
		requested, errRes := parseMediaType(in.MediaType)
		if errRes != nil {
			return errRes, nil
		}

		f, desc, errRes := openMediaArg(deps, in.MediaPath)
		if errRes != nil {
			return errRes, nil
		}
		defer func() { _ = f.Close() }()

		kind := requested
		if kind == "" {
			kind = detectMediaKind(desc.Mime)
		}
		if errRes := validateKind(kind, desc.Mime); errRes != nil {
			return errRes, nil
		}

		up := deps.uploader()
		if up == nil {
			return mcp.NotPairedError(), nil
		}

		opts := envelopeOpts{
			Mime:     desc.Mime,
			Caption:  in.Caption,
			Filename: outboundFilename(in.Filename, desc),
			Context:  replyContext(in.ReplyToID, deps.WA.OwnJID()),
		}
		// Captions do not exist on audio or sticker envelopes; dropping one
		// silently would lose the caller's text with no trace, so refuse.
		if opts.Caption != "" && (kind == kindAudio || kind == kindSticker) {
			return mcp.InvalidArgumentError(fmt.Sprintf(
				"a %s message cannot carry a caption; send the text as a separate send_message call", kind)), nil
		}

		switch kind {
		case kindImage, kindSticker:
			opts.Width, opts.Height = imageDimensions(f)
			if kind == kindSticker {
				opts.Animated = isAnimatedWebP(f)
			}
		case kindAudio:
			// The audio path may replace both the bytes and the mimetype,
			// so it owns the whole upload.
			return sendAudioAttachment(ctx, deps, up, sendAudioParams{
				to: to, file: f, desc: desc, opts: opts, replyTo: in.ReplyToID,
			})
		}

		resp, err := up.UploadReader(ctx, f, nil, kind.mediaType())
		if err != nil {
			return uploadFailure(kind, err), nil
		}
		return sendUploaded(ctx, deps, to, kind, resp, opts, in.ReplyToID, desc)
	}
}

// sendAudioAttachment sends a non-PTT audio message. WhatsApp only plays a
// known set of codecs (playableAudioMimes); anything else is transcoded to
// Opus when ffmpeg is available and refused when it is not, so a caller
// never gets a "sent" result for bytes that will not play.
func sendAudioAttachment(ctx context.Context, deps Deps, up MediaUploader, p sendAudioParams) (any, error) {
	payload, errRes := prepareAudio(ctx, deps, p.file, p.desc, func(_ audio.Info, d media.Descriptor) bool {
		_, ok := playableAudioMimes[baseMime(d.Mime)]
		return ok
	})
	if errRes != nil {
		return errRes, nil
	}
	p.opts.Mime, p.opts.Seconds = payload.Mime, payload.Seconds
	if payload.Transcoded {
		// The row should name what was sent, not the .wav that went in.
		p.opts.Filename = payload.Desc.Filename
	}

	resp, err := uploadAudio(ctx, up, p.file, payload)
	if err != nil {
		return uploadFailure(kindAudio, err), nil
	}
	return sendUploaded(ctx, deps, p.to, kindAudio, resp, p.opts, p.replyTo, payload.Desc)
}

// sendAudioParams bundles what an audio send needs beyond the deps and the
// uploader. It exists only to keep that call under the argument count a
// reader can hold.
type sendAudioParams struct {
	to      types.JID
	file    *os.File
	desc    media.Descriptor
	opts    envelopeOpts
	replyTo string
}

// transcodeToOpus converts the blob behind f to Ogg/Opus and stores the
// result, returning the new bytes and their descriptor. Storing the output
// is what keeps the send self-consistent: the cached message row points at
// the bytes that were actually sent, so download_media on our own message
// is a cache hit rather than a CDN round trip.
func transcodeToOpus(ctx context.Context, deps Deps, f *os.File, in media.Descriptor) ([]byte, media.Descriptor, *mcpgo.CallToolResult) {
	if deps.Audio == nil || !deps.Audio.Available() {
		return nil, media.Descriptor{}, mcp.InvalidArgumentError(ffmpegMissingMessage(deps, in.Mime))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, media.Descriptor{}, mcp.InternalError(fmt.Sprintf("rewind media: %v", err))
	}
	opus, err := deps.Audio.ToOpus(ctx, f)
	if err != nil {
		if errors.Is(err, audio.ErrUnavailable) {
			return nil, media.Descriptor{}, mcp.InvalidArgumentError(ffmpegMissingMessage(deps, in.Mime))
		}
		return nil, media.Descriptor{}, mcp.InternalError(fmt.Sprintf(
			"transcoding %s to Opus failed: %v", in.Mime, err))
	}
	desc, err := deps.Media.Put(opus, audio.OpusMime, opusFilename(in.Filename))
	if err != nil {
		return nil, media.Descriptor{}, mcp.InternalError(fmt.Sprintf("store transcoded audio: %v", err))
	}
	return opus, desc, nil
}

// ffmpegMissingMessage explains the two-sided FFMPEG_PATH contract at the
// point it bites, naming the path that was probed and the way out.
func ffmpegMissingMessage(deps Deps, mimeType string) string {
	probed := "FFMPEG_PATH"
	if deps.Audio != nil && deps.Audio.Path() != "" {
		probed = fmt.Sprintf("FFMPEG_PATH=%s", deps.Audio.Path())
	}
	return fmt.Sprintf(
		"this audio is %s, which WhatsApp will not play, and no ffmpeg binary was found (%s) to "+
			"transcode it. Upload Ogg/Opus audio instead, or run the -slim image variant, which ships ffmpeg",
		mimeType, probed)
}

func uploadFailure(kind mediaKind, err error) *mcpgo.CallToolResult {
	if errors.Is(err, wa.ErrNotLoggedIn) {
		return mcp.NotConnectedError()
	}
	return mcp.ErrorResult(mcp.ErrMediaUnavailable,
		fmt.Sprintf("upload %s to the WhatsApp CDN failed: %v", kind, err))
}

// parseMediaType maps the media_type argument onto an envelope. The empty
// kind means "auto": the caller left the choice to detectMediaKind, which
// needs the stored mimetype and therefore cannot run this early.
func parseMediaType(requested string) (mediaKind, *mcpgo.CallToolResult) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", "auto":
		return "", nil
	case string(kindImage):
		return kindImage, nil
	case string(kindVideo):
		return kindVideo, nil
	case string(kindAudio):
		return kindAudio, nil
	case string(kindDocument):
		return kindDocument, nil
	case string(kindSticker):
		return kindSticker, nil
	default:
		return "", mcp.InvalidArgumentError(fmt.Sprintf(
			"media_type %q is not one of auto, image, video, audio, document, sticker", requested))
	}
}

// detectMediaKind picks an envelope from a mimetype.
//
// image/webp maps to sticker rather than image because that is what WhatsApp
// itself does with a .webp — sending one as an ImageMessage produces a
// broken attachment on the recipient's phone. Callers who really do want a
// WebP photo pass media_type:"image" explicitly.
func detectMediaKind(mimeType string) mediaKind {
	base := baseMime(mimeType)
	switch {
	case base == "image/webp":
		return kindSticker
	case strings.HasPrefix(base, "image/"):
		return kindImage
	case strings.HasPrefix(base, "video/"):
		return kindVideo
	case strings.HasPrefix(base, "audio/"):
		return kindAudio
	default:
		return kindDocument
	}
}

// validateKind rejects the combinations WhatsApp will accept over the wire
// and then fail to render, which is indistinguishable from data loss for the
// caller. Only sticker is genuinely format-locked; the rest are free-form.
func validateKind(kind mediaKind, mimeType string) *mcpgo.CallToolResult {
	if kind == kindSticker && baseMime(mimeType) != "image/webp" {
		return mcp.InvalidArgumentError(fmt.Sprintf(
			"stickers must be WebP; the stored media is %s. Convert it to WebP before uploading, "+
				"or send it with media_type:\"image\"", mimeType))
	}
	return nil
}

// outboundFilename picks the name a document send advertises: the caller's
// override, else the name recorded when the bytes were stored.
func outboundFilename(override string, desc media.Descriptor) string {
	if s := strings.TrimSpace(override); s != "" {
		return media.SanitizeFilename(s)
	}
	return desc.Filename
}

// opusFilename derives a name for a transcoded blob from the original.
func opusFilename(original string) string {
	base := strings.TrimSpace(original)
	if base == "" {
		return "voice.ogg"
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return media.SanitizeFilename(base + ".ogg")
}

// baseMime strips parameters ("audio/ogg; codecs=opus" -> "audio/ogg") and
// lower-cases the result.
func baseMime(mimeType string) string {
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil {
		return strings.ToLower(parsed)
	}
	base, _, _ := strings.Cut(mimeType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// isAnimatedWebP reports whether f is a WebP carrying an animation chunk.
// WhatsApp needs the flag on the envelope to play an animated sticker
// instead of showing its first frame, and the chunk id sits in the first few
// dozen bytes of the RIFF container.
func isAnimatedWebP(f *os.File) bool {
	defer func() { _, _ = f.Seek(0, io.SeekStart) }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	head := make([]byte, 64)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false
	}
	head = head[:n]
	if len(head) < 16 || string(head[:4]) != "RIFF" || string(head[8:12]) != "WEBP" {
		return false
	}
	return bytes.Contains(head, []byte("ANIM")) || bytes.Contains(head, []byte("ANMF"))
}
