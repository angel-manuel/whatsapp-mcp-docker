package media

import (
	"errors"
	"net/http"
	"strings"
)

// Handler serves stored blobs at RoutePrefix + "{sha256}". It is mounted by
// internal/mcp on the same listener and behind the same bearer auth as
// /mcp — this route is the one capability MCP structurally cannot provide
// (transferring bytes), not a second API surface.
//
// The response deliberately confines everything a client needs to the six
// headers the fronting gateway forwards: Content-Type, Content-Length,
// Content-Disposition, ETag, Last-Modified and Cache-Control.
func (s *Store) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Store) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	digest, ok := NormalizeDigest(digestFromRequest(r))
	if !ok {
		// A non-hex segment is never a real digest — traversal attempts
		// ("../../etc/passwd") land here and are answered like any other
		// unknown object, without disclosing that they were malformed.
		http.NotFound(w, r)
		return
	}

	f, desc, modTime, err := s.Open(digest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", desc.Mime)
	w.Header().Set("Content-Disposition", contentDisposition(desc.Filename))
	// Content-addressed: the bytes behind a digest can never change, so a
	// strong ETag and an immutable cache directive are both safe. private
	// because the route is authenticated.
	w.Header().Set("ETag", `"`+desc.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	// ServeContent gives us Range/206, If-Range, If-None-Match,
	// If-Modified-Since and Content-Length. The name is passed empty so it
	// does not sniff a Content-Type over the one we set above.
	http.ServeContent(w, r, "", modTime, f)
}

// digestFromRequest reads the {sha256} wildcard when the handler was mounted
// on a pattern that declares one, and otherwise falls back to trimming
// RoutePrefix off the path. The fallback keeps the handler usable standalone
// (tests, and any mount that does not use a wildcard pattern).
func digestFromRequest(r *http.Request) string {
	if v := r.PathValue("sha256"); v != "" {
		return v
	}
	return strings.TrimPrefix(r.URL.Path, RoutePrefix)
}

// contentDisposition builds an attachment disposition for name. A non-ASCII
// name gets the RFC 5987 filename* form in addition to a degraded ASCII
// filename, so clients that ignore filename* still receive something usable.
func contentDisposition(name string) string {
	name = SanitizeFilename(name)
	ascii := asciiFallback(name)
	disp := `attachment; filename="` + ascii + `"`
	if ascii != name {
		disp += `; filename*=UTF-8''` + rfc5987Escape(name)
	}
	return disp
}

// asciiFallback replaces every non-ASCII rune with an underscore. The result
// is safe to place inside a quoted-string: SanitizeFilename has already
// removed quotes and control characters.
func asciiFallback(name string) string {
	return strings.Map(func(r rune) rune {
		if r > 0x7f {
			return '_'
		}
		return r
	}, name)
}

const rfc5987Unreserved = "!#$&+-.^_`|~"

// rfc5987Escape percent-encodes s as an RFC 5987 ext-value, which allows a
// narrower character set than URL path escaping does.
func rfc5987Escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlnum := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if isAlnum || strings.IndexByte(rfc5987Unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}
