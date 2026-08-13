package media

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uploadMux mounts both halves exactly as internal/server does, so a test
// can post bytes and then fetch them back through the real routing.
func uploadMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(RoutePrefix+"{sha256}", s.Handler())
	mux.Handle(UploadRoutePattern, s.UploadHandler())
	return mux
}

func postMedia(t *testing.T, s *Store, body []byte, contentType, query string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, UploadRoutePattern+query, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	uploadMux(s).ServeHTTP(rec, req)
	return rec.Result()
}

func decodeDescriptor(t *testing.T, res *http.Response) Descriptor {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	var desc Descriptor
	if err := json.NewDecoder(res.Body).Decode(&desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	return desc
}

func TestUpload_StoresBytesAndAnswersWithADescriptor(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte("pretend this is a jpeg")

	res := postMedia(t, s, body, "image/jpeg", "?filename=holiday.jpg")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	desc := decodeDescriptor(t, res)

	if desc.SHA256 != digestOf(body) {
		t.Errorf("sha256 = %s, want %s", desc.SHA256, digestOf(body))
	}
	if desc.MediaPath != RoutePrefix+digestOf(body) {
		t.Errorf("media_path = %s", desc.MediaPath)
	}
	if desc.Mime != "image/jpeg" {
		t.Errorf("mime = %s, want image/jpeg", desc.Mime)
	}
	if desc.Filename != "holiday.jpg" {
		t.Errorf("filename = %s, want holiday.jpg", desc.Filename)
	}
	if desc.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", desc.Size, len(body))
	}

	// The round trip is the point: bytes posted here must come back out of
	// the byte route the send tools reference.
	rec := httptest.NewRecorder()
	uploadMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, desc.MediaPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", desc.MediaPath, rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("round-tripped body = %q, want %q", rec.Body.String(), body)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
}

func TestUpload_SniffsMimeWhenNotDeclared(t *testing.T) {
	s := newTestStore(t, Options{})
	// A minimal PNG signature is enough for http.DetectContentType.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

	for name, contentType := range map[string]string{
		"absent":       "",
		"octet-stream": "application/octet-stream",
	} {
		t.Run(name, func(t *testing.T) {
			desc := decodeDescriptor(t, postMedia(t, s, png, contentType, ""))
			if desc.Mime != "image/png" {
				t.Errorf("mime = %s, want image/png (sniffed)", desc.Mime)
			}
		})
	}
}

func TestUpload_DeclaredMimeWins(t *testing.T) {
	s := newTestStore(t, Options{})
	// Sniffing a WebP that WhatsApp will carry as a sticker must not
	// override what the caller said it is.
	desc := decodeDescriptor(t, postMedia(t, s, []byte("RIFF????WEBPVP8 "), "image/webp", ""))
	if desc.Mime != "image/webp" {
		t.Errorf("mime = %s, want image/webp", desc.Mime)
	}
}

func TestUpload_IsIdempotent(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte("the same bytes twice")

	first := decodeDescriptor(t, postMedia(t, s, body, "application/pdf", "?filename=a.pdf"))
	second := decodeDescriptor(t, postMedia(t, s, body, "application/pdf", "?filename=b.pdf"))

	if first.SHA256 != second.SHA256 {
		t.Fatalf("digests differ: %s vs %s", first.SHA256, second.SHA256)
	}
	// Content-addressed storage means the second POST is a cache hit; the
	// name recorded on first write is the one that stands.
	if second.Filename != "a.pdf" {
		t.Errorf("filename = %s, want the first upload's a.pdf", second.Filename)
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("store holds %v, want exactly one blob + one sidecar", names)
	}
}

func TestUpload_RejectsOversizedBody(t *testing.T) {
	s := newTestStore(t, Options{MaxUploadBytes: 16})

	res := postMedia(t, s, bytes.Repeat([]byte("x"), 17), "application/octet-stream", "")
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.StatusCode)
	}
	_ = res.Body.Close()

	// Exactly at the limit is fine, and nothing from the rejected request
	// may be left behind.
	res = postMedia(t, s, bytes.Repeat([]byte("x"), 16), "application/octet-stream", "")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 at exactly the limit", res.StatusCode)
	}
	desc := decodeDescriptor(t, res)

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("oversized upload left a temp file behind: %s", e.Name())
		}
	}
	if len(entries) != 2 {
		t.Errorf("store holds %d files, want the one accepted blob + sidecar", len(entries))
	}
	if desc.Size != 16 {
		t.Errorf("size = %d, want 16", desc.Size)
	}
}

// An empty body is the caller forgetting --data-binary, not a store
// failure, so it must answer 4xx and leave nothing behind.
func TestUpload_RejectsEmptyBody(t *testing.T) {
	s := newTestStore(t, Options{})
	res := postMedia(t, s, nil, "image/jpeg", "")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty upload", res.StatusCode)
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("store holds %d files after a rejected upload, want none", len(entries))
	}
}

func TestUpload_RejectsGET(t *testing.T) {
	s := newTestStore(t, Options{})
	rec := httptest.NewRecorder()
	uploadMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, UploadRoutePattern, nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "POST") {
		t.Errorf("Allow = %q, want it to list POST", got)
	}
}

func TestUpload_SanitizesFilename(t *testing.T) {
	s := newTestStore(t, Options{})
	// A traversal-shaped name reaches Content-Disposition on the way back
	// out, so it must be flattened on the way in.
	desc := decodeDescriptor(t, postMedia(t, s, []byte("doc"), "application/pdf", "?filename=../../etc/passwd"))
	if desc.Filename != "passwd" {
		t.Errorf("filename = %q, want it stripped to passwd", desc.Filename)
	}
	if strings.ContainsAny(filepath.Base(desc.SHA256), "/\\") {
		t.Errorf("digest is not a bare name: %q", desc.SHA256)
	}
}

func TestPutReader_MatchesPut(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte("identical bytes, two entry points")

	viaPut, err := s.Put(body, "text/plain", "note.txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	viaReader, err := s.PutReader(bytes.NewReader(body), "text/plain", "note.txt")
	if err != nil {
		t.Fatalf("PutReader: %v", err)
	}
	if viaPut != viaReader {
		t.Errorf("descriptors differ:\n put    = %+v\n reader = %+v", viaPut, viaReader)
	}

	f, _, _, err := s.Open(viaReader.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("stored bytes = %q, want %q", got, body)
	}
}
