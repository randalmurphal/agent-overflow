package itemmeta

import (
	"bytes"
	"encoding/json"
)

var (
	activityKindKey = []byte(`"activityKind"`)
	promptKey       = []byte(`"prompt"`)
)

// TrimEncryptedCollabPrompt removes the model-service ciphertext that
// MultiAgentV2 raw collaboration function calls carry in their message field.
// The Codex adapter identifies canonical V2 rows with input.activityKind;
// legacy collabAgentToolCall rows have no such field and keep their plaintext
// prompt preview.
func TrimEncryptedCollabPrompt(raw []byte) ([]byte, bool) {
	if len(raw) == 0 || !bytes.Contains(raw, activityKindKey) || !bytes.Contains(raw, promptKey) {
		return raw, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return raw, false
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(top["input"], &input); err != nil || input == nil {
		return raw, false
	}
	if _, hasActivityKind := input["activityKind"]; !hasActivityKind {
		return raw, false
	}
	if _, hasPrompt := input["prompt"]; !hasPrompt {
		return raw, false
	}
	delete(input, "prompt")

	encodedInput, err := json.Marshal(input)
	if err != nil {
		return raw, false
	}
	top["input"] = encodedInput
	out, err := json.Marshal(top)
	if err != nil {
		return raw, false
	}
	return out, true
}
