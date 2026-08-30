package itemmeta

import (
	"encoding/json"
	"testing"
)

func TestCodexBackgroundRuntimeTransitionsPreserveOwnership(t *testing.T) {
	original := `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"]},"custom":"keep"}`
	ended, err := MarkCodexBackgroundRuntimeEnded(original)
	if err != nil {
		t.Fatalf("mark ended: %v", err)
	}
	active, err := MarkCodexBackgroundRuntimeActive(ended)
	if err != nil {
		t.Fatalf("mark active: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(active), &meta); err != nil {
		t.Fatalf("decode active meta: %v", err)
	}
	if meta["live_background_active"] != true || meta["custom"] != "keep" {
		t.Fatalf("active meta = %v", meta)
	}
	if _, exists := meta["codex_background_end_reason"]; exists {
		t.Fatalf("active transition retained session-end reason: %v", meta)
	}
	input := meta["input"].(map[string]any)
	if input["tool"] != "spawn_agent" {
		t.Fatalf("ownership input changed: %v", input)
	}
}

func TestCodexBackgroundRuntimeTransitionsRejectMalformedMeta(t *testing.T) {
	if _, err := MarkCodexBackgroundRuntimeEnded(`not-json`); err == nil {
		t.Fatal("ended transition accepted malformed meta")
	}
	if _, err := MarkCodexBackgroundRuntimeActive(`[]`); err == nil {
		t.Fatal("active transition accepted a non-object")
	}
}
