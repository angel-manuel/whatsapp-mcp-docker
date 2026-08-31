package wa

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// SlogAdapter wraps a *slog.Logger so it satisfies whatsmeow's waLog.Logger
// interface. Without it Config.Logger falls back to waLog.Noop and the entire
// whatsmeow connection lifecycle — auto-reconnect attempts, connect failures,
// stream errors — is discarded, which makes a socket that dies and never comes
// back impossible to diagnose after the fact.
//
// whatsmeow logs printf-style, so msg is a format string; slog wants a plain
// message, so the format is rendered eagerly. Debugf is the chatty one
// (whatsmeow logs every node it sends and receives at debug level) — it is
// gated behind LOG_LEVEL=debug like any other debug record, so the default
// info level keeps the reconnect errors without the firehose.
func SlogAdapter(l *slog.Logger) waLog.Logger {
	if l == nil {
		return waLog.Noop
	}
	return slogAdapter{l: l}
}

// slogAdapter carries the module path assigned by Sub as a "module"
// attribute rather than baking it into the message, so log processors can
// filter on it.
type slogAdapter struct {
	l      *slog.Logger
	module string
}

func (a slogAdapter) Errorf(msg string, args ...any) { a.logf(slog.LevelError, msg, args...) }
func (a slogAdapter) Warnf(msg string, args ...any)  { a.logf(slog.LevelWarn, msg, args...) }
func (a slogAdapter) Infof(msg string, args ...any)  { a.logf(slog.LevelInfo, msg, args...) }
func (a slogAdapter) Debugf(msg string, args ...any) { a.logf(slog.LevelDebug, msg, args...) }

// Sub returns a child adapter tagged with the nested module path.
// whatsmeow calls this per subsystem ("Client", "Client/Send", ...).
func (a slogAdapter) Sub(module string) waLog.Logger {
	if a.module != "" {
		module = a.module + "/" + module
	}
	return slogAdapter{l: a.l, module: module}
}

func (a slogAdapter) logf(level slog.Level, msg string, args ...any) {
	ctx := context.Background()
	// Enabled is checked before Sprintf so the disabled debug firehose
	// costs a comparison rather than a full format of every stanza.
	if !a.l.Enabled(ctx, level) {
		return
	}
	// Only run Sprintf when there is something to substitute. whatsmeow logs
	// plenty of argument-free strings containing a literal % (percentages,
	// encoded paths); Sprintf would rewrite those into "...%!(NOVERB)".
	rendered := msg
	if len(args) > 0 {
		rendered = fmt.Sprintf(msg, args...)
	}
	if a.module == "" {
		a.l.Log(ctx, level, rendered)
		return
	}
	a.l.Log(ctx, level, rendered, slog.String("module", a.module))
}
