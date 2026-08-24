package rollout

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// `event_msg/item_completed` — the record set a PAGINATED rollout is built
// out of.
//
// Codex 0.147 introduced `ThreadHistoryMode`. In `legacy` mode a rollout
// persists the old per-family event records (`user_message`, `agent_message`,
// `agent_reasoning`, `patch_apply_end`, `mcp_tool_call_end`, `web_search_end`,
// `sub_agent_activity`, `entered_review_mode`, `exited_review_mode`,
// `context_compacted`) and only two item variants — Plan and the `clock.sleep`
// extension item. In `paginated` mode that is inverted: NONE of those legacy
// events are written and EVERY turn item is, as one
// `event_msg/item_completed` line (codex-rs/rollout/src/policy.rs
// `should_persist_event_msg`). A reader that only knew the legacy set imported
// a paginated thread with no tool detail, no diffs, no MCP results and no
// sub-agent activity at all.
//
// This file maps each variant onto the SAME emit helpers the legacy branch
// uses for its counterpart — `applyEndEvent` for anything tool-shaped,
// `emitNotification` for lifecycle rows — so a paginated import and a legacy
// import of the same conversation produce the same rows. It deliberately does
// not build a second payload vocabulary.
//
// ## What is NOT emitted here, and why
//
// UserMessage, HookPrompt, AgentMessage and Reasoning are recognised and
// SKIPPED. Their content is already in the file as `response_item` lines,
// which `should_persist_response_item` persists unconditionally in BOTH
// modes, and the mirror is the better source in every case:
//
//   - It is complete. On a real paginated rollout every AgentMessage and
//     Reasoning item has a `response_item` twin carrying the same id, written
//     on the very next line (verified 2224/2224 on a 12k-line 0.147 file).
//   - It is the ONLY source for user text. A native paginated rollout writes
//     no UserMessage items at all; the user's prompt exists only as
//     `response_item/message` with `role:"user"`.
//   - Items cannot be deduplicated against it by id in a MIGRATED file. The
//     legacy→paginated migration mints fresh item ids for the items it
//     synthesises (rollout_migration/canonicalizer.rs `next_item_id`), so a
//     migrated thread's UserMessage item and its surviving `response_item`
//     twin share no key at all. Preferring the mirror is the only rule that
//     is correct for native-paginated, migrated, and mixed files alike.
//   - Reasoning items are not even the better text. The migration writes ONE
//     item per reasoning chunk, each re-stating the whole accumulated
//     `summary_text` under the same id, so emitting them would triple a
//     three-chunk thought; and a native paginated file's Reasoning items
//     carry EMPTY `summary_text`/`raw_content` (the readable summary lives on
//     the `response_item`).
//
// The corresponding suppression on the other side lives in dispatch.go:
// `convertMessage` / `convertReasoning` stop honouring the legacy
// `hasEventMsgMessage` / `hasEventMsgReasoning` dedup when the file is
// paginated, because in that mode the legacy twin they defer to does not
// exist.

// extensionItemKinds maps ExtensionItem's `kind` tag onto the TurnItem
// variant name the dispatch below already handles. ExtensionItem is flattened
// into `TurnItem::Extension`, so these arrive as `"type":"Extension"` plus a
// `kind` (codex-rs/ext/items/src/lib.rs).
var extensionItemKinds = map[string]string{
	"web.search":           "websearch",
	"clock.sleep":          "sleep",
	"image_gen.generation": "imagegeneration",
}

