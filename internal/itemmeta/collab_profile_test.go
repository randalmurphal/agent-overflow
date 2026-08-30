package itemmeta

import (
	"encoding/json"
	"testing"
)

func TestTrimUnverifiedCodexV2Profile(t *testing.T) {
	tests := []struct {
		name            string
		raw             string
		wantChanged     bool
		wantModel       bool
		wantEffort      bool
		wantActivityKey bool
	}{
		{
			name:            "v2 profile",
			raw:             `{"input":{"tool":"spawn_agent","activityKind":"started","model":"gpt-parent","reasoningEffort":"high","agentPath":"/root/reviewer"},"status":"completed"}`,
			wantChanged:     true,
			wantActivityKey: true,
		},
		{
			name:        "v1 effective profile",
			raw:         `{"input":{"tool":"spawn_agent","model":"gpt-child","reasoningEffort":"low"}}`,
			wantModel:   true,
			wantEffort:  true,
			wantChanged: false,
		},
		{
			name:            "v2 no profile",
			raw:             `{"input":{"tool":"spawn_agent","activityKind":"started","agentPath":"/root/reviewer"}}`,
			wantActivityKey: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, changed := TrimUnverifiedCodexV2Profile([]byte(tc.raw))
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			var decoded struct {
				Input map[string]json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(rewritten, &decoded); err != nil {
				t.Fatalf("decode rewritten meta: %v", err)
			}
			_, hasModel := decoded.Input["model"]
			_, hasEffort := decoded.Input["reasoningEffort"]
			_, hasActivityKey := decoded.Input["activityKind"]
			if hasModel != tc.wantModel || hasEffort != tc.wantEffort || hasActivityKey != tc.wantActivityKey {
				t.Fatalf("input keys = %#v", decoded.Input)
			}
			second, changedAgain := TrimUnverifiedCodexV2Profile(rewritten)
			if changedAgain || string(second) != string(rewritten) {
				t.Fatalf("second pass changed fixed point: changed=%v second=%s first=%s", changedAgain, second, rewritten)
			}
		})
	}
}
