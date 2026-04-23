package codex

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// ClassifyNotification converts a Codex app-server notification into zero or
// more ProviderEvents. Parent/child linkage for CollabAgent children is
// resolved in session.go, not here: the protocol only gives us child
// provider-thread IDs, so the session owns the receiver-thread -> parent-card
// mapping and stamps ParentToolUseID onto routed child events.
//
// The method catalog is grouped into per-family dispatchers (turn / item /
// thread / account / etc.) so each family's shape lives in one place. Each
// dispatcher returns (events, handled): when handled is false the caller
// falls through to the next group. Adding a new method means editing the
// dispatcher for its family, not the top-level switch.
func ClassifyNotification(threadID, method string, params json.RawMessage) []provider.ProviderEvent {
	now := time.Now()

	if events, ok := classifyTurnNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyItemNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyThreadNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyAccountNotification(threadID, method, params, now); ok {
		return events
	}
	if events, ok := classifyMiscNotification(threadID, method, params, now); ok {
		return events
	}
	return nil
}

// classifyTurnNotification handles `turn/*` methods plus the closely
// related `thread/tokenUsage/updated` (per-turn usage signal).
func classifyTurnNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "turn/started":
		turnID := readNestedString(params, "turn", "id")
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    turnID,
			Timestamp: now,
		}}, true

	case "turn/completed":
		return classifyTurnCompleted(threadID, params, now), true

	case "turn/diff/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "diff"),
			Meta:      params,
			Replace:   true,
			Timestamp: now,
		}}, true

	case "turn/aborted":
		turnID := readNestedString(params, "turn", "id")
		meta, _ := json.Marshal(map[string]any{"aborted": true})
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			TurnID:    turnID,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "turn/plan/updated":
		// Incremental plan updates are intentionally dropped. The finalized
		// plan still lands via item/completed(item.type=plan).
		return nil, true

	case "thread/tokenUsage/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      params,
			Timestamp: now,
		}}, true
	}
	return nil, false
}

// classifyTurnCompleted breaks out the multi-event turn/completed shape.
// A single notification can produce up to three events: error (if the turn
// failed), token-usage (if usage was reported), and a turn-complete with the
// status surfaced into meta.
func classifyTurnCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
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
}

// nonToolItemTypes is the deny-list for Codex item type strings that
// arrive via `item/started` / `item/completed` but must NOT be routed
// as tool_call rows. These are content channels with their own
// dedicated triage paths:
//
//   - userMessage:             persisted by app.PersistItem on SendMessage
//   - agentMessage / assistantMessage: written by handleTextDelta from the
//     item/agentMessage/delta stream; settled on
//     turn/completed
//   - reasoning:               written by handleThinking from the
//     item/reasoning/*Delta stream
//   - plan:                    item/completed is special-cased to emit
//     EventProposedPlan; item/started is noise
//   - todoList:                no triage path yet; suppress rather than
//     pollute the tool_call stream until the
//     renderer knows what to do with it
//
// Routing any of these as EventToolStart creates ghost tool_call rows
// that (a) show up as ToolCallCard in the timeline and (b) thrash the
// sidebar live-status projection running -> idle -> running during
// a turn because every new item/started flips the projection to
// `running` and every item/completed flips it back, producing a
// visible "Completed" pill in the gap between `userMessage` settling
// and `agentMessage` starting.
//
// Codex wire reference: docs/references/codex-wire.md §Item lifecycle.
var nonToolItemTypes = map[string]struct{}{
	"userMessage":      {},
	"agentMessage":     {},
	"assistantMessage": {},
	"reasoning":        {},
	"plan":             {},
	"todoList":         {},
}

func isNonToolCodexItemType(itemType string) bool {
	_, nonTool := nonToolItemTypes[itemType]
	return nonTool
}

