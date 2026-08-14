package tools_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/media"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
)

// fakeUploader stands in for whatsmeow's media upload. It records which of
// the two entry points was taken — send_file streams a stored blob through
// UploadReader, the audio path uploads transcoded bytes from memory — and
// derives the response the way whatsmeow does, so the plaintext digest the
// cache row ends up carrying is the digest of what was actually sent.
type fakeUploader struct {
	err error

	uploadCalls int
	readerCalls int
	lastType    whatsmeow.MediaType
	lastBytes   []byte
}

func (f *fakeUploader) Upload(_ context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploadCalls++
	f.lastType = appInfo
	f.lastBytes = append([]byte(nil), plaintext...)
	return f.respond(plaintext)
}

func (f *fakeUploader) UploadReader(_ context.Context, plaintext io.Reader, _ io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.readerCalls++
	f.lastType = appInfo
	data, err := io.ReadAll(plaintext)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}
	f.lastBytes = data
	return f.respond(data)
}

func (f *fakeUploader) respond(data []byte) (whatsmeow.UploadResponse, error) {
	if f.err != nil {
		return whatsmeow.UploadResponse{}, f.err
	}
	sum := sha256.Sum256(data)
	encSum := sha256.Sum256(append([]byte("enc"), data...))
	return whatsmeow.UploadResponse{
		URL:           "https://mmg.whatsapp.net/upload",
		DirectPath:    "/v/t62/uploaded",
		MediaKey:      []byte{0xAA, 0xBB, 0xCC},
		FileSHA256:    sum[:],
		FileEncSHA256: encSum[:],
		FileLength:    uint64(len(data)),
	}, nil
}

var _ tools.MediaUploader = (*fakeUploader)(nil)

// fakeTranscoder drives both sides of the FFMPEG_PATH contract without an
// ffmpeg binary: available=false is the distroless image, available=true is
// the -slim one.
type fakeTranscoder struct {
	available bool
	out       []byte
	err       error

	calls  int
	lastIn []byte
}

func (f *fakeTranscoder) Available() bool { return f.available }
func (f *fakeTranscoder) Path() string    { return "/usr/bin/ffmpeg" }

func (f *fakeTranscoder) ToOpus(_ context.Context, in io.Reader) ([]byte, error) {
	f.calls++
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	f.lastIn = data
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

var _ tools.AudioTranscoder = (*fakeTranscoder)(nil)

const sendChatJID = "34600111222@s.whatsapp.net"

// mediaHarness wires a paired harness with a fake uploader (and optionally a
// fake transcoder) and stores payload in the media store, returning the
// descriptor a caller would have received from POST /media.
type mediaHarness struct {
	*testHarness
	up    *fakeUploader
	audio *fakeTranscoder
	desc  media.Descriptor
}

func newMediaHarness(t *testing.T, payload []byte, mime, filename string, audio *fakeTranscoder) *mediaHarness {
	t.Helper()
	up := &fakeUploader{}
	h := newHarnessWithDeps(t, true, nil,
		&mockWA{sendResp: whatsmeow.SendResponse{ID: "wamid.SENT"}},
		func(d *tools.Deps) {
			d.Uploader = up
			if audio != nil {
				d.Audio = audio
			}
		})
	desc, err := h.media.Put(payload, mime, filename)
	if err != nil {
		t.Fatalf("seed media: %v", err)
	}
	return &mediaHarness{testHarness: h, up: up, audio: audio, desc: desc}
}

// pngBytes encodes a w×h PNG, which is enough for the dimension probe.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 1, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestSendFile_ImageUploadsAndSends(t *testing.T) {
	payload := pngBytes(t, 24, 12)
	h := newMediaHarness(t, payload, "image/png", "shot.png", nil)

	res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
		"caption":    "look at this",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	out := structured(t, res)

	if got := out["media_type"]; got != "image" {
		t.Errorf("media_type = %v, want image", got)
	}
	if got := out["message_id"]; got != "wamid.SENT" {
		t.Errorf("message_id = %v", got)
	}
	if got := out["sha256"]; got != h.desc.SHA256 {
		t.Errorf("sha256 = %v, want %v", got, h.desc.SHA256)
	}
	if got := out["chat_jid"]; got != sendChatJID {
		t.Errorf("chat_jid = %v", got)
	}

	if h.up.readerCalls != 1 || h.up.uploadCalls != 0 {
		t.Errorf("upload calls: reader=%d upload=%d, want the streaming path",
			h.up.readerCalls, h.up.uploadCalls)
	}
	if h.up.lastType != whatsmeow.MediaImage {
		t.Errorf("upload media type = %q, want %q", h.up.lastType, whatsmeow.MediaImage)
	}
	if !bytes.Equal(h.up.lastBytes, payload) {
		t.Error("uploaded bytes differ from the stored blob")
	}

	img := h.mock.lastSendMs.GetImageMessage()
	if img == nil {
		t.Fatalf("sent message is not an ImageMessage: %+v", h.mock.lastSendMs)
	}
	if img.GetCaption() != "look at this" {
		t.Errorf("caption = %q", img.GetCaption())
	}
	if img.GetMimetype() != "image/png" {
		t.Errorf("mimetype = %q", img.GetMimetype())
	}
	if img.GetDirectPath() != "/v/t62/uploaded" || img.GetURL() != "https://mmg.whatsapp.net/upload" {
		t.Errorf("locator not copied from the upload response: %+v", img)
	}
	if img.GetWidth() != 24 || img.GetHeight() != 12 {
		t.Errorf("dimensions = %dx%d, want 24x12", img.GetWidth(), img.GetHeight())
	}
	if img.GetContextInfo() != nil {
		t.Errorf("unexpected ContextInfo on a non-reply: %+v", img.GetContextInfo())
	}
}

