package wa

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func newCapturingLogger(level slog.Level) (waLog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	return SlogAdapter(l), &buf
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestSlogAdapter_LevelsAndFormatting verifies each waLog level maps to
// the matching slog level and that the printf-style format is rendered.
// whatsmeow logs the reconnect failures we need through Errorf with %v
// arguments, so an adapter that dropped the args would emit "%!v(MISSING)"
// exactly when the log matters most.
func TestSlogAdapter_LevelsAndFormatting(t *testing.T) {
	t.Parallel()

	log, buf := newCapturingLogger(slog.LevelDebug)
	log.Errorf("error reconnecting: %v", "boom")
	log.Warnf("warn %d", 1)
	log.Infof("info")
	log.Debugf("debug %s", "x")

	records := decodeRecords(t, buf)
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4: %v", len(records), records)
	}

	want := []struct{ level, msg string }{
		{"ERROR", "error reconnecting: boom"},
		{"WARN", "warn 1"},
		{"INFO", "info"},
		{"DEBUG", "debug x"},
	}
	for i, w := range want {
		if got := records[i]["level"]; got != w.level {
			t.Errorf("record %d level=%v, want %s", i, got, w.level)
		}
		if got := records[i]["msg"]; got != w.msg {
			t.Errorf("record %d msg=%v, want %q", i, got, w.msg)
		}
	}
}

// TestSlogAdapter_RespectsLevel verifies the debug firehose (whatsmeow
// logs every stanza it sends and receives) is dropped at the default
// info level, while the error records that explain a dead socket survive.
func TestSlogAdapter_RespectsLevel(t *testing.T) {
	t.Parallel()

	log, buf := newCapturingLogger(slog.LevelInfo)
	log.Debugf("every stanza ever")
	log.Errorf("error reconnecting after autoreconnect sleep")

	records := decodeRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (debug dropped): %v", len(records), records)
	}
	if got := records[0]["level"]; got != "ERROR" {
		t.Errorf("level=%v, want ERROR", got)
	}
}

// TestSlogAdapter_SubNestsModule verifies Sub tags records with the
// module path rather than mangling the message, and that nesting joins
// with "/" the way whatsmeow's own stdout logger presents it.
func TestSlogAdapter_SubNestsModule(t *testing.T) {
	t.Parallel()

	log, buf := newCapturingLogger(slog.LevelDebug)
	log.Infof("root")
	log.Sub("Client").Infof("one level")
	log.Sub("Client").Sub("Send").Infof("two levels")

	records := decodeRecords(t, buf)
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if _, ok := records[0]["module"]; ok {
		t.Errorf("root record should carry no module attribute: %v", records[0])
	}
	if got := records[1]["module"]; got != "Client" {
		t.Errorf("module=%v, want Client", got)
	}
	if got := records[2]["module"]; got != "Client/Send" {
		t.Errorf("module=%v, want Client/Send", got)
	}
}

// TestSlogAdapter_NilLoggerIsNoop keeps the zero value safe: wa.Config
// callers that leave Logger unset must not panic on the first record.
func TestSlogAdapter_NilLoggerIsNoop(t *testing.T) {
	t.Parallel()

	log := SlogAdapter(nil)
	if log != waLog.Noop {
		t.Errorf("SlogAdapter(nil) = %v, want waLog.Noop", log)
	}
	// Must not panic.
	log.Errorf("no logger configured")
	log.Sub("Client").Infof("still fine")
}

// TestSlogAdapter_LiteralPercentWithoutArgs verifies a message carrying a
// stray % but no args is passed through untouched rather than run through
// Sprintf, which would corrupt it into "%!(NOVERB)".
func TestSlogAdapter_LiteralPercentWithoutArgs(t *testing.T) {
	t.Parallel()

	log, buf := newCapturingLogger(slog.LevelDebug)
	log.Infof("battery at 80%")

	records := decodeRecords(t, buf)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if got := records[0]["msg"]; got != "battery at 80%" {
		t.Errorf("msg=%v, want %q", got, "battery at 80%")
	}
}
