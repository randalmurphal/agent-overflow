package codex

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// nonToolItemTypes is the deny-list for Codex item type strings that
// arrive via `item/started` / `item/completed` but must NOT be routed
// as tool_call rows. These are content channels with their own
// dedicated triage paths:
//
//   - userMessage:             item/started is noise (the in-flight
//     half has no UI signal); item/completed
//     is special-cased to emit EventUserText
//     so triage's pending-send correlator can
//     stamp the AO-owned `user:<turnIndex>`
//     row (mirrors Claude's `isReplay:true`
//     promotion in parse_user.go).
//   - agentMessage / assistantMessage: written by handleTextDelta from the
//     item/agentMessage/delta stream; settled on
//     item/completed
//   - reasoning:               written by handleThinking from the
//     item/reasoning/*Delta stream; settled on
//     item/completed
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
	"userMessage":       {},
	"agentMessage":      {},
	"assistantMessage":  {},
	"reasoning":         {},
	"plan":              {},
	"todoList":          {},
	"hookPrompt":        {},
	"contextCompaction": {},
	"enteredReviewMode": {},
	"exitedReviewMode":  {},
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
		switch itemType {
		case "enteredReviewMode":
			review := readNestedString(params, "item", "review")
			return []provider.ProviderEvent{{
				Kind:      provider.EventNotification,
				ThreadID:  threadID,
				TurnID:    readTopLevelString(params, "turnId"),
				ItemID:    itemID,
				ItemType:  itemType,
				Content:   reviewStatusSummary("Code review started", review),
				Meta:      mergeMetaKeys(params, map[string]any{"kind": "review_status", "title": "Code review started"}),
				Timestamp: now,
			}}, true
		case "contextCompaction":
			// Compaction start (manual thread/compact/start or auto,
			// codex ≥0.96 — all three core compaction paths emit the
			// item pair). Opens the live compacting window; the matching
			// item/completed arrives below as EventCompactBoundary and
			// closes it. A FAILED compaction never completes its item
			// (core sends an error and abandons it), so triage also
			// clears on turn completion.
			meta, _ := json.Marshal(provider.CompactionStatusMeta{Active: true})
			return []provider.ProviderEvent{{
				Kind:      provider.EventCompactionStatus,
				ThreadID:  threadID,
				TurnID:    readTopLevelString(params, "turnId"),
				ItemID:    itemID,
				ItemType:  itemType,
				Meta:      meta,
				Timestamp: now,
			}}, true
		case "send_input", "close_agent", "exitedReviewMode", "hookPrompt", "subAgentActivity":
			// subAgentActivity: codex >= 0.146's emit_sub_agent_activity
			// (codex-rs/core/src/tools/handlers/multi_agents_v2.rs) fires
			// item/started AND item/completed for the same item, for every
			// activity kind. The completed leg is the whole story —
			// classifySubAgentActivityCompleted synthesizes the begin/end
			// pair for kind "started", a lone completion for "interacted",
			// and a status event for "interrupted". Letting the started leg
			// reach the generic tool branch below mints a raw tool_call row
			// named "subAgentActivity": transient for the two kinds whose
			// completed leg upserts the same item id, but permanent for
			// "interrupted", whose completed leg is a status event and never
			// settles the row — turn-end reconciliation then flips it to
			// errored.
			return nil, true
		}
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

	// The streaming delta cases below decode params ONCE into a raw map
	// and read fields from it — they fire per chunk on the read-loop
	// goroutine, where the former per-field readTopLevelString calls each
	// paid a full map decode plus a copy of the delta payload.
	case "item/agentMessage/delta":
		fields := decodeTopLevel(params)
		delta := readRawString(fields, "delta")
		if delta == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			TurnID:    readRawString(fields, "turnId"),
			ItemID:    readRawString(fields, "itemId"),
			Content:   delta,
			Role:      "assistant",
			Timestamp: now,
		}}, true

	case "item/commandExecution/outputDelta":
		fields := decodeTopLevel(params)
		return []provider.ProviderEvent{{
			Kind:      provider.EventCommandOutput,
			ThreadID:  threadID,
			TurnID:    readRawString(fields, "turnId"),
			ItemID:    readRawString(fields, "itemId"),
			Content:   readRawString(fields, "delta"),
			Meta:      params,
			Timestamp: now,
		}}, true

	case "item/fileChange/outputDelta", "item/fileChange/patchUpdated":
		// outputDelta is the underlying apply_patch tool response, and
		// patchUpdated is a pre-apply structured snapshot. Codex TUI keeps
		// both out of the transcript; the visible edit comes from the
		// fileChange item itself.
		return nil, true

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
	// accumulates into the turn's thinking row. The DELTAS intentionally
	// ignore `contentIndex` / `summaryIndex` — multi-section reasoning is
	// concatenated into the single row, with `summaryPartAdded` below
	// injecting a paragraph break BETWEEN summary sections so they stay
	// readable. That notification does read its `summaryIndex`; see the
	// case for why.
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		fields := decodeTopLevel(params)
		delta := readRawString(fields, "delta")
		if delta == "" {
			delta = readRawString(fields, "text")
		}
		if delta == "" {
			delta = readRawString(readRawObject(fields, "content"), "text")
		}
		if delta == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			TurnID:    readRawString(fields, "turnId"),
			ItemID:    readRawString(fields, "itemId"),
			Content:   delta,
			Timestamp: now,
		}}, true

	// Boundary marker BETWEEN consecutive reasoning-summary sections.
	// Emit a paragraph-break delta so the thinking row reads as
	// separate thoughts rather than one run-on blob — triage appends
	// this onto the same streaming item like any other thinking delta.
	//
	// Codex sends this notification for EVERY summary part, the first
	// one included: the streaming path forwards
	// `ResponseEvent::ReasoningSummaryPartAdded` with no index guard
	// (rust-v0.150.1 codex-rs/core/src/session/turn.rs:2672) and the SSE
	// mapping forwards every `response.reasoning_summary_part.added`
	// (codex-rs/codex-api/src/sse/responses.rs:490). So the break is
	// ours to place, and emitting it unconditionally opened every Codex
	// thinking row with a blank paragraph. Upstream's own
	// sequential-cutoff path guards with the same `> 0` test
	// (turn.rs:2707).
	//
	// `summaryIndex` is a required i64 on the notification
	// (`ReasoningSummaryPartAddedNotification`,
	// codex-rs/app-server-protocol/src/protocol/v2/item.rs:1434), so it
	// is always readable in practice; a missing or unparseable one is
	// treated as 0 and emits nothing. That direction is the safe one —
	// a dropped break is invisible, a spurious leading one is not — and
	// it also means the completed item's `summary` join in
	// `extractCodexReasoningText` still matches the delta stream.
	case "item/reasoning/summaryPartAdded":
		fields := decodeTopLevel(params)
		if index, ok := readRawInt(fields, "summaryIndex"); !ok || index <= 0 {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventThinking,
			ThreadID:  threadID,
			TurnID:    readRawString(fields, "turnId"),
			ItemID:    readRawString(fields, "itemId"),
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

// classifyItemCompleted breaks out item/completed because the plan-item
// branch (itemType=="plan") produces a different event kind than the
// generic tool-complete branch.
func classifyItemCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	item := readNestedObject(params, "item")
	itemID := readRawString(item, "id")
	itemType := classifyCodexItemTypeFromItem(item)
	turnID := readTopLevelString(params, "turnId")
	if itemType == "" {
		return nil
	}
	if itemType == "subAgentActivity" {
		return classifySubAgentActivityCompleted(threadID, params, now)
	}
	switch itemType {
	case "contextCompaction":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			ItemType:  itemType,
			Content:   "Context compacted",
			Meta:      params,
			Timestamp: now,
		}}
	case "exitedReviewMode":
		review := readRawString(item, "review")
		return []provider.ProviderEvent{{
			Kind:      provider.EventNotification,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    itemID,
			ItemType:  itemType,
			Content:   reviewStatusSummary("Code review finished", review),
			Meta:      mergeMetaKeys(params, map[string]any{"kind": "review_status", "title": "Code review finished"}),
			Timestamp: now,
		}}
	case "enteredReviewMode", "hookPrompt":
		return nil
	}
	// Plan is a non-tool type we DO route here — as a proposed-plan event,
	// not as a tool_call completion. Assistant text and reasoning
	// completions are also routed here, but only as content block stops so
	// multi-message Codex turns split transcript rows at the wire boundary.
	// userMessage is the other carve-out: every wire echo of an AO-initiated
	// send (or a future cascade injection like the Codex-side equivalent of
	// task_notification) is promoted to EventUserText so triage's
	// pending-send correlator can stamp the AO-owned `user:<turnIndex>` row.
	// This is the receive-side mirror of Claude's `isReplay:true` promotion
	// in parse_user.go.
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
	switch itemType {
	case "agentMessage", "assistantMessage":
		text, textPresent := readRawStringPresent(item, "text")
		return []provider.ProviderEvent{{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       threadID,
			TurnID:         turnID,
			ItemID:         itemID,
			ItemType:       itemType,
			Content:        text,
			ContentPresent: textPresent,
			Meta:           agentMessageBlockMeta(item),
			Timestamp:      now,
		}}
	case "reasoning":
		text, textPresent := extractCodexReasoningText(item)
		return []provider.ProviderEvent{{
			Kind:           provider.EventContentBlockStop,
			ThreadID:       threadID,
			TurnID:         turnID,
			ItemID:         itemID,
			ItemType:       itemType,
			Content:        text,
			ContentPresent: textPresent,
			Meta:           json.RawMessage(`{"blockType":"thinking"}`),
			Timestamp:      now,
		}}
	}
	if itemType == "userMessage" {
		content := extractCodexUserMessageText(item)
		// item.id is the Codex-assigned uuid for the user envelope. When
		// present it lands in meta as `provider_item_id`; absent / empty
		// omits the key entirely (never empty-string). Phase E reads
		// this to stamp the AO-owned `user:<turnIndex>` row, so the
		// meta key has to be the stable handle, not a synthesized id.
		//
		// `clientId` is the OTHER identity on this item and answers a
		// different question: who authored the message. Upstream types it
		// `Option<String>` on ThreadItem::UserMessage (rust-v0.149.0
		// codex-rs/app-server-protocol/src/protocol/v2/item.rs:236) and
		// threads it from the submitter through
		// TurnInput::UserInput{client_id} → UserMessageItem.client_id
		// (core/src/session/mod.rs:4074), so the echo carries back exactly
		// the `clientUserMessageId` the `turn/start` or `turn/steer` that
		// produced it sent (session_turn.go). That is how the app layer
		// matches an echo to the row it sent without relying on ordering.
		// Absent on turns nobody correlated — a `codex queue` row's own
		// dispatch, say — so the key is omitted rather than empty.
		metaFields := make(map[string]string, 2)
		if itemID != "" {
			metaFields["provider_item_id"] = itemID
		}
		if clientID := readRawString(item, "clientId"); clientID != "" {
			metaFields[userMessageClientIDMetaKey] = clientID
		}
		var meta json.RawMessage
		if len(metaFields) > 0 {
			if marshaled, err := json.Marshal(metaFields); err == nil {
				meta = marshaled
			}
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventUserText,
			ThreadID:  threadID,
			TurnID:    turnID,
			Content:   content,
			Meta:      meta,
			Timestamp: now,
		}}
	}
	if isNonToolCodexItemType(itemType) {
		return nil
	}
	events := make([]provider.ProviderEvent, 0, 2)
	if isCommandExecutionItemType(itemType) {
		if output := readRawString(item, "aggregatedOutput"); output != "" {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventCommandOutput,
				ThreadID:  threadID,
				TurnID:    turnID,
				ItemID:    itemID,
				ItemType:  itemType,
				Content:   output,
				Meta:      enrichItemMetaFromItem(params, item),
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
		Content:   codexToolResultContent(item, itemType),
		Meta:      enrichItemMetaFromItem(params, item),
		Timestamp: now,
	})
	return events
}