// classifyItemNotification handles `item/*` methods — the tool-call
// lifecycle (started/updated/completed) plus the streaming deltas and
// reasoning channels.
func classifyItemNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "item/started":
		itemID := readNestedString(params, "item", "id")
		itemType := classifyCodexItemType(params)
		if itemType == "" || isNonToolCodexItemType(itemType) {
			// Drop non-tool item/started events; they have their own
			// triage channels (see nonToolItemTypes doc above).
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      enrichItemMeta(params),
			Timestamp: now,
		}}, true

	case "item/completed":
		return classifyItemCompleted(threadID, params, now), true

	case "item/agentMessage/delta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   delta,
			Role:      "assistant",
			Timestamp: now,
		}}, true

	case "item/commandExecution/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCommandOutput,
			ThreadID:  threadID,
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}, true

	case "item/fileChange/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}, true

	// Reasoning deltas arrive via one of two mutually-exclusive channels
	// depending on the model class (see
	// codex-rs/app-server/README.md §reasoning notifications):
	//
	//   - `summaryTextDelta` — readable reasoning summaries produced by
	//     OpenAI's proprietary reasoning models (o-series, GPT-5). The
	//     raw chain-of-thought stays hidden; this is the user-facing
	//     summary only.
	//   - `textDelta`        — raw reasoning text emitted by
	//     open-source reasoning models (e.g. DeepSeek-R1). These models
	//     expose the full chain-of-thought directly.
	//
	// A given model emits ONE of these, never both, so routing both to
	// `EventThinking` produces correct UX: whichever channel is active
	// accumulates into the turn's thinking row. We intentionally ignore
	// `contentIndex` / `summaryIndex` — multi-section reasoning is
	// concatenated into the single row (`summaryPartAdded` below
	// injects a paragraph break so sections stay readable).
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			delta = readTopLevelString(params, "text")
		}
		if delta == "" {
			delta = readNestedString(params, "content", "text")
		}
		if delta == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			Content:   delta,
			Timestamp: now,
		}}, true

	// Boundary marker between consecutive reasoning-summary sections.
	// Emit a paragraph-break delta so the thinking row reads as
	// separate thoughts rather than one run-on blob — triage appends
	// this onto the same streaming item like any other thinking delta.
	case "item/reasoning/summaryPartAdded":
		return []provider.ProviderEvent{{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			Content:   "\n\n",
			Timestamp: now,
		}}, true

	case "item/commandExecution/terminalInteraction":
		// Codex emits this whenever the model calls `write_stdin` on a
		// backgrounded unified-exec PTY. Two variants:
		//   - stdin == "": polling-only ("waited for background terminal")
		//   - stdin != "": actual keystrokes were forwarded
		// We surface BOTH variants as EventTerminalInteraction so triage
		// can decide what to persist (Phase 6 only renders the empty
		// variant; the non-empty branch stays parsed so future phases
		// don't need a parser change).
		return []provider.ProviderEvent{{
			Kind:      provider.EventTerminalInteraction,
			ThreadID:  threadID,
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
			Content:   readTopLevelString(params, "stdin"),
			Meta:      buildTerminalInteractionMeta(params),
			Timestamp: now,
		}}, true

	case "item/mcpToolCall/progress",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed":
		return nil, true
	}
	return nil, false
}

// buildTerminalInteractionMeta packages the notification fields triage
// needs to persist the "waited" row. Meta preserves the raw `stdin`
// (so the frontend / future phases can differentiate empty-poll from
// real input) alongside the PTY process_id for debugging.
func buildTerminalInteractionMeta(params json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"process_id": readTopLevelString(params, "processId"),
		"stdin":      readTopLevelString(params, "stdin"),
	})
	if err != nil {
		return params
	}
	return encoded
}

// classifyItemCompleted breaks out item/completed because the plan-item
// branch (itemType=="plan") produces a different event kind than the
// generic tool-complete branch.
func classifyItemCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	itemID := readNestedString(params, "item", "id")
	itemType := classifyCodexItemType(params)
	turnID := readTopLevelString(params, "turnId")
	if itemType == "" {
		return nil
	}
	// Plan is the one non-tool type we DO route here — as a proposed-plan
	// event, not as a tool_call completion. The remaining non-tool types
	// (userMessage / agentMessage / reasoning / todoList) are handled by
	// their own triage paths and must not settle a tool_call row.
	if itemType == "plan" {
		planMarkdown := extractCodexPlanMarkdown(params)
		if planMarkdown == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventProposedPlan,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			ItemType:  itemType,
			Content:   planMarkdown,
			Meta:      params,
			Timestamp: now,
		}}
	}
	if isNonToolCodexItemType(itemType) {
		return nil
	}
	events := make([]provider.ProviderEvent, 0, 2)
	if isCommandExecutionItemType(itemType) {
		if output := readNestedString(params, "item", "aggregatedOutput"); output != "" {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventCommandOutput,
				ThreadID:  threadID,
				TurnID:    turnID,
				ItemID:    itemID,
				ItemType:  itemType,
				Content:   output,
				Meta:      enrichItemMeta(params),
				Replace:   true,
				Timestamp: now,
			})
		}
	}
	events = append(events, provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		TurnID:    turnID,
		ItemID:    itemID,
		ItemType:  itemType,
		Meta:      enrichItemMeta(params),
		Timestamp: now,
	})
	return events
}

