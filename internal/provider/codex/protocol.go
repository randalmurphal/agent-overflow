package codex

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// ClassifyNotification converts a Codex app-server notification into
// zero or more ProviderEvents. Unrecognized methods are silently skipped.
func ClassifyNotification(threadID, method string, params json.RawMessage) []provider.ProviderEvent {
	now := time.Now()

	switch method {

	// --- Handle and emit ---

	case "turn/started":
		turnID := readNestedString(params, "turn", "id")
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    turnID,
			Timestamp: now,
		}}

	case "turn/completed":
		turnID := readNestedString(params, "turn", "id")
		status := readNestedString(params, "turn", "status")
		errorMsg := readNestedString(params, "turn", "error", "message")

		var events []provider.ProviderEvent

		if status == "failed" && errorMsg != "" {
			events = append(events, provider.ProviderEvent{
				Kind: provider.EventError, ThreadID: threadID, TurnID: turnID, Content: errorMsg, Timestamp: now,
			})
		}

		// Extract usage data if present in the turn/completed notification.
		if usageData := extractUsageFromTurn(params); usageData != nil {
			events = append(events, provider.ProviderEvent{
				Kind: provider.EventTokenUsage, ThreadID: threadID, TurnID: turnID, Meta: usageData, Timestamp: now,
			})
		}

		events = append(events, provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: turnID, Timestamp: now,
		})
		return events

	case "turn/diff/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "diff"),
			Meta:      params,
			Timestamp: now,
		}}

	case "item/started":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      params,
			Timestamp: now,
		}}

	case "item/completed":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      params,
			Timestamp: now,
		}}

	case "item/agentMessage/delta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   delta,
			Role:      "assistant",
			Timestamp: now,
		}}

	case "item/commandExecution/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCommandOutput,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}

	case "item/fileChange/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}

	case "thread/tokenUsage/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      params,
			Timestamp: now,
		}}

	case "error":
		errorMsg := readNestedString(params, "error", "message")
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errorMsg,
			Meta:      params,
			Timestamp: now,
		}}

	case "turn/plan/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "plan_updated",
			Meta:      params,
			Timestamp: now,
		}}

	// --- Explicitly skipped notifications ---

	case "thread/started",
		"thread/status/changed",
		"thread/name/updated",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"thread/compacted",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed",
		"item/reasoning/textDelta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/summaryPartAdded",
		"item/mcpToolCall/progress",
		"serverRequest/resolved",
		"account/updated",
		"account/rateLimits/updated",
		"account/login/completed",
		"model/rerouted",
		"configWarning",
		"deprecationNotice":
		return nil

	default:
		return nil
	}
}

// -- JSON helpers --

// readTopLevelString reads a string from the top level of a JSON object.
func readTopLevelString(data json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// extractUsageFromTurn checks for usage/cost data in a turn/completed notification.
// It looks for turn.usage or top-level usage fields.
// Returns nil if no usage data is found.
func extractUsageFromTurn(params json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return nil
	}

	// Check top-level "usage" field.
	if raw, ok := m["usage"]; ok {
		return raw
	}

	// Check nested turn.usage field.
	if turnRaw, ok := m["turn"]; ok {
		var turn map[string]json.RawMessage
		if json.Unmarshal(turnRaw, &turn) == nil {
			if raw, ok := turn["usage"]; ok {
				return raw
			}
		}
	}

	return nil
}

// readNestedString reads a string by walking through nested object keys.
// E.g., readNestedString(data, "turn", "id") reads data.turn.id.
func readNestedString(data json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			return ""
		}
		if json.Unmarshal(raw, &m) != nil {
			return ""
		}
	}
	return ""
}