// A sent attachment must be as retrievable as a received one: the mirrored
// row carries the plaintext digest, so download_media resolves it straight
// out of the store without a CDN round trip.
func TestSendFile_MirrorsIntoCacheForDownloadMedia(t *testing.T) {
	payload := []byte("%PDF-1.7 a report")
	h := newMediaHarness(t, payload, "application/pdf", "report.pdf", nil)

	sent := structured(t, callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	}))

	res := callTool(t, h.testHarness, "download_media", map[string]any{
		"chat_jid":   sendChatJID,
		"message_id": sent["message_id"],
	})
	if res.IsError {
		t.Fatalf("download_media on our own send failed: %+v", structured(t, res))
	}
	out := structured(t, res)
	if out["media_path"] != h.desc.MediaPath {
		t.Errorf("media_path = %v, want the stored %v", out["media_path"], h.desc.MediaPath)
	}
	if out["filename"] != "report.pdf" {
		t.Errorf("filename = %v, want report.pdf", out["filename"])
	}
}

func TestSendFile_DetectsEnvelopeFromMime(t *testing.T) {
	cases := []struct {
		name     string
		mime     string
		payload  []byte
		want     string
		wantType whatsmeow.MediaType
		check    func(*testing.T, *waE2E.Message)
	}{
		{
			name: "video", mime: "video/mp4", payload: []byte("ftypmp42"),
			want: "video", wantType: whatsmeow.MediaVideo,
			check: func(t *testing.T, m *waE2E.Message) {
				if m.GetVideoMessage() == nil {
					t.Errorf("not a VideoMessage: %+v", m)
				}
			},
		},
		{
			name: "document", mime: "application/pdf", payload: []byte("%PDF-1.7"),
			want: "document", wantType: whatsmeow.MediaDocument,
			check: func(t *testing.T, m *waE2E.Message) {
				doc := m.GetDocumentMessage()
				if doc == nil {
					t.Fatalf("not a DocumentMessage: %+v", m)
				}
				if doc.GetFileName() != "thing.pdf" {
					t.Errorf("file name = %q, want thing.pdf", doc.GetFileName())
				}
			},
		},
		{
			// WhatsApp treats a .webp as a sticker, so auto-detection has
			// to as well — an ImageMessage carrying WebP renders broken.
			name: "webp becomes a sticker", mime: "image/webp", payload: []byte("RIFF????WEBPVP8 data"),
			want: "sticker", wantType: whatsmeow.MediaImage,
			check: func(t *testing.T, m *waE2E.Message) {
				if m.GetStickerMessage() == nil {
					t.Errorf("not a StickerMessage: %+v", m)
				}
			},
		},
		{
			name: "unknown becomes a document", mime: "application/x-tar", payload: []byte("tarball"),
			want: "document", wantType: whatsmeow.MediaDocument,
			check: func(t *testing.T, m *waE2E.Message) {
				if m.GetDocumentMessage() == nil {
					t.Errorf("not a DocumentMessage: %+v", m)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMediaHarness(t, tc.payload, tc.mime, "thing.pdf", nil)
			out := structured(t, callTool(t, h.testHarness, "send_file", map[string]any{
				"recipient":  sendChatJID,
				"media_path": h.desc.MediaPath,
			}))
			if got := out["media_type"]; got != tc.want {
				t.Errorf("media_type = %v, want %v", got, tc.want)
			}
			if h.up.lastType != tc.wantType {
				t.Errorf("upload media type = %q, want %q", h.up.lastType, tc.wantType)
			}
			tc.check(t, h.mock.lastSendMs)
		})
	}
}

func TestSendFile_ExplicitMediaTypeOverridesDetection(t *testing.T) {
	h := newMediaHarness(t, pngBytes(t, 4, 4), "image/png", "shot.png", nil)

	out := structured(t, callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
		"media_type": "document",
		"filename":   "renamed.png",
	}))
	if got := out["media_type"]; got != "document" {
		t.Fatalf("media_type = %v, want document", got)
	}
	doc := h.mock.lastSendMs.GetDocumentMessage()
	if doc == nil {
		t.Fatalf("not a DocumentMessage: %+v", h.mock.lastSendMs)
	}
	if doc.GetFileName() != "renamed.png" {
		t.Errorf("file name = %q, want the override renamed.png", doc.GetFileName())
	}
}

