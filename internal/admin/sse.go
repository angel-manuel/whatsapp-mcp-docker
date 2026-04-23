package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// errSSEUnsupported is returned by sseWriter when the ResponseWriter cannot
// be flushed — SSE is incompatible with buffering reverse proxies / writers.
var errSSEUnsupported = errors.New("streaming unsupported on this ResponseWriter")

// sseWriter serializes "event: X\ndata: {...}\n\n" frames and flushes each.
// The first Send sets SSE headers; subsequent calls just write.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errSSEUnsupported
	}
	return &sseWriter{w: w, flusher: flusher}, nil
}

func (s *sseWriter) Send(event string, data any) error {
	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("sse marshal: %w", err)
	}

	var buf bytes.Buffer
	if event != "" {
		fmt.Fprintf(&buf, "event: %s\n", event)
	}
	// Per spec, a newline inside data must repeat the "data:" field; our
	// payloads are single-line JSON so a straight write is safe.
	fmt.Fprintf(&buf, "data: %s\n\n", payload)

	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
