package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/audio"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
)

var sendAudioMessageSchema = json.RawMessage(`{
  "type": "object",
  "required": ["recipient", "media_path"],
  "properties": {
    "recipient": {
      "type": "string",
      "description": "Destination chat: a JID ('user@s.whatsapp.net' or 'group@g.us') or a raw phone number with country code (digits only, no + or spaces)."
    },
    "media_path": {
      "type": "string",
      "description": "Reference to previously stored audio: the '/media/<sha256>' path returned by download_media, or by a 'POST /media' upload on the same port and bearer token as /mcp. A bare <sha256> is also accepted."
    },
    "reply_to_id": {
      "type": "string",
      "description": "Optional stanza id of the message to quote-reply to."
    }
  },
  "additionalProperties": false
}`)

// SendAudioResult is the structured output of send_audio_message. It extends
// the common media result with the two things a caller cannot infer: how
// long the voice note is, and whether the bytes had to be transcoded (in
// which case `sha256` points at the converted blob, not the uploaded one).
type SendAudioResult struct {
	SendMediaResult
	DurationSeconds uint32 `json:"duration_seconds"`
	Transcoded      bool   `json:"transcoded"`
	VoiceNote       bool   `json:"voice_note"`
}

// sendAudioMessage sends a voice note (PTT). Voice notes are the one
// envelope with a hard codec requirement: WhatsApp plays Ogg/Opus and
// nothing else, so this tool either has Opus already or makes it with
// ffmpeg. When neither is possible it refuses — sending an mp3 as a PTT
// produces a voice note that no recipient can play, and no error anyone
// would see.
func sendAudioMessage(deps Deps) mcp.Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Recipient string `json:"recipient"`
			MediaPath string `json:"media_path"`
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

		f, desc, errRes := openMediaArg(deps, in.MediaPath)
		if errRes != nil {
			return errRes, nil
		}
		defer func() { _ = f.Close() }()

		up := deps.uploader()
		if up == nil {
			return mcp.NotPairedError(), nil
		}

		opts := envelopeOpts{
			Mime:    audio.OpusMime,
			PTT:     true,
			Context: replyContext(in.ReplyToID, deps.WA.OwnJID()),
		}

		// The stored mimetype is a hint from whoever uploaded the bytes;
		// the magic number is the fact. Probe decides, so a mislabelled
		// upload is transcoded rather than sent as an unplayable note.
		info, err := audio.Probe(f)
		if err != nil {
			return mcp.InternalError(fmt.Sprintf("probe audio: %v", err)), nil
		}

		var (
			transcoded bool
			resp       whatsmeow.UploadResponse
		)
		if info.IsOggOpus {
			resp, err = up.UploadReader(ctx, f, nil, kindAudio.mediaType())
			if err != nil {
				return uploadFailure(kindAudio, err), nil
			}
		} else {
			opus, newDesc, errRes := transcodeToOpus(ctx, deps, f, desc)
			if errRes != nil {
				return errRes, nil
			}
			transcoded = true
			desc = newDesc
			if probed, err := audio.ProbeBytes(opus); err == nil {
				info = probed
			}
			resp, err = up.Upload(ctx, opus, kindAudio.mediaType())
			if err != nil {
				return uploadFailure(kindAudio, err), nil
			}
		}

		opts.Seconds = info.Seconds()
		opts.Filename = voiceNoteFilename(desc)

		sent, err := sendUploaded(ctx, deps, to, kindAudio, resp, opts, in.ReplyToID, desc)
		if err != nil {
			return nil, err
		}
		base, ok := sent.(SendMediaResult)
		if !ok {
			// sendUploaded returned a structured error result; pass it on
			// unchanged rather than wrapping it in a success shape.
			return sent, nil
		}
		return SendAudioResult{
			SendMediaResult: base,
			DurationSeconds: opts.Seconds,
			Transcoded:      transcoded,
			VoiceNote:       true,
		}, nil
	}
}

// voiceNoteFilename keeps the cached row's filename meaningful. A voice note
// carries no filename on the wire, but download_media reads this column when
// naming the blob it hands back.
func voiceNoteFilename(desc media.Descriptor) string {
	if desc.Filename != "" {
		return desc.Filename
	}
	return "voice.ogg"
}
