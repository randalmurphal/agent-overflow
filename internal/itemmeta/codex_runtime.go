package itemmeta

import (
	"encoding/json"
	"fmt"
)

const CodexBackgroundEndReasonSessionEnded = "session_ended"

// MarkCodexBackgroundRuntimeEnded records the only fact AO can prove after
// the owning app-server exits: the work is no longer live. It does not invent
// a child completion result, and it preserves spawn ownership metadata so a
// later session can resume or recover mailbox history.
func MarkCodexBackgroundRuntimeEnded(raw string) (string, error) {
	meta, err := decodeRuntimeObject(raw)
	if err != nil {
		return "", err
	}
	meta["live_background_active"] = json.RawMessage(`false`)
	meta["codex_background_end_reason"] = json.RawMessage(`"` + CodexBackgroundEndReasonSessionEnded + `"`)
	return encodeObject(meta)
}

// MarkCodexBackgroundRuntimeActive reverses the session-end projection when a
// later typed child turn/started notification proves work is live again.
func MarkCodexBackgroundRuntimeActive(raw string) (string, error) {
	meta, err := decodeRuntimeObject(raw)
	if err != nil {
		return "", err
	}
	meta["live_background_active"] = json.RawMessage(`true`)
	delete(meta, "codex_background_end_reason")
	return encodeObject(meta)
}

func decodeRuntimeObject(raw string) (map[string]json.RawMessage, error) {
	if raw == "" {
		return make(map[string]json.RawMessage), nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil, fmt.Errorf("decode item meta: %w", err)
	}
	if meta == nil {
		return nil, fmt.Errorf("decode item meta: expected JSON object")
	}
	return meta, nil
}

func encodeObject(meta map[string]json.RawMessage) (string, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("encode item meta: %w", err)
	}
	return string(encoded), nil
}