// applyItemCompleted converts one `event_msg/item_completed` line.
func (c *converter) applyItemCompleted(env envelope) {
	var p itemCompletedPayload
	if json.Unmarshal(env.Payload, &p) != nil || len(p.Item) == 0 {
		c.corrupt++
		return
	}
	// The DISCRIMINATORS first, on their own. The four kinds this reader
	// drops (`usermessage` / `hookprompt` / `agentmessage` / `reasoning`) are
	// the highest-volume items in a paginated rollout by a wide margin, and
	// every one of them is dropped here because the `response_item` mirror
	// owns that content. Decoding the whole turnItem for them — its content
	// slices, changes map, receiver lists and agent-state map — is pure
	// allocation on the hottest path in the importer. A malformed item in
	// that set is dropped without a corrupt count for the same reason: the
	// bytes are never read.
	var head struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(p.Item, &head) != nil {
		c.corrupt++
		return
	}
	// Matched case-insensitively on purpose. A ROLLOUT spells these
	// PascalCase (TurnItem's serde tag carries no `rename_all`), while the
	// app-server v2 `ThreadItem` mirror of the same data is camelCase and
	// pre-0.147 files wrote a couple of them lowercase. Case is the one
	// axis on which the same variant has been spelled three ways, and none
	// of the three should decide whether a tool row exists.
	name := strings.ToLower(strings.TrimSpace(head.Type))
	if name == "extension" {
		mapped, ok := extensionItemKinds[strings.ToLower(strings.TrimSpace(head.Kind))]
		if !ok {
			c.unknown["event_msg/item_completed/Extension/"+strings.TrimSpace(head.Kind)]++
			return
		}
		name = mapped
	}
	switch name {
	// --- content the `response_item` mirror already owns (see the file
	// comment). Recognised and dropped, never counted as unknown.
	case "usermessage", "hookprompt", "agentmessage", "reasoning":
		return
	}

	var item turnItem
	if json.Unmarshal(p.Item, &item) != nil {
		c.corrupt++
		return
	}

	switch name {
	case "plan":
		c.applyPlanItem(p, item)
	case "sleep":
		c.emitNotification(sleepSummary(sleepDurationMS(item)), map[string]any{"kind": "sleep"}, "")
	case "contextcompaction":
		// The durable `compacted` record carries the summary and always
		// precedes this item, exactly as it precedes the legacy
		// `context_compacted` twin. Same rule, same flag.
		if c.sawCompacted {
			return
		}
		c.emitCompactionBoundary("", nil)

	case "commandexecution":
		c.applyCommandExecutionItem(p, item)
	case "filechange":
		c.applyFileChangeItem(p, item)
	case "mcptoolcall":
		c.applyMCPToolCallItem(p, item)
	case "dynamictoolcall":
		c.applyDynamicToolCallItem(p, item)
	case "websearch":
		c.applyWebSearchItem(p, item)
	case "imageview":
		c.applyImageViewItem(p, item)
	case "imagegeneration":
		c.applyImageGenerationItem(p, item)
	case "collabagenttoolcall":
		c.applyCollabAgentToolCallItem(p, item)

	case "subagentactivity":
		c.applySubAgentActivityItem(item)
	case "enteredreviewmode":
		c.beginReviewItem(p, item)
	case "exitedreviewmode":
		c.exitReviewItem(item)

	default:
		// The enum is open and the installed CLI is routinely ahead of any
		// checkout: an unrecognised variant is counted, never fatal.
		c.unknown["event_msg/item_completed/"+head.Type]++
	}
}

func (c *converter) applyPlanItem(p itemCompletedPayload, item turnItem) {
	if strings.TrimSpace(item.Text) == "" {
		return
	}
	c.ensureTurn()
	c.emit(provider.ProviderEvent{
		Kind:      provider.EventProposedPlan,
		TurnID:    strings.TrimSpace(p.TurnID),
		ItemID:    item.ID,
		ItemType:  "plan",
		Content:   item.Text,
		Meta:      p.Item,
		Timestamp: c.itemCompletedAt(p),
	})
}

// applyCommandExecutionItem is the paginated counterpart of
// `exec_command_end` (applyExecCommandEnd). `CommandExecutionItem.id` IS the
// call id — upstream's own `as_legacy_end_event` copies it straight into
// `ExecCommandEndEvent.call_id` — so it correlates through the same call_id
// machinery as everything else, including the synthetic `exec-<uuid>` ids a
// command run from inside an `exec` script gets.
func (c *converter) applyCommandExecutionItem(p itemCompletedPayload, item turnItem) {
	if commandExecutionInFlight(item.Status) {
		// `in_progress` is a live-stream state. A rollout only persists the
		// completion, but a truncated/rewritten one can still hold it, and
		// settling it as finished would invent an exit status.
		return
	}
	ev := endEvent{
		callID:   item.ID,
		toolName: "Bash",
		itemType: "commandExecution",
		what:     "command result",
		enrich: toolEnrichment{
			cwd:      pathFromURI(item.Cwd),
			exitCode: item.ExitCode,
			isError:  item.ExitCode != nil && *item.ExitCode != 0,
			output:   firstNonEmpty(item.AggregatedOutput, item.Stdout, item.Stderr),
			extra:    map[string]any{},
		},
	}
	if len(item.Command) > 0 {
		ev.enrich.command = strings.Join(item.Command, " ")
	}
	if item.Source != "" {
		ev.enrich.extra["source"] = item.Source
	}
	if status := commandExecutionStatus(item.Status); status != "" {
		ev.enrich.itemStatus = status
		if status == "failed" {
			ev.enrich.isError = true
		}
	}
	c.applyItemEndEvent(p, ev)
}

