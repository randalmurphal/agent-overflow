package itemmeta

import (
	"bytes"
	"encoding/json"
)

var (
	modelKey           = []byte(`"model"`)
	reasoningEffortKey = []byte(`"reasoningEffort"`)
)

// TrimUnverifiedCodexV2Profile removes child profile fields written before
// the Codex adapter queried the child thread's effective configuration.
// MultiAgentV2 rows are identified by input.activityKind. V1 rows carry
// effective profile fields from their typed tool item and are preserved.
func TrimUnverifiedCodexV2Profile(raw []byte) ([]byte, bool) {
	if len(raw) == 0 || !bytes.Contains(raw, activityKindKey) ||
		(!bytes.Contains(raw, modelKey) && !bytes.Contains(raw, reasoningEffortKey)) {
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
	_, hadModel := input["model"]
	_, hadReasoningEffort := input["reasoningEffort"]
	if !hadModel && !hadReasoningEffort {
		return raw, false
	}
	delete(input, "model")
	delete(input, "reasoningEffort")

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
