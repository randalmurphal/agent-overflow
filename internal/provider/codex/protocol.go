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
			Replace:   true,
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
		willRetry := readTopLevelBool(params, "willRetry")
		if willRetry {
			meta, _ := json.Marshal(map[string]any{
				"willRetry": true,
				"error":     json.RawMessage(params),
			})
			return []provider.ProviderEvent{{
				Kind:      provider.EventSessionStatus,
				ThreadID:  threadID,
				Content:   "retrying",
				Meta:      meta,
				Timestamp: now,
			}}
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errorMsg,
			Meta:      params,
			Timestamp: now,
		}}

	case "turn/aborted":
		turnID := readNestedString(params, "turn", "id")
		meta, _ := json.Marshal(map[string]any{"aborted": true})
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			TurnID:    turnID,
			Meta:      meta,
			Timestamp: now,
		}}

	case "item/updated":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      params,
			Replace:   true,
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

	case "serverRequest/resolved":
		requestID := readTopLevelIDString(params, "providerRequestId")
		if requestID == "" {
			requestID = readTopLevelIDString(params, "requestId")
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventApprovalResolved,
			ThreadID:  threadID,
			ItemID:    requestID,
			Meta:      params,
			Timestamp: now,
		}}

	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			delta = readTopLevelString(params, "text")
		}
		if delta == "" {
			delta = readNestedString(params, "content", "text")
		}
		if delta == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			Content:   delta,
			Timestamp: now,
		}}

	case "thread/name/updated":
		name := readTopLevelString(params, "threadName")
		if name == "" {
			name = readTopLevelString(params, "name")
		}
		meta, _ := json.Marshal(map[string]string{"newTitle": name})
		return []provider.ProviderEvent{{
			Kind:      provider.EventThreadRenamed,
			ThreadID:  threadID,
			Content:   name,
			Meta:      meta,
			Timestamp: now,
		}}

	case "account/rateLimits/updated":
		meta := normalizeRateLimitsMeta(params, now)
		return []provider.ProviderEvent{{
			Kind:      provider.EventRateLimits,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}

	case "model/rerouted":
		toModel := readTopLevelString(params, "toModel")
		meta, _ := json.Marshal(map[string]string{"newModel": toModel})
		return []provider.ProviderEvent{{
			Kind:      provider.EventModelRerouted,
			ThreadID:  threadID,
			Content:   toModel,
			Meta:      meta,
			Timestamp: now,
		}}

	case "thread/compacted":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			Meta:      params,
			Timestamp: now,
		}}

	// --- Explicitly skipped notifications ---

	case "thread/started",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed",
		"item/reasoning/summaryPartAdded",
		"item/mcpToolCall/progress",
		"account/updated",
		"account/login/completed",
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

func readTopLevelIDString(data json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	return ""
}

// extractUsageFromTurn checks for usage/cost data in a turn/completed notification.
// It looks for turn.usage or top-level usage fields.
// When usage is found, it computes cost using the model from the turn metadata.
// Returns nil if no usage data is found.
func extractUsageFromTurn(params json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return nil
	}

	var usageRaw json.RawMessage
	var model string

	// Check top-level "usage" field.
	if raw, ok := m["usage"]; ok {
		usageRaw = raw
	}

	// Check nested turn.usage field.
	if usageRaw == nil {
		if turnRaw, ok := m["turn"]; ok {
			var turn map[string]json.RawMessage
			if json.Unmarshal(turnRaw, &turn) == nil {
				if raw, ok := turn["usage"]; ok {
					usageRaw = raw
				}
			}
		}
	}

	if usageRaw == nil {
		return nil
	}

	// Extract model name for cost calculation.
	model = readTopLevelString(params, "model")
	if model == "" {
		model = readNestedString(params, "turn", "model")
	}

	// Parse usage, compute cost, and enrich the data.
	var usage provider.TokenUsage
	if json.Unmarshal(usageRaw, &usage) == nil && model != "" {
		cost := provider.CalculateCost(model, usage)
		if cost > 0 {
			usage.TotalCostUSD = cost
			enriched, err := json.Marshal(usage)
			if err == nil {
				return enriched
			}
		}
	}

	return usageRaw
}

// readTopLevelBool reads a boolean from the top level of a JSON object.
// Returns false if the key is missing or the value is not a boolean.
func readTopLevelBool(data json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	raw, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b
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