func commandExecutionInFlight(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "in_progress")
}

// commandExecutionStatus maps CommandExecutionStatus onto the `item_status`
// vocabulary triage already decodes. `declined` is a refusal, which triage
// spells `errored` via its `failed`/`errored` arm — the row must not read as
// a clean completion.
func commandExecutionStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return "completed"
	case "failed", "declined":
		return "failed"
	default:
		return ""
	}
}

// applyFileChangeItem is the paginated counterpart of `patch_apply_end`.
func (c *converter) applyFileChangeItem(p itemCompletedPayload, item turnItem) {
	ev := endEvent{
		callID:   item.ID,
		toolName: "file_change",
		itemType: "fileChange",
		what:     "patch result",
		enrich: toolEnrichment{
			output:    firstNonEmpty(item.Stdout, item.Stderr),
			diffPatch: assembleUnifiedPatch(item.Changes),
			extra:     map[string]any{},
		},
	}
	// PatchApplyStatus; absent means the apply never settled, which
	// upstream reports by emitting no legacy end event at all.
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "failed", "declined":
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
	case "completed":
		ev.enrich.itemStatus = "completed"
	}
	if paths := changedPaths(item.Changes); len(paths) > 0 {
		ev.enrich.extra["files"] = paths
		ev.input = map[string]any{}
		if len(paths) == 1 {
			ev.input["file_path"] = paths[0]
		} else {
			ev.input["files"] = paths
		}
	}
	c.applyItemEndEvent(p, ev)
}

// applyMCPToolCallItem is the paginated counterpart of `mcp_tool_call_end`.
// The item's `result` is the CallToolResult directly (not the legacy
// Ok/Err-tagged wrapper), and a failure is a separate `error` object.
func (c *converter) applyMCPToolCallItem(p itemCompletedPayload, item turnItem) {
	ev := endEvent{
		callID:   item.ID,
		toolName: "mcp_tool_call",
		itemType: "mcpToolCall",
		what:     "MCP tool result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if item.Server != "" {
		ev.enrich.extra["mcpServer"] = item.Server
		ev.toolName = item.Server
	}
	if item.Tool != "" {
		ev.enrich.extra["mcpTool"] = item.Tool
		ev.toolName = item.Server + "__" + item.Tool
	}
	if len(item.Arguments) > 0 && !isJSONNull(item.Arguments) {
		ev.input = map[string]any{"arguments": item.Arguments}
	}
	if len(item.Error) > 0 && !isJSONNull(item.Error) {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
		var mcpErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(item.Error, &mcpErr) == nil {
			ev.enrich.output = mcpErr.Message
		}
	} else if len(item.Result) > 0 && !isJSONNull(item.Result) {
		var result struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(item.Result, &result) == nil {
			ev.enrich.output, _ = contentText(result.Content)
		}
	}
	c.applyItemEndEvent(p, ev)
}

// applyDynamicToolCallItem has no legacy rollout counterpart at all —
// `DynamicToolCallRequest`/`Response` are in policy.rs's not-persisted arm, so
// a legacy rollout recorded a dynamic tool call only as its `response_item`
// pair. In a paginated file the item is the record, and it carries the result
// the response item's output line would otherwise supply.
func (c *converter) applyDynamicToolCallItem(p itemCompletedPayload, item turnItem) {
	if strings.EqualFold(strings.TrimSpace(item.Status), "in_progress") {
		return
	}
	name := strings.TrimSpace(item.Tool)
	if ns := strings.TrimSpace(item.Namespace); ns != "" && name != "" {
		name = ns + "__" + name
	}
	if name == "" {
		name = "tool"
	}
	ev := endEvent{
		callID:   item.ID,
		toolName: name,
		itemType: "toolCall",
		what:     "dynamic tool result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if len(item.Arguments) > 0 && !isJSONNull(item.Arguments) {
		ev.input = map[string]any{"arguments": item.Arguments}
	}
	if len(item.ContentItems) > 0 {
		if encoded, err := json.Marshal(item.ContentItems); err == nil {
			ev.enrich.output, _ = contentText(encoded)
		}
	}
	if item.Success != nil && !*item.Success {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
	} else if strings.EqualFold(strings.TrimSpace(item.Status), "failed") {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
	}
	if len(item.Error) > 0 && !isJSONNull(item.Error) {
		var message string
		if json.Unmarshal(item.Error, &message) == nil && message != "" {
			ev.enrich.output = firstNonEmpty(message, ev.enrich.output)
		}
	}
	c.applyItemEndEvent(p, ev)
}

