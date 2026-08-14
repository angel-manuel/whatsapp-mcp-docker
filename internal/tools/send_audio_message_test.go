package tools_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
)

// oggOpusFixture builds a minimal Ogg/Opus stream whose final granule
// encodes seconds of audio. It mirrors the fixture builder in internal/audio
// — duplicated rather than exported, because the shape of a valid stream is
// exactly what these tests are pinning down and a shared helper could drift
// into asserting nothing.
func oggOpusFixture(seconds uint64) []byte {
	const preSkip = 312

	page := func(headerType byte, granule uint64, seq uint32, payload []byte) []byte {
		p := make([]byte, 27)
		copy(p, "OggS")
		p[5] = headerType
		binary.LittleEndian.PutUint64(p[6:14], granule)
		binary.LittleEndian.PutUint32(p[18:22], seq)
		p[26] = 1
		p = append(p, byte(len(payload)))
		return append(p, payload...)
	}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], 48000)

	var b bytes.Buffer
	b.Write(page(0x02, 0, 0, head))
	b.Write(page(0x00, 0, 1, []byte("OpusTags")))
	b.Write(page(0x04, preSkip+seconds*48000, 2, []byte("frames")))
	return b.Bytes()
}

// removeBlobBytes replaces the stored blob with a directory of the same
// name. That is the cheapest way to get a file the store can *open* but
// nothing can *read*: os.Open on a directory succeeds and the first Read
// fails with EISDIR — which is what a truncated or EIO-ridden blob looks
// like to the probe, without needing a broken filesystem.
func removeBlobBytes(t *testing.T, h *mediaHarness) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(h.media.Dir(), h.desc.SHA256+".*"))
	if err != nil {
		t.Fatalf("glob blob: %v", err)
	}
	for _, m := range matches {
		if strings.HasSuffix(m, ".json") {
			continue // keep the sidecar so the descriptor still resolves
		}
		if err := os.Remove(m); err != nil {
			t.Fatalf("remove blob: %v", err)
		}
		if err := os.Mkdir(m, 0o750); err != nil {
			t.Fatalf("replace blob with a directory: %v", err)
		}
		return
	}
	t.Fatalf("no blob found for %s", h.desc.SHA256)
}

// errStub is the canned failure the transcoder fakes return.
var errStub = errors.New("ffmpeg: Invalid data found when processing input")

