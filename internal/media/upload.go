package media

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
)

// UploadRoutePattern is the URL the upload handler is mounted on. It is the
// inbound mirror of RoutePrefix + "{sha256}": the send tools take a
// `/media/<sha256>` reference, exactly like download_media hands one back,
// and the bytes behind it get here by POST rather than through MCP.
const UploadRoutePattern = "/media"

// DefaultMaxUploadBytes caps a single POST /media body at 100 MiB. WhatsApp
// itself refuses attachments well below this for every type except
// documents, and the cap exists mainly so one oversized request cannot fill
// the data volume before the retention sweeper next runs.
const DefaultMaxUploadBytes int64 = 100 << 20

// sniffLen is how many leading bytes are inspected when the caller sent no
// usable Content-Type. It matches http.DetectContentType's own window.
const sniffLen = 512

// Errors the upload path returns for a caller's mistake rather than a store
// failure. The route maps them to 4xx; nothing here is logged as an internal
// error, because none of it is one.
var (
	// ErrTooLarge is returned by PutReader when the payload exceeds the
	// configured cap. The upload route maps it to 413.
	ErrTooLarge = errors.New("media: upload exceeds the configured size limit")
	// ErrEmptyUpload is returned by PutReader for a zero-length body. The
	// upload route maps it to 400.
	ErrEmptyUpload = errors.New("media: refusing to store an empty upload")
)

// maxUpload resolves the effective per-upload cap.
func (s *Store) maxUpload() int64 {
	if s.opts.MaxUploadBytes > 0 {
		return s.opts.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

// UploadHandler accepts POST UploadRoutePattern and stores the request body
// as a blob, answering with the same JSON descriptor shape download_media
// returns. It is mounted by internal/server on the MCP listener, behind the
// same bearer auth as /mcp and GET /media/{sha256}.
//
// This is the inbound half of the "MCP moves pointers, HTTP moves bytes"
// split: the send tools take a `/media/<sha256>` reference and never a
// base64 payload, so an attachment never has to pass through an agent's
// context window in either direction.
//
// The body is the raw file. Content-Type names the mimetype (sniffed when
// absent or generic) and an optional `filename` query parameter names the
// file — which is what a document send advertises to the recipient.
func (s *Store) UploadHandler() http.Handler {
	return http.HandlerFunc(s.serveUpload)
}

func (s *Store) serveUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := s.maxUpload()
	body := bufio.NewReaderSize(http.MaxBytesReader(w, r.Body, limit+1), sniffLen)
	mimeType := resolveUploadMime(r.Header.Get("Content-Type"), body)

	desc, err := s.PutReader(body, mimeType, r.URL.Query().Get("filename"))
	if err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, ErrTooLarge), errors.As(err, &maxErr):
			http.Error(w, fmt.Sprintf("payload exceeds the %d byte limit", limit),
				http.StatusRequestEntityTooLarge)
		case errors.Is(err, ErrEmptyUpload):
			http.Error(w, "request body is empty; POST the file bytes as the body",
				http.StatusBadRequest)
		default:
			applog.WithEvent(s.log, "media.upload").Error("store upload failed",
				slog.String("err", err.Error()))
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	applog.WithEvent(s.log, "media.upload").Info("stored upload",
		slog.String("sha256", desc.SHA256),
		slog.String("mime", desc.Mime),
		slog.Int64("size", desc.Size))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", desc.MediaPath)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(desc)
}

// resolveUploadMime prefers the caller's Content-Type and falls back to
// sniffing the leading bytes. A generic octet-stream is treated as "not
// stated": clients that POST with curl's default header would otherwise turn
// every image into a document send.
func resolveUploadMime(header string, body *bufio.Reader) string {
	if declared := strings.TrimSpace(header); declared != "" && !isGenericMime(declared) {
		return declared
	}
	peek, err := body.Peek(sniffLen)
	if len(peek) == 0 || (err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull)) {
		return DefaultMime
	}
	return http.DetectContentType(peek)
}

func isGenericMime(m string) bool {
	base, _, ok := strings.Cut(m, ";")
	if !ok {
		base = m
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case DefaultMime, "application/x-www-form-urlencoded", "binary/octet-stream":
		return true
	default:
		return false
	}
}
