package itemmeta

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func collabMeta(t *testing.T, agentsStates map[string]any) []byte {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"toolName": "wait_agent",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1"},
			"agentsStates":      agentsStates,
		},
	})
}

func decodeAgentsStates(t *testing.T, raw []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Input struct {
			AgentsStates map[string]map[string]json.RawMessage `json:"agentsStates"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("decode trimmed meta: %v", err)
	}
	return top.Input.AgentsStates
}

func TestTrimCollabAgentStateMessages_DropsMessagesKeepsStatus(t *testing.T) {
	heavy := strings.Repeat("finding line\n", 10_000)
	raw := collabMeta(t, map[string]any{
		"child-1": map[string]any{"status": "completed", "message": heavy},
		"child-2": map[string]any{"status": "errored", "message": "boom"},
	})

	trimmed, changed := TrimCollabAgentStateMessages(raw)
	if !changed {
		t.Fatalf("expected trim to report changed")
	}
	if len(trimmed) > 1024 {
		t.Errorf("trimmed meta = %d bytes; expected well under 1 KB", len(trimmed))
	}
	states := decodeAgentsStates(t, trimmed)
	for id, wantStatus := range map[string]string{"child-1": `"completed"`, "child-2": `"errored"`} {
		entry, ok := states[id]
		if !ok {
			t.Fatalf("trim dropped agentsStates entry %s", id)
		}
		if _, hasMessage := entry["message"]; hasMessage {
			t.Errorf("trim left message on %s", id)
		}
		if got := string(entry["status"]); got != wantStatus {
			t.Errorf("status for %s = %s, want %s", id, got, wantStatus)
		}
	}

	// Unrelated keys survive.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		t.Fatalf("decode trimmed top: %v", err)
	}
	if _, ok := top["toolName"]; !ok {
		t.Errorf("trim dropped unrelated top-level keys")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(top["input"], &input); err != nil {
		t.Fatalf("decode trimmed input: %v", err)
	}
	if _, ok := input["receiverThreadIds"]; !ok {
		t.Errorf("trim dropped unrelated input keys")
	}
}

func TestTrimCollabAgentStateMessages_DropsNullAndMessageOnlyEntries(t *testing.T) {
	// The trim deletes the message KEY whenever present — a null
	// message goes too, and a (malformed) message-only entry collapses
	// to an empty object rather than being dropped or erroring.
	raw := collabMeta(t, map[string]any{
		"child-null": map[string]any{"status": "completed", "message": nil},
		"child-only": map[string]any{"message": "orphan"},
	})

	trimmed, changed := TrimCollabAgentStateMessages(raw)
	if !changed {
		t.Fatalf("expected trim to report changed")
	}
	states := decodeAgentsStates(t, trimmed)
	for id, entry := range states {
		if _, hasMessage := entry["message"]; hasMessage {
			t.Errorf("message key survived on %s", id)
		}
	}
	if got := string(states["child-null"]["status"]); got != `"completed"` {
		t.Errorf("status for child-null = %s, want \"completed\"", got)
	}
	if len(states["child-only"]) != 0 {
		t.Errorf("message-only entry should become an empty object, got %v", states["child-only"])
	}
}

func TestTrimCollabAgentStateMessages_FixedPoint(t *testing.T) {
	raw := collabMeta(t, map[string]any{
		"child-1": map[string]any{"status": "completed", "message": "done"},
	})
	trimmed, changed := TrimCollabAgentStateMessages(raw)
	if !changed {
		t.Fatalf("first pass should change")
	}
	again, changedAgain := TrimCollabAgentStateMessages(trimmed)
	if changedAgain {
		t.Fatalf("second pass must be a no-op")
	}
	if !bytes.Equal(trimmed, again) {
		t.Fatalf("second pass mutated bytes:\n first %s\nsecond %s", trimmed, again)
	}
}

func TestTrimCollabAgentStateMessages_LeavesUnchangedShapes(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"no agentsStates":    `{"toolName":"Bash","input":{"command":"ls"}}`,
		"malformed":          `{"input":{"agentsStates":`,
		"bare-string state":  `{"input":{"agentsStates":{"child-1":"running"}}}`,
		"object no message":  `{"input":{"agentsStates":{"child-1":{"status":"running"}}}}`,
		"agentsStates array": `{"input":{"agentsStates":["child-1"]}}`,
		"agentsStates null":  `{"input":{"agentsStates":null}}`,
		"agentsStates empty": `{"input":{"agentsStates":{}}}`,
		"input non-object":   `{"input":"agentsStates"}`,
		"top-level only":     `{"agentsStates":{"child-1":{"status":"completed","message":"x"}}}`,
	}
	for name, raw := range cases {
		trimmed, changed := TrimCollabAgentStateMessages([]byte(raw))
		if changed {
			t.Errorf("%s: reported changed", name)
		}
		if string(trimmed) != raw {
			t.Errorf("%s: bytes mutated:\n got %s\nwant %s", name, trimmed, raw)
		}
	}
}
