package main

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func TestThreadIDFromEventMap(t *testing.T) {
	got := threadIDFromEvent(map[string]any{"threadId": "abc"})
	if got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

func TestThreadIDFromEventMapString(t *testing.T) {
	got := threadIDFromEvent(map[string]string{"threadId": "xyz"})
	if got != "xyz" {
		t.Errorf("got %q, want xyz", got)
	}
}

func TestThreadIDFromEventProviderEvent(t *testing.T) {
	evt := provider.ProviderEvent{ThreadID: "t-1"}
	if got := threadIDFromEvent(evt); got != "t-1" {
		t.Errorf("got %q, want t-1", got)
	}
}

func TestThreadIDFromEventStructLiteral(t *testing.T) {
	// Anonymous struct with json-tagged threadId, no Go field named ThreadID.
	payload := struct {
		ThreadID string `json:"threadId"`
		Payload  string `json:"payload"`
	}{
		ThreadID: "literal-thread",
		Payload:  "p",
	}
	if got := threadIDFromEvent(payload); got != "literal-thread" {
		t.Errorf("got %q, want literal-thread", got)
	}
}

func TestThreadIDFromEventJSONFallback(t *testing.T) {
	// Pure JSON payload where reflection finds no match but marshaling does.
	raw := json.RawMessage(`{"threadId":"raw-thread","extra":{"a":1}}`)
	if got := threadIDFromEvent(raw); got != "raw-thread" {
		t.Errorf("got %q, want raw-thread", got)
	}
}

func TestThreadIDFromEventAbsent(t *testing.T) {
	if got := threadIDFromEvent(map[string]any{"other": "value"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := threadIDFromEvent(nil); got != "" {
		t.Errorf("got %q for nil, want empty", got)
	}
	if got := threadIDFromEvent("bare string"); got != "" {
		t.Errorf("got %q for string, want empty", got)
	}
}

func TestThreadIDFromEventTrimsWhitespace(t *testing.T) {
	if got := threadIDFromEvent(map[string]any{"threadId": "  spaced  "}); got != "spaced" {
		t.Errorf("got %q, want spaced", got)
	}
}

func TestThreadIDFromEventPointer(t *testing.T) {
	evt := &provider.ProviderEvent{ThreadID: "ptr-thread"}
	if got := threadIDFromEvent(evt); got != "ptr-thread" {
		t.Errorf("got %q, want ptr-thread", got)
	}
	var nilPtr *provider.ProviderEvent
	if got := threadIDFromEvent(nilPtr); got != "" {
		t.Errorf("got %q for nil pointer, want empty", got)
	}
}