func TestSendFile_StickerMustBeWebP(t *testing.T) {
	h := newMediaHarness(t, pngBytes(t, 8, 8), "image/png", "shot.png", nil)

	res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
		"media_type": "sticker",
	})
	out := expectError(t, res, mcp.ErrInvalidArgument)
	if msg, _ := out["message"].(string); !strings.Contains(msg, "WebP") {
		t.Errorf("message = %q, want it to name the required format", msg)
	}
	if h.up.readerCalls+h.up.uploadCalls != 0 {
		t.Error("refused send still uploaded")
	}
	if h.mock.sendCalls != 0 {
		t.Error("refused send still called SendMessage")
	}
}

func TestSendFile_AnimatedStickerIsFlagged(t *testing.T) {
	// RIFF container advertising WEBP with an animation chunk.
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8XANIM....ANMF....")
	h := newMediaHarness(t, webp, "image/webp", "wave.webp", nil)

	if res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	}); res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	sticker := h.mock.lastSendMs.GetStickerMessage()
	if sticker == nil {
		t.Fatalf("not a StickerMessage: %+v", h.mock.lastSendMs)
	}
	if !sticker.GetIsAnimated() {
		t.Error("IsAnimated = false, want true for a WebP with an ANIM chunk")
	}
}

func TestSendFile_CaptionRejectedWhereTheEnvelopeHasNone(t *testing.T) {
	h := newMediaHarness(t, []byte("RIFF????WEBPVP8 x"), "image/webp", "s.webp", nil)

	res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
		"caption":    "this text would vanish",
	})
	out := expectError(t, res, mcp.ErrInvalidArgument)
	if msg, _ := out["message"].(string); !strings.Contains(msg, "caption") {
		t.Errorf("message = %q, want it to explain the caption", msg)
	}
}

