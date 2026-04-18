package codex

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// Codex does not expose a parent/child tool-use linkage in its app-server
// protocol. Claude's streaming SDK surfaces parent_tool_use_id on events
// emitted inside a Task tool invocation so downstream UI can nest the
// sub-agent's output under its parent; Codex has no analogous field on
// item/started, item/updated, or item/completed notifications. We
// therefore leave ProviderEvent.ParentToolUseID empty for every Codex
// event — the absence is intentional, not a bug. If Codex adds this
// concept in the future, extract the id here and populate the field.
//
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

		// Surface turn.status so downstream can distinguish completed /
		// interrupted / failed turns (wire values: "completed" | "interrupted"
		// | "failed" | "inProgress" — see codex-source TurnStatus.ts).
		turnMeta := mergeMetaKeys(params, map[string]any{
			"turn_status": status,
		})

		events = append(events, provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: turnID, Meta: turnMeta, Timestamp: now,
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
			Meta:      enrichItemMeta(params),
			Timestamp: now,
		}}

	case "item/completed":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		if itemType == "plan" {
			planMarkdown := extractCodexPlanMarkdown(params)
			if planMarkdown == "" {
				return nil
			}
			return []provider.ProviderEvent{{
				Kind:      provider.EventProposedPlan,
				ThreadID:  threadID,
				ItemID:    itemID,
				ItemType:  itemType,
				Content:   planMarkdown,
				Meta:      params,
				Timestamp: now,
			}}
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      enrichItemMeta(params),
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
		// Stream the incremental plan update as a dedicated EventPlanUpdate.
		// Previously routed as EventSessionStatus with content="plan_updated",
		// which collided with the generic status string and prevented the
		// frontend from reacting to plan updates distinctly. The full
		// finalized plan still arrives via item/completed (itemType=plan)
		// → EventProposedPlan; this kind is for the intermediate steps.
		return []provider.ProviderEvent{{
			Kind:      provider.EventPlanUpdate,
			ThreadID:  threadID,
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

	// item/commandExecution/terminalInteraction and item/mcpToolCall/progress
	// both map to EventToolProgress with a `subtype` discriminator in Meta.
	// We collapse two wire notifications onto one EventKind deliberately —
	// neither has enough downstream-rendering baggage to justify its own
	// EventKind today, and giving each its own would require adding entries
	// to AllEventKinds plus frontend `never`-guard branches. If downstream
	// rendering ever diverges (e.g. terminal interactions want a dedicated
	// timeline affordance), promote either to its own EventKind by adding
	// to AllEventKinds in internal/provider/types.go and adding a branch in
	// the frontend router.
	case "item/commandExecution/terminalInteraction":
		itemID := readTopLevelString(params, "itemId")
		turnID := readTopLevelString(params, "turnId")
		processID := readTopLevelString(params, "processId")
		stdin := readTopLevelString(params, "stdin")
		meta, _ := json.Marshal(map[string]any{
			"subtype":   "terminal_interaction",
			"processId": processID,
			"stdin":     stdin,
		})
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolProgress,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			Meta:      meta,
			Timestamp: now,
		}}

	case "item/mcpToolCall/progress":
		itemID := readTopLevelString(params, "itemId")
		turnID := readTopLevelString(params, "turnId")
		message := readTopLevelString(params, "message")
		meta, _ := json.Marshal(map[string]any{
			"subtype": "mcp_progress",
			"message": message,
		})
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolProgress,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			Meta:      meta,
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

func extractCodexPlanMarkdown(data json.RawMessage) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}

	candidates := []string{
		readNestedString(data, "item", "text"),
		readNestedString(data, "item", "summary"),
		readNestedString(data, "item", "title"),
		readNestedString(data, "item", "result", "text"),
		readNestedString(data, "item", "result", "summary"),
		readTopLevelString(data, "text"),
		readTopLevelString(data, "summary"),
		readTopLevelString(data, "message"),
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}

	var item map[string]json.RawMessage
	if rawItem, ok := payload["item"]; ok && json.Unmarshal(rawItem, &item) == nil {
		if rawResult, ok := item["result"]; ok {
			var result map[string]json.RawMessage
			if json.Unmarshal(rawResult, &result) == nil {
				if command := readRawString(result, "command"); command != "" {
					return command
				}
			}
		}
	}

	return ""
}

func readRawString(m map[string]json.RawMessage, key string) string {
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

// enrichItemMeta augments the raw item/started or item/completed params with
// discoverable top-level keys: `source` (CommandExecutionSource —
// "agent" | "userShell" | "unifiedExecStartup" | "unifiedExecInteraction")
// and `item_status` (CommandExecutionStatus / McpToolCallStatus / etc. —
// "inProgress" | "completed" | "failed" | "declined"). Both live inside
// `params.item` on the wire; surfacing them at the top of Meta means
// downstream consumers don't need to know the nested path. The original
// params are preserved so anything that already reads `item.xxx` still
// works. If marshaling fails (it shouldn't — we're round-tripping decoded
// JSON) the raw params are returned unmodified rather than dropping data.
func enrichItemMeta(params json.RawMessage) json.RawMessage {
	source := readNestedString(params, "item", "source")
	status := readNestedString(params, "item", "status")
	if source == "" && status == "" {
		return params
	}
	extras := map[string]any{}
	if source != "" {
		extras["source"] = source
	}
	if status != "" {
		extras["item_status"] = status
	}
	return mergeMetaKeys(params, extras)
}

// mergeMetaKeys decodes base as a JSON object, overlays extras on top, and
// returns the re-encoded result. If base is not a JSON object (or decoding
// fails) we fall back to marshaling extras alone so the enrichment keys are
// still present. Used by turn/completed and item enrichment to carry both
// raw wire fields and synthesized top-level keys in the same Meta blob.
func mergeMetaKeys(base json.RawMessage, extras map[string]any) json.RawMessage {
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil || merged == nil {
		merged = map[string]any{}
	}
	for k, v := range extras {
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		// Shouldn't happen: merged is built from decoded JSON + known-safe
		// values. Preserve base rather than drop everything.
		return base
	}
	return out
}