func TestSendAudioMessage_OpusInputNeedsNoFFmpeg(t *testing.T) {
	opus := oggOpusFixture(7)
	// available:false is the distroless image — Opus in, no transcode.
	h := newMediaHarness(t, opus, "audio/ogg", "note.ogg", &fakeTranscoder{available: false})

	res := callTool(t, h.testHarness, "send_audio_message", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	out := structured(t, res)

	if out["transcoded"] != false {
		t.Errorf("transcoded = %v, want false", out["transcoded"])
	}
	if out["voice_note"] != true {
		t.Errorf("voice_note = %v, want true", out["voice_note"])
	}
	if got := out["duration_seconds"]; got != float64(7) {
		t.Errorf("duration_seconds = %v, want 7", got)
	}
	if out["sha256"] != h.desc.SHA256 {
		t.Errorf("sha256 = %v, want the uploaded blob %v", out["sha256"], h.desc.SHA256)
	}
	if h.audio.calls != 0 {
		t.Errorf("ffmpeg was invoked %d times for Opus input", h.audio.calls)
	}

	audioMsg := h.mock.lastSendMs.GetAudioMessage()
	if audioMsg == nil {
		t.Fatalf("sent message is not an AudioMessage: %+v", h.mock.lastSendMs)
	}
	if !audioMsg.GetPTT() {
		t.Error("PTT = false, want a voice note")
	}
	if audioMsg.GetSeconds() != 7 {
		t.Errorf("Seconds = %d, want 7", audioMsg.GetSeconds())
	}
	if !strings.HasPrefix(audioMsg.GetMimetype(), "audio/ogg") {
		t.Errorf("mimetype = %q, want an audio/ogg opus type", audioMsg.GetMimetype())
	}
	if h.up.readerCalls != 1 {
		t.Errorf("reader uploads = %d, want the stored blob streamed as-is", h.up.readerCalls)
	}
}

func TestSendAudioMessage_TranscodesWhenFFmpegIsPresent(t *testing.T) {
	opus := oggOpusFixture(3)
	tr := &fakeTranscoder{available: true, out: opus}
	mp3 := append([]byte{0xFF, 0xFB, 0x90, 0x00}, []byte("an mp3, allegedly")...)
	h := newMediaHarness(t, mp3, "audio/mpeg", "clip.mp3", tr)

	res := callTool(t, h.testHarness, "send_audio_message", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	out := structured(t, res)

	if tr.calls != 1 {
		t.Fatalf("ffmpeg calls = %d, want 1", tr.calls)
	}
	if !bytes.Equal(tr.lastIn, mp3) {
		t.Error("transcoder did not receive the stored bytes")
	}
	if out["transcoded"] != true {
		t.Errorf("transcoded = %v, want true", out["transcoded"])
	}
	if out["duration_seconds"] != float64(3) {
		t.Errorf("duration_seconds = %v, want 3 (from the converted stream)", out["duration_seconds"])
	}
	// The result must describe what went out, not what came in.
	if out["sha256"] == h.desc.SHA256 {
		t.Error("sha256 still points at the pre-transcode blob")
	}
	if mime, _ := out["mime"].(string); !strings.HasPrefix(mime, "audio/ogg") {
		t.Errorf("mime = %v, want audio/ogg", out["mime"])
	}
	if h.up.uploadCalls != 1 || h.up.readerCalls != 0 {
		t.Errorf("upload calls: upload=%d reader=%d, want the in-memory path",
			h.up.uploadCalls, h.up.readerCalls)
	}
	if !bytes.Equal(h.up.lastBytes, opus) {
		t.Error("uploaded bytes are not the transcoder's output")
	}

	// The converted bytes are stored too, so download_media on our own
	// voice note serves what the recipient actually got.
	dl := callTool(t, h.testHarness, "download_media", map[string]any{
		"chat_jid":   sendChatJID,
		"message_id": out["message_id"],
	})
	if dl.IsError {
		t.Fatalf("download_media on the sent voice note failed: %+v", structured(t, dl))
	}
	if got, want := structured(t, dl)["media_path"], "/media/"+out["sha256"].(string); got != want {
		t.Errorf("media_path = %v, want the transcoded blob at %s", got, want)
	}
}

func TestSendAudioMessage_RefusesNonOpusWithoutFFmpeg(t *testing.T) {
	mp3 := append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 128)...)
	h := newMediaHarness(t, mp3, "audio/mpeg", "clip.mp3", &fakeTranscoder{available: false})

	out := expectError(t, callTool(t, h.testHarness, "send_audio_message", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	}), mcp.ErrInvalidArgument)

	msg, _ := out["message"].(string)
	for _, want := range []string{"ffmpeg", "FFMPEG_PATH", "Opus", "slim"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to mention %q", msg, want)
		}
	}
	if h.mock.sendCalls != 0 {
		t.Error("unplayable audio was sent anyway")
	}
	if h.up.uploadCalls+h.up.readerCalls != 0 {
		t.Error("unplayable audio was uploaded anyway")
	}
}

// A caller can lie about the mimetype; the magic number cannot. An "opus"
// label over mp3 bytes must still go through ffmpeg.
func TestSendAudioMessage_TrustsTheBytesNotTheLabel(t *testing.T) {
	tr := &fakeTranscoder{available: true, out: oggOpusFixture(1)}
	mislabelled := append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 64)...)
	h := newMediaHarness(t, mislabelled, "audio/ogg; codecs=opus", "liar.ogg", tr)

	res := callTool(t, h.testHarness, "send_audio_message", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	if tr.calls != 1 {
		t.Errorf("ffmpeg calls = %d, want the mislabelled input transcoded", tr.calls)
	}
}

func TestSendAudioMessage_TranscodeFailureIsInternal(t *testing.T) {
	tr := &fakeTranscoder{available: true, err: errStub}
	h := newMediaHarness(t, []byte("not audio at all"), "audio/x-wav", "broken.wav", tr)

	out := expectError(t, callTool(t, h.testHarness, "send_audio_message", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	}), mcp.ErrInternal)
	if msg, _ := out["message"].(string); !strings.Contains(msg, errStub.Error()) {
		t.Errorf("message = %q, want the transcoder failure quoted", msg)
	}
	if h.mock.sendCalls != 0 {
		t.Error("failed transcode still sent a message")
	}
}

