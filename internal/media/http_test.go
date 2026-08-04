package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveMux mounts the handler exactly as internal/server does, so the tests
// exercise the same {sha256} wildcard extraction production uses.
func serveMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(RoutePrefix+"{sha256}", s.Handler())
	return mux
}

func TestServeHTTP_ReturnsBytesAndHeaders(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte("0123456789abcdef")
	desc, err := s.Put(body, "video/mp4", "video_20240101_150405.mp4")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rec := httptest.NewRecorder()
	serveMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePrefix+desc.SHA256, nil))

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	if got := res.Header.Get("Content-Length"); got != "16" {
		t.Errorf("Content-Length = %q, want 16", got)
	}
	wantDisp := `attachment; filename="video_20240101_150405.mp4"`
	if got := res.Header.Get("Content-Disposition"); got != wantDisp {
		t.Errorf("Content-Disposition = %q, want %q", got, wantDisp)
	}
	if got := res.Header.Get("ETag"); got != `"`+desc.SHA256+`"` {
		t.Errorf("ETag = %q", got)
	}
	if res.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified is empty")
	}
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("Cache-Control = %q, want a private directive", got)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

func TestServeHTTP_UnknownDigestIs404(t *testing.T) {
	s := newTestStore(t, Options{})
	rec := httptest.NewRecorder()
	serveMux(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePrefix+digestOf([]byte("nope")), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeHTTP_RejectsNonDigestPathSegments(t *testing.T) {
	s := newTestStore(t, Options{})
	if _, err := s.Put([]byte("secret"), "text/plain", "secret.txt"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Anything that is not a bare 64-char hex digest must be refused
	// without the filesystem ever being consulted. The encoded forms are
	// what an attacker sends to slip a traversal past a naive router.
	bad := []string{
		RoutePrefix + "..%2f..%2fetc%2fpasswd",
		RoutePrefix + "%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		RoutePrefix + "not-a-digest",
		RoutePrefix + strings.Repeat("z", 64),
		RoutePrefix + strings.Repeat("a", 63),
		RoutePrefix,
	}
	mux := serveMux(s)
	for _, target := range bad {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Errorf("GET %s: status = %d, want 404", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("GET %s leaked stored content", target)
		}
	}
}

func TestServeHTTP_RangeRequestReturnsPartialContent(t *testing.T) {
	s := newTestStore(t, Options{})
	body := []byte("0123456789abcdef")
	desc, err := s.Put(body, "application/octet-stream", "blob.bin")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, RoutePrefix+desc.SHA256, nil)
	req.Header.Set("Range", "bytes=4-8")
	rec := httptest.NewRecorder()
	serveMux(s).ServeHTTP(rec, req)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := rec.Body.String(); got != string(body[4:9]) {
		t.Errorf("body = %q, want %q", got, string(body[4:9]))
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 4-8/16" {
		t.Errorf("Content-Range = %q, want bytes 4-8/16", got)
	}
	if got := res.Header.Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want 5", got)
	}
}

func TestServeHTTP_HeadHasNoBodyAndPostIsRejected(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("payload"), "text/plain", "p.txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	mux := serveMux(s)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, RoutePrefix+desc.SHA256, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a body of %d bytes", rec.Body.Len())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RoutePrefix+desc.SHA256, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestServeHTTP_ConditionalRequestReturns304(t *testing.T) {
	s := newTestStore(t, Options{})
	desc, err := s.Put([]byte("cacheable"), "text/plain", "c.txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, RoutePrefix+desc.SHA256, nil)
	req.Header.Set("If-None-Match", `"`+desc.SHA256+`"`)
	rec := httptest.NewRecorder()
	serveMux(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
}

func TestContentDisposition_NonASCIIGetsBothForms(t *testing.T) {
	got := contentDisposition("informe-año.pdf")
	if !strings.Contains(got, `filename="informe-a_o.pdf"`) {
		t.Errorf("missing ASCII fallback in %q", got)
	}
	if !strings.Contains(got, `filename*=UTF-8''informe-a%C3%B1o.pdf`) {
		t.Errorf("missing RFC 5987 form in %q", got)
	}
}

func TestContentDisposition_CannotEscapeTheHeader(t *testing.T) {
	// A document filename is attacker-controlled. Neither a quote nor a
	// CRLF may survive into the header value.
	got := contentDisposition("evil\".txt\r\nX-Injected: 1")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("header contains a line break: %q", got)
	}
	if strings.Count(got, `"`) != 2 {
		t.Fatalf("quotes not neutralised: %q", got)
	}
}
