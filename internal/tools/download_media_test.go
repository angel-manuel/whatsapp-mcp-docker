package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/cache"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/mcp"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/tools"
)

// fakeDownloader records how the tool asked for the bytes. The distinction
// between the two call paths matters: direct-path downloads survive URL
// expiry, URL downloads do not, so the tests assert which one was taken
// rather than only that bytes came back.
type fakeDownloader struct {
	data []byte
	err  error

	pathCalls int
	urlCalls  int

	lastDirectPath string
	lastMediaType  whatsmeow.MediaType
	lastMMSType    string
	lastFileLength int
	lastMediaKey   []byte
	lastFileHash   []byte
	lastEncHash    []byte
}

func (f *fakeDownloader) Download(_ context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	f.urlCalls++
	f.lastMediaType = whatsmeow.GetMediaType(msg)
	f.lastDirectPath = msg.GetDirectPath()
	f.lastMediaKey = msg.GetMediaKey()
	return f.data, f.err
}

func (f *fakeDownloader) DownloadMediaWithPath(_ context.Context, directPath string,
	encFileHash, fileHash, mediaKey []byte, fileLength int,
	mediaType whatsmeow.MediaType, mmsType string,
) ([]byte, error) {
	f.pathCalls++
	f.lastDirectPath = directPath
	f.lastEncHash = encFileHash
	f.lastFileHash = fileHash
	f.lastMediaKey = mediaKey
	f.lastFileLength = fileLength
	f.lastMediaType = mediaType
	f.lastMMSType = mmsType
	return f.data, f.err
}

var _ tools.MediaDownloader = (*fakeDownloader)(nil)

const mediaChatJID = "1234567890@s.whatsapp.net"

// seedMedia inserts one media message. Passing an empty directPath models a
// row ingested before migration 004.
func seedMedia(id string, kind cache.MessageKind, mime, filename, url, directPath string, payload []byte) func(*cache.Store) {
	sum := sha256.Sum256(payload)
	return func(s *cache.Store) {
		err := s.InsertMessage(context.Background(), cache.Message{
			ID:        id,
			ChatJID:   mediaChatJID,
			SenderJID: mediaChatJID,
			Timestamp: time.Unix(1_700_000_000, 0).UTC(),
			Kind:      kind,
			Media: &cache.Media{
				Mime: mime, Filename: filename, URL: url, DirectPath: directPath,
				Key:       []byte{0x11, 0x22, 0x33},
				SHA256:    sum[:],
				EncSHA256: []byte{0x44, 0x55},
				Length:    uint64(len(payload)),
			},
		})
		if err != nil {
			panic(err)
		}
	}
}

func TestDownloadMedia_UsesDirectPathAndStoresContentAddressed(t *testing.T) {
	payload := []byte("pretend this is a jpeg")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.IMG", cache.KindImage, "image/jpeg", "", "https://mmg.whatsapp.net/x", "/v/t62/img", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.IMG",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	out := structured(t, res)

	sum := sha256.Sum256(payload)
	wantDigest := hex.EncodeToString(sum[:])
	if got := out["sha256"]; got != wantDigest {
		t.Errorf("sha256 = %v, want %v", got, wantDigest)
	}
	if got := out["media_path"]; got != "/media/"+wantDigest {
		t.Errorf("media_path = %v", got)
	}
	if got := out["mime"]; got != "image/jpeg" {
		t.Errorf("mime = %v", got)
	}
	if got := out["size"]; got != float64(len(payload)) {
		t.Errorf("size = %v, want %d", got, len(payload))
	}
	// Images carry no filename on the wire, so one is synthesised from the
	// message timestamp — deterministic across repeat calls.
	if got := out["filename"]; got != "image_20231114_221320.jpg" {
		t.Errorf("filename = %v", got)
	}
	// The whole point of the descriptor: payload bytes must never reach the
	// tool result. mcp-go mirrors the structured JSON into a text block for
	// backward compatibility, and that mirror must stay a descriptor too.
	for _, c := range res.Content {
		text, ok := c.(mcpgo.TextContent)
		if !ok {
			t.Errorf("unexpected non-text content block %T in a descriptor result", c)
			continue
		}
		if strings.Contains(text.Text, string(payload)) {
			t.Errorf("payload bytes leaked into the tool result: %q", text.Text)
		}
	}

	if dl.pathCalls != 1 || dl.urlCalls != 0 {
		t.Fatalf("pathCalls=%d urlCalls=%d, want the direct-path route", dl.pathCalls, dl.urlCalls)
	}
	if dl.lastDirectPath != "/v/t62/img" {
		t.Errorf("directPath = %q", dl.lastDirectPath)
	}
	if dl.lastMediaType != whatsmeow.MediaImage {
		t.Errorf("mediaType = %q, want %q", dl.lastMediaType, whatsmeow.MediaImage)
	}
	if dl.lastMMSType != "" {
		t.Errorf("mmsType = %q, want empty so whatsmeow derives it", dl.lastMMSType)
	}
	if dl.lastFileLength != len(payload) {
		t.Errorf("fileLength = %d, want %d", dl.lastFileLength, len(payload))
	}

	// Bytes on disk must match the digest the descriptor advertises.
	blob := filepath.Join(h.media.Dir(), wantDigest+".jpg")
	got, err := os.ReadFile(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("stored bytes = %q, want %q", got, payload)
	}
}

