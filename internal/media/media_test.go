package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "media"), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPut_StoresContentAddressedAndIsIdempotent(t *testing.T) {
	s := newTestStore(t, Options{})
	data := []byte("fake jpeg bytes")
	want := digestOf(data)

	desc, err := s.Put(data, "image/jpeg", "image_20240101_120000.jpg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if desc.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", desc.SHA256, want)
	}
	if desc.MediaPath != "/media/"+want {
		t.Errorf("MediaPath = %q", desc.MediaPath)
	}
	if desc.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", desc.Size, len(data))
	}
	if desc.Mime != "image/jpeg" || desc.Filename != "image_20240101_120000.jpg" {
		t.Errorf("descriptor = %+v", desc)
	}

	blob := filepath.Join(s.Dir(), want+".jpg")
	got, err := os.ReadFile(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("blob bytes = %q, want %q", got, data)
	}

	// A repeat call must not rewrite the blob or change the descriptor.
	again, err := s.Put(data, "image/jpeg", "different-name.jpg")
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if again != desc {
		t.Errorf("repeat Put changed descriptor: %+v vs %+v", again, desc)
	}

	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("dir has %d entries, want 2 (blob + sidecar)", len(entries))
	}
}

func TestPut_EmptyMimeAndFilenameGetDefaults(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("x"), "", "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if desc.Mime != DefaultMime {
		t.Errorf("Mime = %q, want %q", desc.Mime, DefaultMime)
	}
	if desc.Filename == "" {
		t.Error("Filename is empty; a download needs a name")
	}
}