// classifyThreadNotification handles `thread/*` methods — the name-change,
// compaction, and lifecycle no-ops.
func classifyThreadNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
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
		}}, true

	case "thread/compacted":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			Meta:      params,
			Timestamp: now,
		}}, true

	case "thread/started",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed":
		return nil, true
	}
	return nil, false
}

// classifyAccountNotification handles `account/*` and `model/*` methods —
// rate-limit refreshes, model reroute signals, and the login/account
// no-ops.
func classifyAccountNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
	case "account/rateLimits/updated":
		meta := normalizeRateLimitsMeta(params, now)
		return []provider.ProviderEvent{{
			Kind:      provider.EventRateLimits,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "model/rerouted":
		toModel := readTopLevelString(params, "toModel")
		meta, _ := json.Marshal(map[string]string{"newModel": toModel})
		return []provider.ProviderEvent{{
			Kind:      provider.EventModelRerouted,
			ThreadID:  threadID,
			Content:   toModel,
			Meta:      meta,
			Timestamp: now,
		}}, true

	case "account/updated", "account/login/completed":
		return nil, true
	}
	return nil, false
}

// classifyMiscNotification handles the remaining grab-bag: errors, server
// request resolution, and pure-informational notices.
func classifyMiscNotification(threadID, method string, params json.RawMessage, now time.Time) ([]provider.ProviderEvent, bool) {
	switch method {
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
			}}, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errorMsg,
			Meta:      params,
			Timestamp: now,
		}}, true

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
		}}, true

	case "configWarning", "deprecationNotice":
		return nil, true
	}
	return nil, false
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
// "agent" | "userShell" | "unifiedExecStartup" | "unifiedExecInteraction"),
// `item_status` (CommandExecutionStatus / McpToolCallStatus / etc. —
// "inProgress" | "completed" | "failed" | "declined"), and `process_id`
// (the PTY handle for unifiedExec commands, optional on the wire). All
// three live inside `params.item`; surfacing them at the top of Meta
// means downstream consumers (the Codex background-terminal projector,
// the renderer) don't need to know the nested path. The original params
// are preserved so anything that already reads `item.xxx` still works.
// If marshaling fails (it shouldn't — we're round-tripping decoded JSON)
// the raw params are returned unmodified rather than dropping data.
func enrichItemMeta(params json.RawMessage) json.RawMessage {
	source := readNestedString(params, "item", "source")
	status := readNestedString(params, "item", "status")
	processID := readNestedString(params, "item", "processId")
	extras := map[string]any{}
	if source != "" {
		extras["source"] = source
	}
	if status != "" {
		extras["item_status"] = status
	}
	if processID != "" {
		extras["process_id"] = processID
	}
	if item := readNestedObject(params, "item"); item != nil {
		if isCommandExecutionItemType(readRawString(item, "type")) {
			extras["toolName"] = "Bash"
			input := map[string]any{}
			if command := readRawString(item, "command"); command != "" {
				input["command"] = command
				extras["command"] = command
			}
			if cwd := readRawString(item, "cwd"); cwd != "" {
				input["cwd"] = cwd
			}
			if exitCode, ok := readRawInt(item, "exitCode"); ok {
				extras["exitCode"] = exitCode
				extras["exit_code"] = exitCode
			}
			if durationMs, ok := readRawInt(item, "durationMs"); ok {
				extras["durationMs"] = durationMs
			}
			if len(input) > 0 {
				extras["input"] = input
			}
		}
		if readRawString(item, "type") == "collabAgentToolCall" {
			tool := normalizeCollabToolName(readRawString(item, "tool"))
			// toolName mirrors the itemType classification so the
			// frontend can pick a renderer without having to inspect
			// input.tool. Keep in sync with classifyCodexItemType's
			// collabAgentToolCall branch.
			toolName := "collab_agent"
			switch tool {
			case "send_input":
				toolName = "send_input"
			case "wait_agent":
				toolName = "wait_agent"
			}
			input := map[string]any{
				"tool": tool,
			}
			if prompt := readRawString(item, "prompt"); prompt != "" {
				input["prompt"] = prompt
			}
			if model := readRawString(item, "model"); model != "" {
				input["model"] = model
			}
			if reasoningEffort := readRawString(item, "reasoningEffort"); reasoningEffort != "" {
				input["reasoningEffort"] = reasoningEffort
			}
			if receiverThreadIDs := readRawStringArray(item, "receiverThreadIds"); len(receiverThreadIDs) > 0 {
				input["receiverThreadIds"] = receiverThreadIDs
			}
			// Surface agentsStates — a map of child thread ID →
			// {status, message?} — that the parent spawn/wait card
			// tracks. See codex-source v2.rs:4462 (`agents_states`)
			// and codex-wire.md §Collab agent lifecycle. The UI can
			// render a live child-status badge from this without
			// needing to subscribe to every child thread's
			// session-status events.
			if agentsStates := readRawJSONObject(item, "agentsStates"); agentsStates != nil {
				input["agentsStates"] = agentsStates
			}
			extras["toolName"] = toolName
			extras["input"] = input
		}
	}
	if len(extras) == 0 {
		return params
	}
	return mergeMetaKeys(params, extras)
}