func TestSendFile_QuoteReply(t *testing.T) {
	h := newMediaHarness(t, pngBytes(t, 4, 4), "image/png", "shot.png", nil)
	own := types.NewJID("34699000111", types.DefaultUserServer)
	own.Device = 3
	h.mock.ownJID = own

	if res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":   sendChatJID,
		"media_path":  h.desc.MediaPath,
		"reply_to_id": "wamid.QUOTED",
	}); res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}

	ci := h.mock.lastSendMs.GetImageMessage().GetContextInfo()
	if ci == nil {
		t.Fatal("reply carried no ContextInfo")
	}
	if ci.GetStanzaID() != "wamid.QUOTED" {
		t.Errorf("stanza id = %q", ci.GetStanzaID())
	}
	if ci.GetParticipant() != "34699000111@s.whatsapp.net" {
		t.Errorf("participant = %q, want our own non-AD JID", ci.GetParticipant())
	}
}

func TestSendFile_AcceptsEveryMediaReferenceForm(t *testing.T) {
	payload := pngBytes(t, 4, 4)
	digest := hex.EncodeToString(func() []byte { s := sha256.Sum256(payload); return s[:] }())

	for name, ref := range map[string]string{
		"descriptor path": "/media/" + digest,
		"bare digest":     digest,
		"gateway url":     "https://gw.example.test/media/" + digest + "?token=x",
	} {
		t.Run(name, func(t *testing.T) {
			h := newMediaHarness(t, payload, "image/png", "shot.png", nil)
			if res := callTool(t, h.testHarness, "send_file", map[string]any{
				"recipient":  sendChatJID,
				"media_path": ref,
			}); res.IsError {
				t.Fatalf("%s was rejected: %+v", name, structured(t, res))
			}
		})
	}
}

func TestSendFile_ArgumentErrors(t *testing.T) {
	unknown := strings.Repeat("ab", 32)
	cases := map[string]struct {
		args     map[string]any
		wantCode mcp.ErrorCode
		wantText string
	}{
		"missing recipient": {
			args:     map[string]any{"media_path": "/media/" + unknown},
			wantCode: mcp.ErrInvalidArgument, wantText: "recipient",
		},
		"malformed media_path": {
			args:     map[string]any{"recipient": sendChatJID, "media_path": "/etc/passwd"},
			wantCode: mcp.ErrInvalidArgument, wantText: "media_path",
		},
		"unknown digest": {
			args:     map[string]any{"recipient": sendChatJID, "media_path": "/media/" + unknown},
			wantCode: mcp.ErrNotFound, wantText: "POST /media",
		},
		"bad media_type": {
			args:     map[string]any{"recipient": sendChatJID, "media_path": "/media/" + unknown, "media_type": "gif"},
			wantCode: mcp.ErrInvalidArgument, wantText: "media_type",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newMediaHarness(t, []byte("payload"), "application/pdf", "a.pdf", nil)
			out := expectError(t, callTool(t, h.testHarness, "send_file", tc.args), tc.wantCode)
			if msg, _ := out["message"].(string); !strings.Contains(msg, tc.wantText) {
				t.Errorf("message = %q, want it to mention %q", msg, tc.wantText)
			}
			if h.mock.sendCalls != 0 {
				t.Error("rejected call still reached SendMessage")
			}
		})
	}
}

func TestSendFile_UploadFailureIsMediaUnavailable(t *testing.T) {
	h := newMediaHarness(t, pngBytes(t, 4, 4), "image/png", "shot.png", nil)
	h.up.err = errors.New("media conn refused")

	out := expectError(t, callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	}), mcp.ErrMediaUnavailable)
	if msg, _ := out["message"].(string); !strings.Contains(msg, "media conn refused") {
		t.Errorf("message = %q, want the upload failure quoted", msg)
	}
	if h.mock.sendCalls != 0 {
		t.Error("failed upload still sent a message")
	}
}

// The whatsmeow client is nil until the device is linked, so a send that
// gets past the pairing gate but finds no upload surface must still report
// not_paired rather than a nil-pointer internal error.
func TestSendFile_WithoutAnUploaderIsNotPaired(t *testing.T) {
	h := newHarnessWithDeps(t, true, nil, &mockWA{}, nil)
	desc, err := h.media.Put([]byte("payload"), "application/pdf", "a.pdf")
	if err != nil {
		t.Fatalf("seed media: %v", err)
	}

	expectError(t, callTool(t, h, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": desc.MediaPath,
	}), mcp.ErrNotPaired)
}