type subAgentActivity struct {
	ItemID        string
	Kind          string
	AgentThreadID string
	AgentPath     string
}

// userMessageClientIDMetaKey is where a `userMessage` echo's `clientId`
// lands in ProviderEvent.Meta. Snake_case to match the rest of the Meta
// contract (`provider_item_id`, `item_status`, `process_id`), not the wire's
// camelCase.
const userMessageClientIDMetaKey = "client_id"

func decodeSubAgentActivity(method string, params json.RawMessage) (subAgentActivity, bool) {
	if method != "item/completed" {
		return subAgentActivity{}, false
	}
	return decodeSubAgentActivityItem(readNestedObject(params, "item"))
}

func decodeSubAgentActivityItem(item map[string]json.RawMessage) (subAgentActivity, bool) {
	if item == nil || readRawString(item, "type") != "subAgentActivity" {
		return subAgentActivity{}, false
	}
	activity := subAgentActivity{
		ItemID:        strings.TrimSpace(readRawString(item, "id")),
		Kind:          strings.TrimSpace(readRawString(item, "kind")),
		AgentThreadID: strings.TrimSpace(readRawString(item, "agentThreadId")),
		AgentPath:     strings.TrimSpace(readRawString(item, "agentPath")),
	}
	if activity.ItemID == "" || activity.AgentThreadID == "" {
		return subAgentActivity{}, false
	}
	return activity, true
}

