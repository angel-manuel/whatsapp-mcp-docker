// Package media owns the on-disk, content-addressed blob store that backs
// the media MCP tools and the byte routes they hand out references to:
// GET /media/{sha256} outbound, POST /media inbound.
//
// MCP cannot carry bytes usefully, so the split is deliberate and runs both
// ways: download_media fetches an attachment and returns a small JSON
// *descriptor*, send_file and send_audio_message take one, and the bytes
// themselves move over plain HTTP on the same authenticated listener that
// serves /mcp. Nothing in this package ever puts payload bytes into a tool
// result, or accepts them from one.
//
// Layout under the store directory, for a blob whose plaintext SHA-256 is
// <sha>:
//
//	<sha>.<ext>   the bytes, exactly as downloaded
//	<sha>.json    a sidecar descriptor (mime, filename, size, ext)
//
// The sidecar exists so the byte route can answer with the right
// Content-Type and Content-Disposition without consulting the message cache;
// the store is self-describing and can be swept, backed up, or wiped on its
// own.
//
// The two files also carry two different timestamps, which is what lets
// retention be both age-aware and usage-aware without a database:
//
//	blob mtime      when the bytes were downloaded. Never rewritten, so
//	                Last-Modified is stable and TTL means real age.
//	sidecar mtime   when the object was last resolved (served or looked
//	                up). This is the recency signal for LRU eviction.
package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned by Lookup and Open for a digest that is not (or is
// no longer) in the store. Callers map it to a 404 / not_found.
var ErrNotFound = errors.New("media: blob not found")

// RoutePrefix is the URL prefix under which blobs are served. Descriptor
// MediaPath values are built from it, and internal/mcp mounts the handler
// there; keeping both derived from one constant stops them drifting.
const RoutePrefix = "/media/"

// DefaultMime is used for Content-Type when a message carried no mimetype.
const DefaultMime = "application/octet-stream"

// Descriptor is the JSON shape handed back to MCP callers. It is
// deliberately small: a pointer to the bytes plus what a client needs to
// present them. MediaPath is relative to the server root, so the gateway
// fronting this container can join it onto whatever base URL it exposes.
type Descriptor struct {
	MediaPath string `json:"media_path"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
}

// sidecar is the on-disk form of a Descriptor. It stores Ext rather than
// MediaPath because the path is derived, while the extension is needed to
// find the blob again.
type sidecar struct {
	SHA256   string `json:"sha256"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Ext      string `json:"ext"`
}

// Options configures retention (MaxBytes, TTL — independent, and both off by
// default; see Sweep) plus the inbound cap on POST /media, which is a
// refusal rather than a retention rule and has a non-zero default.
type Options struct {
	// MaxBytes caps the total size of stored blobs. Zero means unlimited.
	MaxBytes int64
	// TTL evicts blobs older than this. Zero disables age-based eviction.
	TTL time.Duration
	// MaxUploadBytes caps a single inbound blob (POST /media). Zero means
	// DefaultMaxUploadBytes. Unlike MaxBytes this is a hard refusal, not an
	// eviction trigger.
	MaxUploadBytes int64
	// Logger receives sweep results. Nil defaults to slog.Default.
	Logger *slog.Logger
}

// Store is a handle to a content-addressed media directory. It is safe for
// concurrent use: writes go through a temp file plus rename, so a reader
// never observes a partially written blob.
type Store struct {
	dir  string
	opts Options
	log  *slog.Logger
}

// Open creates dir if needed and returns a Store rooted there.
func Open(dir string, opts Options) (*Store, error) {
	if dir == "" {
		return nil, errors.New("media: dir is required")
	}
	if opts.MaxBytes < 0 {
		return nil, fmt.Errorf("media: MaxBytes must not be negative, got %d", opts.MaxBytes)
	}
	if opts.TTL < 0 {
		return nil, fmt.Errorf("media: TTL must not be negative, got %s", opts.TTL)
	}
	if opts.MaxUploadBytes < 0 {
		return nil, fmt.Errorf("media: MaxUploadBytes must not be negative, got %d", opts.MaxUploadBytes)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("media: create %s: %w", dir, err)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Store{dir: dir, opts: opts, log: log}, nil
}

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

// Put stores data under its own SHA-256. It is idempotent: a digest already
// present is a cache hit, the bytes are not rewritten, and the existing
// descriptor is returned unchanged — so a repeated download_media call costs
// nothing beyond a stat.
//
// mime and filename describe the blob for the byte route; an empty mime
// becomes DefaultMime and an empty filename is synthesised from the digest.
// A cache hit counts as a use (see touch), which is what keeps
// recently-requested media alive under size-based eviction (see Sweep).
func (s *Store) Put(data []byte, mimeType, filename string) (Descriptor, error) {
	return s.putStream(bytes.NewReader(data), false, mimeType, filename)
}

// PutReader is Put for a stream of unknown length, and the entry point the
// POST /media route uses. Memory use is bounded regardless of payload size,
// which is what makes it safe to hang off an HTTP handler.
//
// Unlike Put, the payload is treated as untrusted: it is capped at the
// store's upload limit (ErrTooLarge past it) and a zero-length body is
// refused (ErrEmptyUpload).
func (s *Store) PutReader(r io.Reader, mimeType, filename string) (Descriptor, error) {
	return s.putStream(r, true, mimeType, filename)
}

// putStream is the one implementation behind Put and PutReader. It spools r
// to a temp file in the store directory while hashing it, then moves it into
// place under its own digest — so nothing is ever buffered whole in memory
// and a reader never observes a partially written blob.
//
// bounded marks an untrusted inbound payload (an HTTP body rather than bytes
// this process just downloaded): those are capped at the store's upload limit
// and may not be empty.
func (s *Store) putStream(r io.Reader, bounded bool, mimeType, filename string) (Descriptor, error) {
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

	src := r
	limit := s.maxUpload()
	if bounded {
		// Read one byte past the cap so a payload exactly at the limit still
		// succeeds and the first byte over it is detected without buffering
		// the whole body.
		src = io.LimitReader(r, limit+1)
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return Descriptor{}, fmt.Errorf("media: buffer payload: %w", err)
	}
	if bounded {
		if size > limit {
			return Descriptor{}, ErrTooLarge
		}
		if size == 0 {
			return Descriptor{}, ErrEmptyUpload
		}
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("media: close temp blob: %w", err)
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
		return Descriptor{}, fmt.Errorf("media: chmod blob %s: %w", digest, err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, digest+ext)); err != nil {
		return Descriptor{}, fmt.Errorf("media: rename blob %s: %w", digest, err)
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

// Lookup returns the descriptor for digest without opening the blob, and
// records the object as used. Returns ErrNotFound if either the sidecar or
// the blob is missing.
func (s *Store) Lookup(digest string) (Descriptor, error) {
	meta, err := s.readSidecar(digest)
	if err != nil {
		return Descriptor{}, err
	}
	if _, err := os.Stat(s.blobPath(meta)); err != nil {
		if os.IsNotExist(err) {
			return Descriptor{}, ErrNotFound
		}
		return Descriptor{}, fmt.Errorf("media: stat blob %s: %w", digest, err)
	}
	s.touch(meta)
	return meta.descriptor(), nil
}

// Open returns an open handle to the blob plus its descriptor and the time
// the bytes were stored, and records the object as used. The caller owns the
// returned file and must close it.
//
// The returned time is the blob's own mtime, which never changes — so
// Last-Modified stays stable across requests even though each request counts
// as a use for eviction purposes.
func (s *Store) Open(digest string) (*os.File, Descriptor, time.Time, error) {
	meta, err := s.readSidecar(digest)
	if err != nil {
		return nil, Descriptor{}, time.Time{}, err
	}
	f, err := os.Open(s.blobPath(meta))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Descriptor{}, time.Time{}, ErrNotFound
		}
		return nil, Descriptor{}, time.Time{}, fmt.Errorf("media: open blob %s: %w", digest, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Descriptor{}, time.Time{}, fmt.Errorf("media: stat blob %s: %w", digest, err)
	}
	s.touch(meta)
	return f, meta.descriptor(), info.ModTime(), nil
}

func (s *Store) readSidecar(digest string) (sidecar, error) {
	norm, ok := NormalizeDigest(digest)
	if !ok {
		// Not a digest at all: refuse before touching the filesystem, so a
		// traversal attempt never reaches a path join.
		return sidecar{}, ErrNotFound
	}
	body, err := os.ReadFile(filepath.Join(s.dir, norm+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return sidecar{}, ErrNotFound
		}
		return sidecar{}, fmt.Errorf("media: read sidecar %s: %w", norm, err)
	}
	var meta sidecar
	if err := json.Unmarshal(body, &meta); err != nil {
		return sidecar{}, fmt.Errorf("media: parse sidecar %s: %w", norm, err)
	}
	// Trust the filename on disk, never the one inside the sidecar, so a
	// corrupted/hand-edited sidecar cannot redirect reads elsewhere.
	meta.SHA256 = norm
	return meta, nil
}

func (s *Store) blobPath(meta sidecar) string {
	return filepath.Join(s.dir, meta.SHA256+sanitizeExt(meta.Ext))
}

// touch records the object as used, by bumping the *sidecar's* mtime. The
// blob's own mtime is deliberately left alone: it is both the Last-Modified
// this store serves and the age TTL measures, and neither should move just
// because someone read the file.
//
// Failures are ignored. A blob that cannot be touched is still perfectly
// serveable; it just looks staler than it is to the next sweep.
func (s *Store) touch(meta sidecar) {
	now := time.Now()
	_ = os.Chtimes(filepath.Join(s.dir, meta.SHA256+".json"), now, now)
}

// writeFile writes name atomically: a temp file in the same directory,
// fsync-free rename. Readers see either the old state or the complete new
// one, never a truncated blob.
func (s *Store) writeFile(name string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("media: create temp in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename succeeded
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("media: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("media: close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("media: chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("media: rename into %s: %w", name, err)
	}
	return nil
}

func (m sidecar) descriptor() Descriptor {
	mimeType := m.Mime
	if mimeType == "" {
		mimeType = DefaultMime
	}
	return Descriptor{
		MediaPath: RoutePrefix + m.SHA256,
		Mime:      mimeType,
		Size:      m.Size,
		Filename:  m.Filename,
		SHA256:    m.SHA256,
	}
}

// NormalizeDigest validates that s is a 64-character hex SHA-256 and returns
// its lower-case form. Anything else — a relative path, a shorter hash, a
// name with a slash or a dot — is rejected. This is the single gate every
// filesystem path in this package passes through.
func NormalizeDigest(s string) (string, bool) {
	if len(s) != 64 {
		return "", false
	}
	out := make([]byte, 64)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			out[i] = c
		case c >= 'A' && c <= 'F':
			out[i] = c + ('a' - 'A')
		default:
			return "", false
		}
	}
	return string(out), true
}

// mimeExt pins the extension for the media types WhatsApp actually sends.
// The system mime database is consulted only for anything not listed here,
// so the common cases stay stable across container images.
var mimeExt = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"video/3gpp":      ".3gp",
	"video/quicktime": ".mov",
	"video/webm":      ".webm",
	"audio/ogg":       ".ogg",
	"audio/opus":      ".opus",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"audio/aac":       ".aac",
	"audio/amr":       ".amr",
	"audio/wav":       ".wav",
	"audio/x-wav":     ".wav",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

// ExtensionForMime maps a MIME type to the file extension used on disk,
// including the leading dot. Parameters are ignored ("audio/ogg;
// codecs=opus" is an ogg), and anything unrecognised falls back to ".bin".
func ExtensionForMime(mimeType string) string {
	base := mimeType
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil {
		base = parsed
	} else if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	base = strings.ToLower(strings.TrimSpace(base))
	if ext, ok := mimeExt[base]; ok {
		return ext
	}
	if exts, err := mime.ExtensionsByType(base); err == nil && len(exts) > 0 {
		return sanitizeExt(exts[0])
	}
	return ".bin"
}

// sanitizeExt keeps an extension to a short, dot-prefixed, alphanumeric
// token. It guards blobPath against a sidecar whose ext field contains a
// separator.
func sanitizeExt(ext string) string {
	if ext == "" {
		return ".bin"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if len(ext) > 16 {
		return ".bin"
	}
	for i := 1; i < len(ext); i++ {
		c := ext[i]
		isAlnum := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !isAlnum {
			return ".bin"
		}
	}
	return strings.ToLower(ext)
}

// SanitizeFilename strips path separators and control characters from a
// caller-supplied name. WhatsApp document filenames are attacker-controlled;
// they end up in Content-Disposition and must not be able to escape it.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = filepath.Base(filepath.ToSlash(name))
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '/' || r == '"':
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}
