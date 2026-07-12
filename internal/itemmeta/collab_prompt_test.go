package itemmeta

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestTrimEncryptedCollabPromptRemovesOnlyV2Prompt(t *testing.T) {
	raw := []byte(`{"toolName":"collab_agent","input":{"tool":"spawn_agent","activityKind":"started","prompt":"gAAAA-encrypted","agentPath":"/root/reviewer"}}`)
	trimmed, changed := TrimEncryptedCollabPrompt(raw)
	if !changed {
		t.Fatal("expected V2 prompt trim")
	}
	var decoded struct {
		Input map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		t.Fatalf("decode trimmed meta: %v", err)
	}
	if _, ok := decoded.Input["prompt"]; ok {
		t.Fatalf("prompt survived trim: %s", trimmed)
	}
	if got := string(decoded.Input["agentPath"]); got != `"/root/reviewer"` {
		t.Fatalf("agentPath = %s", got)
	}

	again, changedAgain := TrimEncryptedCollabPrompt(trimmed)
	if changedAgain || !bytes.Equal(again, trimmed) {
		t.Fatal("trim must be a fixed point")
	}
}

func TestTrimEncryptedCollabPromptPreservesLegacyPlaintext(t *testing.T) {
	raw := []byte(`{"toolName":"collab_agent","input":{"tool":"spawn_agent","prompt":"Review the parser"}}`)
	trimmed, changed := TrimEncryptedCollabPrompt(raw)
	if changed || !bytes.Equal(trimmed, raw) {
		t.Fatalf("legacy prompt changed: %s", trimmed)
	}
}