// classifySubAgentActivityCompleted normalizes MultiAgentV2's canonical
// activity item onto the same provider contract used by V1
// collabAgentToolCall items. Codex emits BOTH item/started and
// item/completed for every activity item (emit_sub_agent_activity,
// codex-rs/core/src/tools/handlers/multi_agents_v2.rs); the started leg is
// deliberately dropped in classifyItemNotification because this function is
// the one that expands kind "started" into the begin/end pair the background
// projector already understands. The typed activity supplies the authoritative
// child thread and task path; raw function-call metadata can enrich safe
// fields such as role/model/effort, but V2's encrypted prompt is unavailable
// to clients.
func classifySubAgentActivityCompleted(threadID string, params json.RawMessage, now time.Time) []provider.ProviderEvent {
	activity, ok := decodeSubAgentActivity("item/completed", params)
	if !ok {
		return nil
	}
	turnID := readTopLevelString(params, "turnId")
	switch activity.Kind {
	case "started":
		startMeta := subAgentActivityCollabMeta(params, activity, "spawnAgent", "inProgress", true)
		completeMeta := subAgentActivityCollabMeta(params, activity, "spawnAgent", "completed", true)
		return []provider.ProviderEvent{
			{
				Kind:      provider.EventToolStart,
				ThreadID:  threadID,
				TurnID:    turnID,
				ItemID:    activity.ItemID,
				ItemType:  "collab_agent",
				Meta:      startMeta,
				Timestamp: now,
			},
			{
				Kind:      provider.EventToolComplete,
				ThreadID:  threadID,
				TurnID:    turnID,
				ItemID:    activity.ItemID,
				ItemType:  "collab_agent",
				Meta:      completeMeta,
				Timestamp: now,
			},
		}
	case "interacted":
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    activity.ItemID,
			ItemType:  "send_input",
			Meta:      subAgentActivityCollabMeta(params, activity, "sendInput", "completed", false),
			Timestamp: now,
		}}
	case "interrupted", "completed":
		status := activity.Kind
		meta, err := json.Marshal(map[string]any{
			"agent_path":       activity.AgentThreadID,
			"canonical_path":   activity.AgentPath,
			"status":           status,
			"activity_call_id": activity.ItemID,
		})
		if err != nil {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventSubagentStatus,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    activity.ItemID,
			Meta:      meta,
			Timestamp: now,
		}}
	default:
		return nil
	}
}

