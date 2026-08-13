package media

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

// ErrTooLarge is returned by PutReader when the payload exceeds the
// configured cap. The upload route maps it to 413.
var ErrTooLarge = errors.New("media: upload exceeds the configured size limit")

// maxUpload resolves the effective per-upload cap.
func (s *Store) maxUpload() int64 {
	if s.opts.MaxUploadBytes > 0 {
		return s.opts.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

// PutReader is Put for a stream of unknown length: it spools r to a temp
// file in the store directory while hashing it, then moves it into place
// under its own digest. Memory use is bounded regardless of payload size,
// which is what makes it safe to hang off an HTTP route.
//
// Like Put it is idempotent — re-uploading identical bytes is a cache hit
// that rewrites nothing and counts as a use — and it enforces the store's
// upload cap, returning ErrTooLarge once the limit is passed.
func (s *Store) PutReader(r io.Reader, mimeType, filename string) (Descriptor, error) {
	if mimeType == "" {
		mimeType = DefaultMime
	}
	ext := ExtensionForMime(mimeType)

	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return Descriptor{}, fmt.Errorf("media: create temp in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()

	// Read one byte past the cap so an exactly-at-limit payload still
	// succeeds and the first byte over it is detected without buffering the
	// whole body.
	limit := s.maxUpload()
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err != nil {
		return Descriptor{}, fmt.Errorf("media: buffer upload: %w", err)
	}
	if size > limit {
		return Descriptor{}, ErrTooLarge
	}
	if size == 0 {
		return Descriptor{}, errors.New("media: refusing to store an empty upload")
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("media: close temp upload: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if filename == "" {
		filename = digest[:12] + ext
	}

	switch existing, lookupErr := s.readSidecar(digest); {
	case lookupErr == nil:
		if _, statErr := os.Stat(s.blobPath(existing)); statErr == nil {
			s.touch(existing)
			return existing.descriptor(), nil
		}
		// Sidecar without bytes: fall through and rewrite both.
	case !errors.Is(lookupErr, ErrNotFound):
		return Descriptor{}, lookupErr
	}

	meta := sidecar{
		SHA256:   digest,
		Mime:     mimeType,
		Filename: SanitizeFilename(filename),
		Size:     size,
		Ext:      ext,
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return Descriptor{}, fmt.Errorf("media: chmod upload %s: %w", digest, err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, digest+ext)); err != nil {
		return Descriptor{}, fmt.Errorf("media: rename upload %s: %w", digest, err)
	}
	keep = true

	body, err := json.Marshal(meta)
	if err != nil {
		return Descriptor{}, fmt.Errorf("media: marshal sidecar %s: %w", digest, err)
	}
	if err := s.writeFile(digest+".json", body); err != nil {
		// Leave no blob without its sidecar: an orphan would be invisible
		// to Lookup but still count against the size cap.
		_ = os.Remove(filepath.Join(s.dir, digest+ext))
		return Descriptor{}, err
	}
	return meta.descriptor(), nil
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
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			http.Error(w, fmt.Sprintf("payload exceeds the %d byte limit", limit),
				http.StatusRequestEntityTooLarge)
			return
		}
		applog.WithEvent(s.log, "media.upload").Error("store upload failed",
			slog.String("err", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
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