func TestDownloadMedia_SecondCallIsACacheHit(t *testing.T) {
	payload := []byte("bytes")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.VID", cache.KindVideo, "video/mp4", "", "", "/v/t62/vid", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	args := map[string]any{"chat_jid": mediaChatJID, "message_id": "wamid.VID"}
	first := structured(t, callTool(t, h, "download_media", args))
	second := structured(t, callTool(t, h, "download_media", args))

	if dl.pathCalls != 1 {
		t.Errorf("downloader called %d times; the second call must hit the store", dl.pathCalls)
	}
	if first["sha256"] != second["sha256"] || first["media_path"] != second["media_path"] {
		t.Errorf("descriptor changed between calls: %v vs %v", first, second)
	}
	if got := first["filename"]; got != "video_20231114_221320.mp4" {
		t.Errorf("filename = %v", got)
	}
}

func TestDownloadMedia_KindsMapToMediaTypes(t *testing.T) {
	payload := []byte("payload")
	cases := []struct {
		kind cache.MessageKind
		mime string
		want whatsmeow.MediaType
	}{
		{cache.KindImage, "image/jpeg", whatsmeow.MediaImage},
		{cache.KindVideo, "video/mp4", whatsmeow.MediaVideo},
		{cache.KindAudio, "audio/ogg; codecs=opus", whatsmeow.MediaAudio},
		{cache.KindDocument, "application/pdf", whatsmeow.MediaDocument},
		// Stickers are encrypted with the image key derivation.
		{cache.KindSticker, "image/webp", whatsmeow.MediaImage},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			dl := &fakeDownloader{data: payload}
			h := newHarnessWithDeps(t, true,
				seedMedia("wamid.K", tc.kind, tc.mime, "", "", "/v/t62/blob", payload),
				nil,
				func(d *tools.Deps) { d.Downloader = dl })

			res := callTool(t, h, "download_media", map[string]any{
				"chat_jid": mediaChatJID, "message_id": "wamid.K",
			})
			if res.IsError {
				t.Fatalf("unexpected error: %+v", structured(t, res))
			}
			if dl.lastMediaType != tc.want {
				t.Errorf("mediaType = %q, want %q", dl.lastMediaType, tc.want)
			}
		})
	}
}

func TestDownloadMedia_DocumentKeepsItsWireFilename(t *testing.T) {
	payload := []byte("%PDF-1.4")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.DOC", cache.KindDocument, "application/pdf", "Q1 report.pdf", "", "/v/t62/doc", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	out := structured(t, callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.DOC",
	}))
	if got := out["filename"]; got != "Q1 report.pdf" {
		t.Errorf("filename = %v, want the wire filename", got)
	}
}

func TestDownloadMedia_DocumentFilenameIsSanitised(t *testing.T) {
	// Document filenames come from the sender. A traversal attempt must not
	// reach the descriptor, let alone the filesystem.
	payload := []byte("evil")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.EVIL", cache.KindDocument, "application/pdf", "../../etc/passwd", "", "/v/t62/doc", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	out := structured(t, callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.EVIL",
	}))
	if got := out["filename"]; got != "passwd" {
		t.Errorf("filename = %v, want the path stripped", got)
	}
}

func TestDownloadMedia_FallsBackToURLWhenDirectPathMissing(t *testing.T) {
	payload := []byte("legacy row bytes")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.OLD", cache.KindImage, "image/jpeg", "", "https://mmg.whatsapp.net/legacy", "", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.OLD",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %+v", structured(t, res))
	}
	if dl.urlCalls != 1 || dl.pathCalls != 0 {
		t.Fatalf("urlCalls=%d pathCalls=%d, want the URL route", dl.urlCalls, dl.pathCalls)
	}
	if dl.lastMediaType != whatsmeow.MediaImage {
		t.Errorf("mediaType = %q", dl.lastMediaType)
	}
}

