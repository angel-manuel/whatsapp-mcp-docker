// Package log configures a structured logger (log/slog) for the application.
// Callers tag each record with an event_type attribute via WithEvent so
// downstream log processors can rely on that key being present.
package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// EventTypeKey is the attribute key every structured record should carry.
const EventTypeKey = "event_type"

// Options controls logger construction.
type Options struct {
	// Level is one of debug, info, warn, error. Empty means info.
	Level string
	// Format is "json" or "text". Empty means json.
	Format string
	// Writer is the destination. Defaults to os.Stdout.
	Writer io.Writer
}

// New returns a slog.Logger configured with opts. The returned logger has no
// preset attributes; call WithEvent to scope records to a specific event type.
func New(opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	handlerOpts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch strings.ToLower(opts.Format) {
	case "", "json":
		handler = slog.NewJSONHandler(w, handlerOpts)
	case "text":
		handler = slog.NewTextHandler(w, handlerOpts)
	default:
		return nil, fmt.Errorf("unknown log format %q", opts.Format)
	}

	return slog.New(handler), nil
}

// WithEvent returns a child logger whose records carry event_type=eventType.
func WithEvent(l *slog.Logger, eventType string) *slog.Logger {
	return l.With(slog.String(EventTypeKey, eventType))
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}
