package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/angel-manuel/whatsapp-mcp-docker/internal/wa"
)

type healthResponse struct {
	Status string `json:"status"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

type statusResponse struct {
	State     string    `json:"state"`
	Connected bool      `json:"connected"`
	LoggedIn  bool      `json:"logged_in"`
	JID       string    `json:"jid,omitempty"`
	Pushname  string    `json:"pushname,omitempty"`
	LastEvent *wa.Event `json:"last_event,omitempty"`
	UptimeS   int64     `json:"uptime_s"`
}

func (s *Server) snapshot() statusResponse {
	st := s.wa.Status()
	return statusResponse{
		State:     string(st.State),
		Connected: st.Connected,
		LoggedIn:  st.LoggedIn,
		JID:       st.JID,
		Pushname:  st.Pushname,
		LastEvent: st.LastEvent,
		UptimeS:   int64(time.Since(s.start).Seconds()),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	st := s.wa.Status()
	if st.Connected && st.LoggedIn {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"status":    "not_ready",
		"state":     string(st.State),
		"connected": st.Connected,
		"logged_in": st.LoggedIn,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sse, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", err.Error())
		return
	}

	ch, unsubscribe := s.wa.Subscribe()
	defer unsubscribe()

	// Prime the stream with the current state so clients don't have to
	// race a separate /admin/status call.
	if err := sse.Send("status", s.snapshot()); err != nil {
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.Send(string(evt.Type), evt); err != nil {
				s.log.Debug("admin: events sse send failed",
					slog.String("err", err.Error()))
				return
			}
		}
	}
}

// handlePairStart serves the SSE pair flow. The MCP `pairing_start` tool
// goes through the same wa.Client.StartPairing entry point, so the two
// surfaces are mutually exclusive: whichever opens a flow first holds
// it; the other receives ErrPairInProgress until the flow ends.
func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	sse, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", err.Error())
		return
	}

	ctx := r.Context()
	ch, err := s.wa.StartPairing(ctx)
	if err != nil {
		// Map sentinel errors to stable codes. Errors emitted before the SSE
		// stream starts are returned as normal JSON so clients can surface a
		// proper HTTP status.
		status, code := classifyPairError(err)
		if !sse.started {
			writeError(w, status, code, err.Error())
			return
		}
		_ = sse.Send(string(wa.PairEventError), wa.PairEvent{
			Type:  wa.PairEventError,
			Error: err.Error(),
		})
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.Send(string(evt.Type), evt); err != nil {
				return
			}
			if evt.IsTerminal() {
				return
			}
		}
	}
}

func classifyPairError(err error) (int, string) {
	switch {
	case errors.Is(err, wa.ErrAlreadyPaired):
		return http.StatusConflict, "already_paired"
	case errors.Is(err, wa.ErrPairInProgress):
		return http.StatusConflict, "pair_in_progress"
	case errors.Is(err, wa.ErrNotPairing):
		return http.StatusBadRequest, "not_pairing"
	default:
		return http.StatusInternalServerError, "pair_failed"
	}
}

type pairPhoneRequest struct {
	Phone string `json:"phone"`
}

type pairPhoneResponse struct {
	LinkingCode string `json:"linking_code"`
}

func (s *Server) handlePairPhone(w http.ResponseWriter, r *http.Request) {
	var body pairPhoneRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.Phone == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "phone is required")
		return
	}

	code, err := s.wa.PairPhone(r.Context(), body.Phone)
	if err != nil {
		status, codeTag := classifyPairError(err)
		writeError(w, status, codeTag, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pairPhoneResponse{LinkingCode: code})
}

func (s *Server) handleUnpair(w http.ResponseWriter, r *http.Request) {
	if err := s.wa.Unpair(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "unpair_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unpaired"})
}
