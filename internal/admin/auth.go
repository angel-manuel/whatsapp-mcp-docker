package admin

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authed wraps next with a bearer-token check. When the server has no
// AuthToken configured and RequireAuth is false, requests pass through. When
// RequireAuth is true but AuthToken is empty, every request is rejected —
// this avoids silently serving an open admin surface on misconfiguration.
func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" && !s.cfg.RequireAuth {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.AuthToken == "" {
			// RequireAuth without a token — refuse everything. This should
			// not happen in production because config validation rejects it,
			// but defend-in-depth.
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"admin auth is required but no AUTH_TOKEN is configured")
			return
		}

		got, ok := bearerFromRequest(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"missing or malformed Authorization header")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AuthToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerFromRequest extracts the Bearer token from an Authorization header.
// The scheme match is case-insensitive per RFC 7235; the token itself is
// returned verbatim.
func bearerFromRequest(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