// Both audio tools read the stored blob before uploading; a blob that
// cannot be read is a real failure and must be reported, not turned into a
// send with no duration. This pins the two tools to the same answer.
func TestSendAudio_UnreadableBlobIsReportedByBothTools(t *testing.T) {
	for _, tool := range []string{"send_file", "send_audio_message"} {
		t.Run(tool, func(t *testing.T) {
			h := newMediaHarness(t, oggOpusFixture(2), "audio/ogg", "note.ogg",
				&fakeTranscoder{available: true, out: oggOpusFixture(2)})

			// Delete the bytes out from under the descriptor, leaving the
			// sidecar: Open succeeds, reading does not.
			removeBlobBytes(t, h)

			res := callTool(t, h.testHarness, tool, map[string]any{
				"recipient":  sendChatJID,
				"media_path": h.desc.MediaPath,
			})
			// Same failure, same code from both tools: the probe error is
			// the diagnosis, not whatever the upload happens to say about
			// the unreadable bytes afterwards.
			out := expectError(t, res, mcp.ErrInternal)
			if msg, _ := out["message"].(string); !strings.Contains(msg, "probe audio") {
				t.Errorf("message = %q, want it to name the probe failure", msg)
			}
			if h.mock.sendCalls != 0 {
				t.Errorf("%s sent a message despite the unreadable blob", tool)
			}
		})
	}
}

// send_file's audio branch is the attachment path: playable formats go out
// untouched and are not marked as voice notes.
func TestSendFile_PlayableAudioIsAnAttachmentNotAVoiceNote(t *testing.T) {
	mp3 := append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 64)...)
	tr := &fakeTranscoder{available: true, out: oggOpusFixture(1)}
	h := newMediaHarness(t, mp3, "audio/mpeg", "song.mp3", tr)

	res := callTool(t, h.testHarness, "send_file", map[string]any{
		"recipient":  sendChatJID,
		"media_path": h.desc.MediaPath,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	if got := structured(t, res)["media_type"]; got != "audio" {
		t.Errorf("media_type = %v, want audio", got)
	}
	if tr.calls != 0 {
		t.Errorf("ffmpeg calls = %d, want an mp3 attachment sent as-is", tr.calls)
	}
	audioMsg := h.mock.lastSendMs.GetAudioMessage()
	if audioMsg == nil {
		t.Fatalf("not an AudioMessage: %+v", h.mock.lastSendMs)
	}
	if audioMsg.GetPTT() {
		t.Error("PTT = true, want a plain audio attachment")
	}
	if audioMsg.GetMimetype() != "audio/mpeg" {
		t.Errorf("mimetype = %q, want audio/mpeg", audioMsg.GetMimetype())
	}
}

// A format WhatsApp will not play is transcoded even on the attachment path
// — and refused outright when there is nothing to transcode with.
func TestSendFile_UnplayableAudioNeedsFFmpeg(t *testing.T) {
	wav := append([]byte("RIFF....WAVEfmt "), make([]byte, 64)...)

	t.Run("transcodes when available", func(t *testing.T) {
		tr := &fakeTranscoder{available: true, out: oggOpusFixture(2)}
		h := newMediaHarness(t, wav, "audio/x-wav", "memo.wav", tr)
		res := callTool(t, h.testHarness, "send_file", map[string]any{
			"recipient":  sendChatJID,
			"media_path": h.desc.MediaPath,
		})
		if res.IsError {
			t.Fatalf("unexpected error: %+v", structured(t, res))
		}
		if tr.calls != 1 {
			t.Errorf("ffmpeg calls = %d, want 1", tr.calls)
		}
		if audioMsg := h.mock.lastSendMs.GetAudioMessage(); audioMsg.GetPTT() {
			t.Error("PTT = true, want an attachment even after transcoding")
		}
	})

	t.Run("refuses when absent", func(t *testing.T) {
		h := newMediaHarness(t, wav, "audio/x-wav", "memo.wav", &fakeTranscoder{available: false})
		out := expectError(t, callTool(t, h.testHarness, "send_file", map[string]any{
			"recipient":  sendChatJID,
			"media_path": h.desc.MediaPath,
		}), mcp.ErrInvalidArgument)
		if msg, _ := out["message"].(string); !strings.Contains(msg, "ffmpeg") {
			t.Errorf("message = %q, want it to name ffmpeg", msg)
		}
		if h.mock.sendCalls != 0 {
			t.Error("unplayable audio was sent anyway")
		}
	})
}
