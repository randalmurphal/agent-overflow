package claude

import (
	"encoding/json"

	"agent-overflow/internal/provider"
)

func extractToolProgressMeta(raw map[string]json.RawMessage) json.RawMessage {
	if progress, ok := raw["progress"]; ok {
		return progress
	}

	content, ok := raw["content"]
	if !ok {
		return json.RawMessage("{}")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(content, &payload); err != nil {
		return content
	}

	if progress, ok := payload["progress"]; ok {
		return progress
	}

	return content
}

func extractCompactBoundaryMeta(raw map[string]json.RawMessage) json.RawMessage {
	for _, key := range []string{"data", "content"} {
		candidate, ok := raw[key]
		if !ok {
			continue
		}
		if meta := normalizeContextWindow(candidate); meta != nil {
			return meta
		}
		return candidate
	}
	return nil
}

func normalizeContextWindow(raw json.RawMessage) json.RawMessage {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	for _, key := range []string{"contextWindow", "context_window"} {
		nested, ok := payload[key]
		if !ok {
			continue
		}
		if meta := normalizeContextWindow(nested); meta != nil {
			return meta
		}
	}

	window, ok := parseContextWindow(payload)
	if !ok {
		return nil
	}

	meta, err := json.Marshal(window)
	if err != nil {
		return nil
	}
	return meta
}

func parseContextWindow(payload map[string]json.RawMessage) (provider.ContextWindow, bool) {
	var window provider.ContextWindow
	var found bool

	if value, ok := readIntValue(payload, "usedTokens", "used_tokens"); ok {
		window.UsedTokens = value
		found = true
	}
	if value, ok := readIntValue(payload, "maxTokens", "max_tokens"); ok {
		window.MaxTokens = value
		found = true
	}
	if value, ok := readFloatValue(payload, "usedPercentage", "used_percentage"); ok {
		window.UsedPercentage = value
		found = true
	}
	if value, ok := readIntValue(payload, "totalProcessed", "total_processed"); ok {
		window.TotalProcessed = value
		found = true
	}

	return window, found
}

func readIntValue(payload map[string]json.RawMessage, keys ...string) (int, bool) {
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}

		var value int
		if err := json.Unmarshal(raw, &value); err == nil {
			return value, true
		}
	}
	return 0, false
}

func readFloatValue(payload map[string]json.RawMessage, keys ...string) (float64, bool) {
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}

		var value float64
		if err := json.Unmarshal(raw, &value); err == nil {
			return value, true
		}
	}
	return 0, false
}
