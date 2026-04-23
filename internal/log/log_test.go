package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew_JSON(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(Options{Level: "info", Format: "json", Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	WithEvent(l, "test.emit").Info("hello", "k", "v")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line not valid JSON: %v (%q)", err, buf.String())
	}
	if got["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", got["msg"])
	}
	if got[EventTypeKey] != "test.emit" {
		t.Errorf("%s = %v, want test.emit", EventTypeKey, got[EventTypeKey])
	}
	if got["k"] != "v" {
		t.Errorf("k = %v, want v", got["k"])
	}
}

func TestNew_Text(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(Options{Level: "debug", Format: "text", Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	WithEvent(l, "t").Debug("msg")
	if !strings.Contains(buf.String(), "event_type=t") {
		t.Errorf("text output missing event_type: %q", buf.String())
	}
}

func TestNew_LevelFilters(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(Options{Level: "warn", Format: "json", Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("should be dropped")
	if buf.Len() != 0 {
		t.Errorf("info record leaked through warn filter: %q", buf.String())
	}
	l.Warn("should pass")
	if buf.Len() == 0 {
		t.Error("warn record was dropped")
	}
}

func TestNew_InvalidLevel(t *testing.T) {
	if _, err := New(Options{Level: "trace", Format: "json"}); err == nil {
		t.Fatal("want error for unknown level")
	}
}

func TestNew_InvalidFormat(t *testing.T) {
	if _, err := New(Options{Level: "info", Format: "xml"}); err == nil {
		t.Fatal("want error for unknown format")
	}
}

func TestNew_DefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(Options{Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("x")
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Errorf("default output should be JSON, got %q", buf.String())
	}
}