// TestDownloadMedia_NoLocatorExplainsTheBackfillGap is the error message the
// spec calls out explicitly: media_direct_path cannot be backfilled, so the
// user must be told to re-ingest rather than handed an opaque failure.
func TestDownloadMedia_NoLocatorExplainsTheBackfillGap(t *testing.T) {
	payload := []byte("unreachable")
	dl := &fakeDownloader{data: payload}
	h := newHarnessWithDeps(t, true,
		// web.whatsapp.net URLs are unusable without a direct path — this
		// is exactly whatsmeow's own rule.
		seedMedia("wamid.STALE", cache.KindImage, "image/jpeg", "", "https://web.whatsapp.net/x", "", payload),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.STALE",
	})
	s := expectError(t, res, mcp.ErrMediaUnavailable)
	msg, _ := s["message"].(string)
	for _, want := range []string{"cache_sync", "backfilled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if dl.urlCalls != 0 || dl.pathCalls != 0 {
		t.Errorf("downloader was called with no usable locator")
	}
}

func TestDownloadMedia_DownloadFailureIsStructured(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("cdn 410 gone")}
	h := newHarnessWithDeps(t, true,
		seedMedia("wamid.FAIL", cache.KindImage, "image/jpeg", "", "", "/v/t62/img", []byte("x")),
		nil,
		func(d *tools.Deps) { d.Downloader = dl })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.FAIL",
	})
	s := expectError(t, res, mcp.ErrMediaUnavailable)
	if msg, _ := s["message"].(string); !strings.Contains(msg, "cdn 410 gone") {
		t.Errorf("message %q drops the underlying cause", msg)
	}
}

func TestDownloadMedia_TextMessageHasNoMedia(t *testing.T) {
	h := newHarnessWithDeps(t, true, func(s *cache.Store) {
		if err := s.InsertMessage(context.Background(), cache.Message{
			ID: "wamid.TXT", ChatJID: mediaChatJID, SenderJID: mediaChatJID,
			Timestamp: time.Unix(1_700_000_000, 0), Kind: cache.KindText, Body: "hi",
		}); err != nil {
			panic(err)
		}
	}, nil, func(d *tools.Deps) { d.Downloader = &fakeDownloader{} })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.TXT",
	})
	expectError(t, res, mcp.ErrNoMedia)
}

func TestDownloadMedia_MediaKindWithoutKeyIsNoMedia(t *testing.T) {
	h := newHarnessWithDeps(t, true, func(s *cache.Store) {
		if err := s.InsertMessage(context.Background(), cache.Message{
			ID: "wamid.NOKEY", ChatJID: mediaChatJID, SenderJID: mediaChatJID,
			Timestamp: time.Unix(1_700_000_000, 0), Kind: cache.KindImage,
			Media: &cache.Media{Mime: "image/jpeg", URL: "https://mmg.whatsapp.net/x"},
		}); err != nil {
			panic(err)
		}
	}, nil, func(d *tools.Deps) { d.Downloader = &fakeDownloader{} })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.NOKEY",
	})
	expectError(t, res, mcp.ErrNoMedia)
}

func TestDownloadMedia_UnknownMessageIsNotFound(t *testing.T) {
	h := newHarnessWithDeps(t, true, nil, nil,
		func(d *tools.Deps) { d.Downloader = &fakeDownloader{} })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.NOPE",
	})
	expectError(t, res, mcp.ErrNotFound)
}

func TestDownloadMedia_ValidatesArguments(t *testing.T) {
	h := newHarnessWithDeps(t, true, nil, nil,
		func(d *tools.Deps) { d.Downloader = &fakeDownloader{} })

	cases := []map[string]any{
		{"message_id": "wamid.X"},
		{"chat_jid": mediaChatJID},
		{"chat_jid": "   ", "message_id": "wamid.X"},
	}
	for _, args := range cases {
		expectError(t, callTool(t, h, "download_media", args), mcp.ErrInvalidArgument)
	}
}

func TestDownloadMedia_NotPairedIsGated(t *testing.T) {
	h := newHarnessWithDeps(t, false, nil, nil,
		func(d *tools.Deps) { d.Downloader = &fakeDownloader{} })

	res := callTool(t, h, "download_media", map[string]any{
		"chat_jid": mediaChatJID, "message_id": "wamid.X",
	})
	expectError(t, res, mcp.ErrNotPaired)
}
