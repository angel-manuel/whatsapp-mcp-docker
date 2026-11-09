package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	applog "github.com/angel-manuel/whatsapp-mcp-docker/internal/log"
	"github.com/angel-manuel/whatsapp-mcp-docker/internal/sessions"
)

// maxAdminBody caps a POST /sessions body. It carries one short id.
const maxAdminBody = 4 << 10

// NewHandler builds the full HTTP surface:
//
//   - /mcp, /media, /media/{sha256}: the bearer token selects the session
//     and the request is handed to that session's own handler. An unknown
//     token is 401; nothing is shared between sessions.
//   - /sessions, /sessions/{id}: the admin API, behind ADMIN_TOKEN when it
//     is set and anonymous when it is not.
//   - /healthz: unauthenticated liveness, with per-status session counts
//     and never an id.
func NewHandler(mgr *sessions.Manager, adminToken string, log *slog.Logger) http.Handler {
	h := &handler{mgr: mgr, admin: []byte(adminToken), log: log}
	mux := http.NewServeMux()
	routed := http.HandlerFunc(h.routeToSession)
	mux.Handle("/mcp", routed)
	mux.Handle("/media", routed)
	mux.Handle("/media/", routed)
	mux.Handle("GET /sessions", h.adminAuth(http.HandlerFunc(h.listSessions)))
	mux.Handle("POST /sessions", h.adminAuth(http.HandlerFunc(h.createSession)))
	mux.Handle("DELETE /sessions/{id}", h.adminAuth(http.HandlerFunc(h.deleteSession)))
	mux.HandleFunc("/healthz", h.healthz)
	return mux
}

type handler struct {
	mgr   *sessions.Manager
	admin []byte
	log   *slog.Logger
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	return strings.TrimPrefix(auth, prefix), true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-mcp"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "missing or unknown bearer token")
}

// routeToSession resolves the bearer token to a session and serves the
// request with that session's handler.
func (h *handler) routeToSession(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r)
	if !ok {
		unauthorized(w)
		return
	}
	_, inst, found := h.mgr.Lookup(token)
	if !found {
		unauthorized(w)
		return
	}
	if inst == nil || inst.Handler() == nil {
		writeError(w, http.StatusServiceUnavailable, "session_unavailable",
			"this session failed to open at startup; see the server log")
		return
	}
	inst.Handler().ServeHTTP(w, r)
}

// adminAuth gates the admin API on ADMIN_TOKEN. With no token configured
// the API is anonymous by design: it is defence in depth behind an ingress
// that already restricts who can reach /sessions.
func (h *handler) adminAuth(next http.Handler) http.Handler {
	if len(h.admin) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearer(r)
		if !ok || subtle.ConstantTimeCompare([]byte(got), h.admin) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *handler) listSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": h.mgr.List()})
}

func (h *handler) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", `body must be {"id": "<session id>"}`)
		return
	}
	created, err := h.mgr.Create(r.Context(), body.ID)
	if err != nil {
		h.writeManagerError(w, err)
		return
	}
	// The token is shown once and never stored; make sure no cache keeps it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, created)
}

func (h *handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	deleted, err := h.mgr.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted)
}

func (h *handler) writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrInvalidID):
		writeError(w, http.StatusBadRequest, "invalid_id",
			"session id must match ^[a-z0-9][a-z0-9_-]{0,62}$")
	case errors.Is(err, sessions.ErrExists):
		writeError(w, http.StatusConflict, "exists", "a session with this id already exists")
	case errors.Is(err, sessions.ErrLimit):
		writeError(w, http.StatusConflict, "session_limit", "MAX_SESSIONS reached; delete a session first")
	case errors.Is(err, sessions.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no session with this id")
	default:
		applog.WithEvent(h.log, "sessions.admin").Error("admin request failed",
			slog.String("err", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal", "internal error; see the server log")
	}
}

// healthz reports process liveness. It is 200 whenever the server is up:
// with many sessions, one phone going offline must not mark the whole
// container unhealthy. Per-status counts are included for monitoring; ids
// are not, because this is the one route outside every auth gate.
func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counts := map[sessions.Status]int{}
	for _, info := range h.mgr.List() {
		counts[info.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "sessions": counts})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": msg, "code": errCode})
}
