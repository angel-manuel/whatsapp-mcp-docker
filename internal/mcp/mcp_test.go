package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistry_RegisterRejectsInvalid(t *testing.T) {
	r := NewRegistry()

	cases := map[string]Tool{
		"empty name":  {Name: "", Handler: noopHandler},
		"nil handler": {Name: "x", Handler: nil},
	}
	for label, tool := range cases {
		if err := r.Register(tool); err == nil {
			t.Errorf("%s: expected error, got nil", label)
		}
	}
}

func TestRegistry_RegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	must(t, r.Register(Tool{Name: "dup", Handler: noopHandler}))
	if err := r.Register(Tool{Name: "dup", Handler: noopHandler}); err == nil {
		t.Fatal("expected duplicate registration to error")
	}
}

func TestNotPairedError_HasStableShape(t *testing.T) {
	got := NotPairedError()
	if !got.IsError {
		t.Fatal("NotPairedError must set IsError=true")
	}

	structured, ok := got.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent should be map[string]any, got %T", got.StructuredContent)
	}
	if structured["code"] != string(ErrNotPaired) {
		t.Errorf("code=%v, want %q", structured["code"], ErrNotPaired)
	}
	if structured["message"] != NotPairedMessage {
		t.Errorf("message=%v, want %q", structured["message"], NotPairedMessage)
	}

	if len(got.Content) == 0 {
		t.Fatal("expected at least one Content entry for text-only clients")
	}

	// The text-fallback should be parseable JSON carrying the same
	// code so text-only clients can still recover the reason.
	raw, err := json.Marshal(got.Content[0])
	if err != nil {
		t.Fatalf("marshal Content[0]: %v", err)
	}
	if !strings.Contains(string(raw), string(ErrNotPaired)) {
		t.Errorf("text fallback missing %q: %s", ErrNotPaired, raw)
	}
}

func noopHandler(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
