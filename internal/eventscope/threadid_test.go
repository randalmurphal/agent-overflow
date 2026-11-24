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

func TestThreadIDFromEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event any
		want  string
	}{
		{"map", map[string]any{"threadId": "abc"}, "abc"},
		{"string map", map[string]string{"threadId": "xyz"}, "xyz"},
		{"trims whitespace", map[string]any{"threadId": "  spaced  "}, "spaced"},
		{"struct field", providerLikeEvent{ThreadID: "t-1"}, "t-1"},
		// Anonymous struct with json-tagged threadId: reflection finds it
		// via the exported field name first, JSON fallback is unreachable.
		{"struct literal", struct {
			ThreadID string `json:"threadId"`
			Payload  string `json:"payload"`
		}{ThreadID: "literal-thread", Payload: "p"}, "literal-thread"},
		// Pure JSON payload where reflection finds no match but marshaling does.
		{"json fallback", json.RawMessage(`{"threadId":"raw-thread","extra":{"a":1}}`), "raw-thread"},
		{"pointer", &providerLikeEvent{ThreadID: "ptr-thread"}, "ptr-thread"},
		{"nil pointer", (*providerLikeEvent)(nil), ""},
		{"absent key", map[string]any{"other": "value"}, ""},
		{"nil", nil, ""},
		{"bare string", "bare string", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThreadIDFromEvent(tc.event); got != tc.want {
				t.Errorf("ThreadIDFromEvent(%#v) = %q, want %q", tc.event, got, tc.want)
			}
		})
	}
}