func TestLookup_UnknownDigestIsNotFound(t *testing.T) {
	s := newTestStore(t, Options{})
	if _, err := s.Lookup(digestOf([]byte("never stored"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup = %v, want ErrNotFound", err)
	}
	// Non-digest inputs are rejected before any path is built.
	for _, bad := range []string{"", "../../etc/passwd", "abc", "zz" + digestOf(nil)[2:]} {
		if _, err := s.Lookup(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestLookup_AcceptsUppercaseDigest(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("hello"), "text/plain", "hello.txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	upper := ""
	for _, r := range desc.SHA256 {
		if r >= 'a' && r <= 'f' {
			r = r - 'a' + 'A'
		}
		upper += string(r)
	}
	got, err := s.Lookup(upper)
	if err != nil {
		t.Fatalf("Lookup(uppercase): %v", err)
	}
	if got.SHA256 != desc.SHA256 {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, desc.SHA256)
	}
}

func TestNormalizeDigest(t *testing.T) {
	valid := digestOf([]byte("x"))
	cases := []struct {
		in   string
		want bool
	}{
		{valid, true},
		{"", false},
		{valid[:63], false},
		{valid + "a", false},
		{"../" + valid[3:], false},
		{valid[:63] + "g", false},
		{valid[:60] + ".bin", false},
	}
	for _, tc := range cases {
		if _, ok := NormalizeDigest(tc.in); ok != tc.want {
			t.Errorf("NormalizeDigest(%q) ok = %v, want %v", tc.in, ok, tc.want)
		}
	}
}

func TestExtensionForMime(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":              ".jpg",
		"video/mp4":               ".mp4",
		"audio/ogg; codecs=opus":  ".ogg",
		"application/pdf":         ".pdf",
		"application/x-nonsense":  ".bin",
		"":                        ".bin",
		"IMAGE/PNG":               ".png",
		"application/octet-strea": ".bin",
	}
	for in, want := range cases {
		if got := ExtensionForMime(in); got != want {
			t.Errorf("ExtensionForMime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"invoice.pdf":           "invoice.pdf",
		"../../etc/passwd":      "passwd",
		"/abs/path/report.xlsx": "report.xlsx",
		"with\"quote.txt":       "with_quote.txt",
		"line\nbreak.txt":       "linebreak.txt",
		// Backslashes are neutralised rather than treated as separators, so
		// a legitimate name like `AC\DC.mp3` keeps both halves.
		`C:\Windows\evil.exe`: "C:_Windows_evil.exe",
		"":                    "download.bin",
		"..":                  "download.bin",
		"unicode-café.txt":    "unicode-café.txt",
		"   spaced.txt   ":    "spaced.txt",
		"sub/dir/../name.bin": "name.bin",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSweep_TTLEvictsOldBlobs(t *testing.T) {
	s := newTestStore(t, Options{TTL: time.Hour})
	old, err := s.Put([]byte("old"), "image/jpeg", "old.jpg")
	if err != nil {
		t.Fatalf("Put old: %v", err)
	}
	fresh, err := s.Put([]byte("fresh"), "image/jpeg", "fresh.jpg")
	if err != nil {
		t.Fatalf("Put fresh: %v", err)
	}

	backdate(t, s, old.SHA256, ".jpg", time.Now().Add(-2*time.Hour))
	// Reading it does NOT reset its age: TTL measures time since download.
	if _, err := s.Lookup(old.SHA256); err != nil {
		t.Fatalf("Lookup old: %v", err)
	}

	res, err := s.Sweep(time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ExpiredBlobs != 1 {
		t.Errorf("ExpiredBlobs = %d, want 1", res.ExpiredBlobs)
	}
	if _, err := s.Lookup(old.SHA256); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired blob still present: %v", err)
	}
	if _, err := s.Lookup(fresh.SHA256); err != nil {
		t.Errorf("fresh blob evicted: %v", err)
	}
}

func TestSweep_MaxBytesEvictsLeastRecentlyUsed(t *testing.T) {
	// Three 10-byte blobs, cap of 25: exactly one must go, and it must be
	// the least recently used.
	s := newTestStore(t, Options{MaxBytes: 25})
	var digests []string
	for _, body := range []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"} {
		d, err := s.Put([]byte(body), "application/octet-stream", body[:1]+".bin")
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		digests = append(digests, d.SHA256)
	}

	now := time.Now()
	backdate(t, s, digests[0], ".bin", now.Add(-3*time.Hour))
	backdate(t, s, digests[1], ".bin", now.Add(-2*time.Hour))
	backdate(t, s, digests[2], ".bin", now.Add(-1*time.Hour))

	res, err := s.Sweep(now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.EvictedBlobs != 1 {
		t.Fatalf("EvictedBlobs = %d, want 1", res.EvictedBlobs)
	}
	if res.RemainingBytes != 20 {
		t.Errorf("RemainingBytes = %d, want 20", res.RemainingBytes)
	}
	if _, err := s.Lookup(digests[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("oldest blob survived: %v", err)
	}
	for _, d := range digests[1:] {
		if _, err := s.Lookup(d); err != nil {
			t.Errorf("blob %s evicted unexpectedly: %v", d[:8], err)
		}
	}
}

func TestSweep_RemovesOrphansAndStaleTempFiles(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("kept"), "image/jpeg", "kept.jpg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Blob with no sidecar: unreachable through Lookup.
	orphanBlob := filepath.Join(s.Dir(), digestOf([]byte("orphan"))+".bin")
	if err := os.WriteFile(orphanBlob, []byte("orphan"), 0o640); err != nil {
		t.Fatalf("write orphan blob: %v", err)
	}
	// Sidecar with no blob: nothing to serve.
	orphanMeta := filepath.Join(s.Dir(), digestOf([]byte("meta"))+".json")
	if err := os.WriteFile(orphanMeta, []byte(`{"sha256":"x"}`), 0o640); err != nil {
		t.Fatalf("write orphan sidecar: %v", err)
	}
	// Temp file abandoned by a crashed write, older than the grace period.
	stale := filepath.Join(s.Dir(), ".tmp-crashed")
	if err := os.WriteFile(stale, []byte("partial"), 0o640); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes temp: %v", err)
	}

	if _, err := s.Sweep(time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, p := range []string{orphanBlob, orphanMeta, stale} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after sweep", filepath.Base(p))
		}
	}
	if _, err := s.Lookup(desc.SHA256); err != nil {
		t.Errorf("live blob removed by orphan sweep: %v", err)
	}
}

// backdate rewinds both of a stored object's timestamps — the blob's
// (download time, what TTL measures) and the sidecar's (last use, what LRU
// measures) — so retention tests do not have to sleep.
func backdate(t *testing.T, s *Store, digest, ext string, to time.Time) {
	t.Helper()
	for _, name := range []string{digest + ext, digest + ".json"} {
		if err := os.Chtimes(filepath.Join(s.Dir(), name), to, to); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
}

// TestSweep_ServingABlobKeepsItAlive is the difference between LRU and FIFO.
// The oldest-downloaded blob is the one being actively requested; eviction
// must take the blob nobody has asked for instead.
func TestSweep_ServingABlobKeepsItAlive(t *testing.T) {
	s := newTestStore(t, Options{MaxBytes: 25})
	var digests []string
	for _, body := range []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"} {
		d, err := s.Put([]byte(body), "application/octet-stream", body[:1]+".bin")
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		digests = append(digests, d.SHA256)
	}

	now := time.Now()
	for i, d := range digests {
		backdate(t, s, d, ".bin", now.Add(-time.Duration(3-i)*time.Hour))
	}

	// Serve the oldest-downloaded blob over the byte route. That is a use,
	// and it must outrank download order.
	hot := digests[0]
	rec := httptest.NewRecorder()
	serveMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePrefix+hot, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("serve hot blob: status %d", rec.Code)
	}

	if _, err := s.Sweep(time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := s.Lookup(hot); err != nil {
		t.Errorf("the blob being served was evicted: %v", err)
	}
	if _, err := s.Lookup(digests[1]); !errors.Is(err, ErrNotFound) {
		t.Errorf("least-recently-used blob survived: %v", err)
	}
}

// TestOpen_DoesNotMoveLastModified pins the reason recency lives on the
// sidecar rather than the blob: the byte route promises a stable
// Last-Modified for content-addressed, immutable bytes.
func TestOpen_DoesNotMoveLastModified(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("stable"), "text/plain", "s.txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f, _, first, err := s.Open(desc.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = f.Close()

	// Rewind the sidecar far enough that a bug bumping the blob instead
	// would be unmistakable.
	stale := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.Dir(), desc.SHA256+".json"), stale, stale); err != nil {
		t.Fatalf("chtimes sidecar: %v", err)
	}
	f, _, second, err := s.Open(desc.SHA256)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	_ = f.Close()

	if !first.Equal(second) {
		t.Errorf("Last-Modified moved between reads: %s then %s", first, second)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), desc.SHA256+".json"))
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if !info.ModTime().After(stale) {
		t.Errorf("Open did not record the read: sidecar mtime still %s", info.ModTime())
	}
}