// applyWebSearchItem covers both the hosted `TurnItem::WebSearch` and the
// standalone `web.search` extension item; upstream maps the hosted one onto
// `WebSearchEndEvent`, and the extension one is the same shape.
func (c *converter) applyWebSearchItem(p itemCompletedPayload, item turnItem) {
	ev := endEvent{
		callID:   item.ID,
		toolName: "web_search",
		itemType: "webSearch",
		what:     "web search result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if item.Query != "" {
		ev.enrich.extra["query"] = item.Query
		ev.input = map[string]any{"query": item.Query}
	}
	c.applyItemEndEvent(p, ev)
}

// applyImageViewItem is upstream's `ViewImageToolCall`: the model attached a
// local image to its context. `path` is a PathUri.
func (c *converter) applyImageViewItem(p itemCompletedPayload, item turnItem) {
	path := pathFromURI(item.Path)
	ev := endEvent{
		callID:   item.ID,
		toolName: "view_image",
		itemType: "imageView",
		what:     "image view",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if path != "" {
		ev.input = map[string]any{"file_path": path}
		ev.enrich.output = path
	}
	c.applyItemEndEvent(p, ev)
}

// applyImageGenerationItem is upstream's `ImageGenerationEnd`. `result` is the
// base64 image itself and is deliberately NOT carried into the row: it is
// megabytes of payload the import writer would persist as tool output. The
// saved path is what a reader can act on.
func (c *converter) applyImageGenerationItem(p itemCompletedPayload, item turnItem) {
	saved := firstNonEmpty(item.SavedPath, item.SavedPathCC)
	revised := firstNonEmpty(item.RevisedPrompt, item.RevisedPromptCC)
	ev := endEvent{
		callID:   item.ID,
		toolName: "image_generation",
		itemType: "imageGeneration",
		what:     "image generation result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if revised != "" {
		ev.input = map[string]any{"prompt": revised}
	}
	if saved != "" {
		ev.enrich.output = pathFromURI(saved)
		ev.enrich.extra["files"] = []string{pathFromURI(saved)}
	}
	if status := strings.TrimSpace(item.Status); status != "" {
		switch strings.ToLower(status) {
		case "completed", "succeeded":
			ev.enrich.itemStatus = "completed"
		case "failed", "error":
			ev.enrich.itemStatus = "failed"
			ev.enrich.isError = true
		}
	}
	c.applyItemEndEvent(p, ev)
}

// collabAgentToolNames maps `CollabAgentTool` onto the tool name and prose the
// MultiAgentV1 `collab_*_end` records already use, so a collab call imported
// from a paginated file is the same row shape as one imported from a legacy
// file (collab.go).
var collabAgentToolNames = map[string][2]string{
	"spawn_agent":  {"spawn_agent", "Spawned"},
	"send_input":   {"send_message", "Messaged"},
	"resume_agent": {"resume_agent", "Resumed"},
	"wait":         {"wait", "Waited for"},
	"close_agent":  {"close_agent", "Closed"},
}

func (c *converter) applyCollabAgentToolCallItem(p itemCompletedPayload, item turnItem) {
	if strings.EqualFold(strings.TrimSpace(item.Status), "in_progress") {
		return
	}
	names, ok := collabAgentToolNames[strings.ToLower(strings.TrimSpace(item.Tool))]
	if !ok {
		names = [2]string{"collab_agent", "Agent"}
	}
	ev := endEvent{
		callID:   item.ID,
		toolName: names[0],
		itemType: "collab_agent",
		what:     "collab agent result",
		enrich:   toolEnrichment{extra: map[string]any{}},
	}
	if item.Prompt != "" {
		ev.input = map[string]any{"prompt": item.Prompt}
	}
	byThread := map[string]collabAgentRef{}
	for _, ref := range item.ReceiverAgents {
		byThread[ref.ThreadID] = ref
	}
	if len(item.ReceiverThreadIDs) > 0 {
		child := item.ReceiverThreadIDs[0]
		ev.enrich.extra["agentThreadId"] = child
		if ref, ok := byThread[child]; ok {
			if ref.AgentNickname != "" {
				ev.enrich.extra["agentNickname"] = ref.AgentNickname
			}
			if ref.AgentRole != "" {
				ev.enrich.extra["agentRole"] = ref.AgentRole
			}
		}
	}
	var b strings.Builder
	for _, threadID := range sortedRawKeys(item.AgentsStates) {
		label, text := collabStatusText(item.AgentsStates[threadID])
		nickname := byThread[threadID].AgentNickname
		writeCollabStatus(&b, names[1], nickname, label, text)
		if label == "errored" || label == "failed" {
			ev.enrich.isError = true
			ev.enrich.itemStatus = "failed"
		}
	}
	ev.enrich.output = b.String()
	if strings.EqualFold(strings.TrimSpace(item.Status), "failed") {
		ev.enrich.isError = true
		ev.enrich.itemStatus = "failed"
	}
	c.applyItemEndEvent(p, ev)
}

// applySubAgentActivityItem is the paginated counterpart of
// `sub_agent_activity`. The linkage is the same one collab.go documents: the
// item id IS the spawning call's `call_id`, so the row parents under the
// spawn_agent row and the agent path is remembered for the later delivery.
func (c *converter) applySubAgentActivityItem(item turnItem) {
	agentPath := strings.TrimSpace(item.AgentPath)
	parent := c.toolItemIDs[strings.TrimSpace(item.ID)]
	if parent != "" && agentPath != "" {
		c.agentParents[agentPath] = parent
	}
	meta := map[string]any{
		"kind":         "subagent_activity",
		"activityKind": strings.TrimSpace(item.Kind),
	}
	if agentPath != "" {
		meta["agentPath"] = agentPath
	}
	if id := strings.TrimSpace(item.AgentThreadID); id != "" {
		meta["agentThreadId"] = id
	}
	c.emitNotification(subAgentSummary(agentPath, item.Kind), meta, parent)
}

// applyItemEndEvent routes an item-derived end record through the SAME
// three-way resolution every legacy end record uses (enrich a known call /
// stand alone / mark unavailable), with the item's own clock attached.
//
// It also records the call id in `itemRows` when the record stood alone, so
// the `response_item` call for the same id — which arrives AFTER the item in
// a paginated file — does not open a second row for a tool the item already
// reported in full. See tools.go.
func (c *converter) applyItemEndEvent(p itemCompletedPayload, ev endEvent) {
	ev.startedAt = millisTime(p.StartedAtMS)
	ev.completedAt = millisTime(p.CompletedAtMS)
	if _, known := c.tools[strings.TrimSpace(ev.callID)]; !known && ev.selfContained() {
		if id := strings.TrimSpace(ev.callID); id != "" {
			c.itemRows[id] = struct{}{}
		}
	}
	c.applyEndEvent(ev)
}

// itemCompletedAt is the item's own completion clock, falling back to the
// line's timestamp. Migrated rollouts write `started_at_ms: null`, so a
// missing value is normal rather than corrupt.
func (c *converter) itemCompletedAt(p itemCompletedPayload) time.Time {
	if at := millisTime(p.CompletedAtMS); !at.IsZero() {
		return at
	}
	return c.lastTimestamp
}

func millisTime(ms *int64) time.Time {
	if ms == nil || *ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(*ms).UTC()
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// pathFromURI turns a `PathUri` (`file:///home/x`) back into a plain path.
// Codex moved several path fields to PathUri in 0.147; older rollouts carry
// the bare path, and both must render as a path.
func pathFromURI(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "file://") {
		return value
	}
	rest := strings.TrimPrefix(value, "file://")
	// file://<host>/path is not something Codex writes; anything after the
	// scheme up to the first '/' is dropped only when it is empty.
	if decoded, err := decodePercent(rest); err == nil {
		rest = decoded
	}
	return rest
}

// decodePercent undoes percent-encoding without pulling net/url in for what
// is a two-character escape on a path.
func decodePercent(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+2 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), nil
}

// sleepDurationMS reads whichever of the two spellings the file carries.
// See turnItem.DurationMS for why both are accepted.
func sleepDurationMS(item turnItem) int64 {
	if item.DurationMS != 0 {
		return item.DurationMS
	}
	return item.DurationMSSnake
}

func sleepSummary(durationMS int64) string {
	if durationMS <= 0 {
		return "Agent paused"
	}
	return "Agent paused for " + (time.Duration(durationMS) * time.Millisecond).String()
}

func exitedReviewSummary(item turnItem) string {
	if item.ReviewOutput == nil {
		return "Code review finished"
	}
	return reviewSummary("Code review finished", firstNonEmpty(
		item.ReviewOutput.OverallExplanation,
		item.ReviewOutput.OverallCorrectness,
	))
}