func subAgentActivityCollabMeta(params json.RawMessage, activity subAgentActivity, tool, status string, running bool) json.RawMessage {
	item := map[string]any{
		"id":                activity.ItemID,
		"type":              "collabAgentToolCall",
		"tool":              tool,
		"status":            status,
		"receiverThreadIds": []string{activity.AgentThreadID},
		"agentPath":         activity.AgentPath,
		"taskName":          activity.AgentPath,
		"activityKind":      activity.Kind,
	}
	if running {
		item["agentsStates"] = map[string]any{
			activity.AgentThreadID: map[string]string{"status": "running"},
		}
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	var rawItem map[string]json.RawMessage
	if json.Unmarshal(encoded, &rawItem) != nil {
		return nil
	}
	return enrichItemMetaFromItem(params, rawItem)
}

func codexToolResultContent(item map[string]json.RawMessage, itemType string) string {
	switch itemType {
	case "collab_agent", "send_input", "wait_agent", "close_agent", "resume_agent":
		return collabAgentContent(item)
	case "mcpToolCall", "mcp_tool_call":
		return mcpToolCallContent(item)
	case "dynamicToolCall", "dynamic_tool_call":
		return dynamicToolCallContent(item)
	case "imageGeneration", "image_generation":
		return imageGenerationContent(item)
	default:
		return ""
	}
}

func collabAgentContent(item map[string]json.RawMessage) string {
	rawStates := firstRaw(item, "agentsStates", "agents_states")
	var states map[string]json.RawMessage
	if len(rawStates) == 0 || json.Unmarshal(rawStates, &states) != nil {
		return ""
	}
	decoded := make(map[string]collabAgentState, len(states))
	ids := make([]string, 0, len(states))
	for id, raw := range states {
		state := decodeCollabAgentState(raw)
		if state.message != "" {
			decoded[id] = state
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	if len(ids) == 1 {
		return decoded[ids[0]].message
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		state := decoded[id]
		header := "Agent " + id
		if state.status != "" {
			header += " (" + state.status + ")"
		}
		parts = append(parts, header+":\n"+state.message)
	}
	return strings.Join(parts, "\n\n")
}

type collabAgentState struct {
	status  string
	message string
}

func decodeCollabAgentState(raw json.RawMessage) collabAgentState {
	if len(raw) == 0 {
		return collabAgentState{}
	}
	var bare string
	if json.Unmarshal(raw, &bare) == nil {
		return collabAgentState{status: bare}
	}
	var parsed struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return collabAgentState{}
	}
	return collabAgentState{
		status:  parsed.Status,
		message: strings.TrimSpace(parsed.Message),
	}
}

func imageGenerationContent(item map[string]json.RawMessage) string {
	parts := make([]string, 0, 2)
	if prompt := firstNonEmptyString(readRawString(item, "revisedPrompt"), readRawString(item, "revised_prompt")); prompt != "" {
		parts = append(parts, "Revised prompt:\n"+prompt)
	}
	if path := firstNonEmptyString(readRawString(item, "savedPath"), readRawString(item, "saved_path")); path != "" {
		parts = append(parts, "Saved to:\n"+path)
	}
	return strings.Join(parts, "\n\n")
}

func mcpToolCallContent(item map[string]json.RawMessage) string {
	parts := make([]string, 0, 3)
	if errObj := readRawObject(item, "error"); errObj != nil {
		if message := readRawString(errObj, "message"); message != "" {
			parts = append(parts, "Error: "+message)
		}
	}
	if result := readRawObject(item, "result"); result != nil {
		if content := contentBlocksText(result["content"]); content != "" {
			parts = append(parts, content)
		}
		if structured := prettyJSONIfMeaningful(firstRaw(result, "structuredContent", "structured_content")); structured != "" {
			parts = append(parts, "Structured content:\n"+structured)
		}
	}
	return strings.Join(parts, "\n\n")
}

func dynamicToolCallContent(item map[string]json.RawMessage) string {
	return dynamicContentItemsText(firstRaw(item, "contentItems", "content_items"))
}

func contentBlocksText(raw json.RawMessage) string {
	var blocks []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, blockRaw := range blocks {
		var block map[string]json.RawMessage
		if json.Unmarshal(blockRaw, &block) == nil {
			if readRawString(block, "type") == "text" {
				if text := readRawString(block, "text"); text != "" {
					parts = append(parts, text)
					continue
				}
			}
		}
		if compact := compactJSON(blockRaw); compact != "" {
			parts = append(parts, compact)
		}
	}
	return strings.Join(parts, "\n\n")
}

func dynamicContentItemsText(raw json.RawMessage) string {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, itemRaw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(itemRaw, &item) == nil {
			switch readRawString(item, "type") {
			case "inputText":
				if text := readRawString(item, "text"); text != "" {
					parts = append(parts, text)
					continue
				}
			case "inputImage":
				if imageURL := firstNonEmptyString(readRawString(item, "imageUrl"), readRawString(item, "image_url")); imageURL != "" {
					parts = append(parts, "Image: "+imageURL)
					continue
				}
			}
		}
		if compact := compactJSON(itemRaw); compact != "" {
			parts = append(parts, compact)
		}
	}
	return strings.Join(parts, "\n\n")
}

func classifyCodexItemType(params json.RawMessage) string {
	return classifyCodexItemTypeFromItem(readNestedObject(params, "item"))
}

func classifyCodexItemTypeFromItem(item map[string]json.RawMessage) string {
	itemType := readRawString(item, "type")
	if itemType == "fileChange" {
		return "file_change"
	}
	if itemType != "collabAgentToolCall" {
		return itemType
	}
	switch normalizeCollabToolName(readRawString(item, "tool")) {
	case "wait_agent":
		// The parent agent is blocking on one or more children. Surface
		// it as a dedicated item type so the timeline can render it as
		// a "waiting on N agents" card distinct from spawn/send_input.
		return "wait_agent"
	case "send_input":
		return "send_input"
	case "close_agent":
		return "close_agent"
	case "resume_agent":
		return "resume_agent"
	default:
		return "collab_agent"
	}
}

func mergeMap(target map[string]any, source map[string]any) {
	for key, value := range source {
		target[key] = value
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

// AgentMessageDeliveryAsync is the sole variant of Codex's
// `AgentMessageDelivery` enum (codex-rs/protocol/src/items.rs; camelCase on
// the v2 wire as `delivery` on the `agentMessage` ThreadItem,
// protocol/v2/item.rs @ rust-v0.149.0). Upstream documents it as "a
// user-visible message sent without ending the current turn".
//
// Its only producer today is the `send_user_message_async` tool handler
// (codex-rs/core/src/tools/handlers/send_user_message_async.rs), which emits a
// complete agentMessage item MID-TURN and keeps working. The item also carries
// `phase: "finalAnswer"`, so phase alone cannot distinguish it from the real
// final answer — `delivery` is the only signal that this message is an
// interjection rather than the turn's conclusion.
//
// Carried onto the content-block-stop meta for the frontend to render as
// non-final (no UI built here). Absent on every message that is NOT an
// interjection, including every message from a pre-0.149 app-server, so the
// key is omitted rather than defaulted — a reader must not conclude
// "delivery != async ⇒ this is the final answer".
const AgentMessageDeliveryAsync = "async"

// defaultAgentMessageBlockMeta is the pre-existing meta for an agent message
// with no delivery field: the overwhelmingly common case, kept as a constant
// so the hot path allocates nothing.
var defaultAgentMessageBlockMeta = json.RawMessage(`{"blockType":"text"}`)

// agentMessageBlockMeta builds the content-block-stop meta for a completed
// agentMessage, adding `delivery` only when the wire stated one.
func agentMessageBlockMeta(item map[string]json.RawMessage) json.RawMessage {
	delivery := strings.TrimSpace(readRawString(item, "delivery"))
	if delivery == "" {
		return defaultAgentMessageBlockMeta
	}
	meta, err := json.Marshal(map[string]string{"blockType": "text", "delivery": delivery})
	if err != nil {
		return defaultAgentMessageBlockMeta
	}
	return meta
}