func classifyCodexItemType(params json.RawMessage) string {
	itemType := readNestedString(params, "item", "type")
	if itemType != "collabAgentToolCall" {
		return itemType
	}
	switch normalizeCollabToolName(readNestedString(params, "item", "tool")) {
	case "wait_agent":
		// The parent agent is blocking on one or more children. Surface
		// it as a dedicated item type so the timeline can render it as
		// a "waiting on N agents" card distinct from spawn/send_input.
		return "wait_agent"
	case "send_input":
		return "send_input"
	default:
		return "collab_agent"
	}
}

func isCommandExecutionItemType(itemType string) bool {
	return itemType == "commandExecution" || itemType == "command_execution"
}

// normalizeCollabToolName folds the wire forms of CollabAgentTool into our
// internal snake_case names. Wire values come from codex-source
// `codex-rs/app-server-protocol/src/protocol/v2.rs` with
// `#[serde(rename_all = "camelCase")]` — so `CollabAgentTool::Wait`
// serializes to the single-word `"wait"`, NOT `"waitAgent"`. The older
// spellings (`WaitAgent`, `waitAgent`, `wait_agent`) are kept as a
// defensive alias set so a future upstream rename or an older server
// doesn't silently fall through to the default branch.
func normalizeCollabToolName(raw string) string {
	switch raw {
	case "SpawnAgent", "spawnAgent", "spawn_agent":
		return "spawn_agent"
	case "SendInput", "sendInput", "send_input":
		return "send_input"
	case "Wait", "wait", "WaitAgent", "waitAgent", "wait_agent":
		return "wait_agent"
	case "CloseAgent", "closeAgent", "close_agent":
		return "close_agent"
	case "ResumeAgent", "resumeAgent", "resume_agent":
		return "resume_agent"
	default:
		return raw
	}
}

func readNestedObject(data json.RawMessage, keys ...string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return nil
		}
		if i == len(keys)-1 {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) != nil {
				return nil
			}
			return nested
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
	}
	return nil
}

func readRawStringArray(m map[string]json.RawMessage, key string) []string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func readRawInt(m map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

// readRawJSONObject decodes the value at key as a `map[string]any` so it
// can be merged back into Meta without losing structure. Returns nil if
// the key is missing, empty, or the value is not a JSON object — a null
// return is indistinguishable from absent, which matches the callers'
// "only include when populated" semantics. Used for agentsStates
// enrichment where we want the nested {status, message?} shape to
// survive the re-encode.
func readRawJSONObject(m map[string]json.RawMessage, key string) map[string]any {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if len(obj) == 0 {
		return nil
	}
	return obj
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
