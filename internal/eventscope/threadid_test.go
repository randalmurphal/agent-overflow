package eventscope

import (
	"encoding/json"
	"testing"
)

// providerLikeEvent emulates provider.ProviderEvent for the field-lookup
// branch without dragging in the provider package (which would cycle
// since provider's tests depend on store and friends).
type providerLikeEvent struct {
	Kind     string
	ThreadID string
}

func TestThreadIDFromEventMap(t *testing.T) {
	got := ThreadIDFromEvent(map[string]any{"threadId": "abc"})
	if got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

func TestThreadIDFromEventMapString(t *testing.T) {
	got := ThreadIDFromEvent(map[string]string{"threadId": "xyz"})
	if got != "xyz" {
		t.Errorf("got %q, want xyz", got)
	}
}

func TestThreadIDFromEventStructField(t *testing.T) {
	evt := providerLikeEvent{ThreadID: "t-1"}
	if got := ThreadIDFromEvent(evt); got != "t-1" {
		t.Errorf("got %q, want t-1", got)
	}
}

func TestThreadIDFromEventStructLiteral(t *testing.T) {
	// Anonymous struct with json-tagged threadId — reflection finds it
	// via the exported field name first, JSON fallback is unreachable.
	payload := struct {
		ThreadID string `json:"threadId"`
		Payload  string `json:"payload"`
	}{
		ThreadID: "literal-thread",
		Payload:  "p",
	}
	if got := ThreadIDFromEvent(payload); got != "literal-thread" {
		t.Errorf("got %q, want literal-thread", got)
	}
}

func TestThreadIDFromEventJSONFallback(t *testing.T) {
	// Pure JSON payload where reflection finds no match but marshaling does.
	raw := json.RawMessage(`{"threadId":"raw-thread","extra":{"a":1}}`)
	if got := ThreadIDFromEvent(raw); got != "raw-thread" {
		t.Errorf("got %q, want raw-thread", got)
	}
}

func TestThreadIDFromEventAbsent(t *testing.T) {
	if got := ThreadIDFromEvent(map[string]any{"other": "value"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := ThreadIDFromEvent(nil); got != "" {
		t.Errorf("got %q for nil, want empty", got)
	}
	if got := ThreadIDFromEvent("bare string"); got != "" {
		t.Errorf("got %q for string, want empty", got)
	}
}

func TestThreadIDFromEventTrimsWhitespace(t *testing.T) {
	if got := ThreadIDFromEvent(map[string]any{"threadId": "  spaced  "}); got != "spaced" {
		t.Errorf("got %q, want spaced", got)
	}
}

func TestThreadIDFromEventPointer(t *testing.T) {
	evt := &providerLikeEvent{ThreadID: "ptr-thread"}
	if got := ThreadIDFromEvent(evt); got != "ptr-thread" {
		t.Errorf("got %q, want ptr-thread", got)
	}
	var nilPtr *providerLikeEvent
	if got := ThreadIDFromEvent(nilPtr); got != "" {
		t.Errorf("got %q for nil pointer, want empty", got)
	}
}
