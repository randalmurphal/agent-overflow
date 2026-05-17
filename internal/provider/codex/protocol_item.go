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
		case "send_input", "close_agent", "exitedReviewMode", "contextCompaction", "hookPrompt":
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

	case "item/agentMessage/delta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			return nil, true
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
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
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
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
			TurnID:    readTopLevelString(params, "turnId"),
			ItemID:    readTopLevelString(params, "itemId"),
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
			Meta:           json.RawMessage(`{"blockType":"text"}`),
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
		var meta json.RawMessage
		if itemID != "" {
			marshaled, err := json.Marshal(map[string]string{"provider_item_id": itemID})
			if err == nil {
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
