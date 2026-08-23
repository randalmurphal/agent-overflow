// Package claude — parser for `user`-type NDJSON lines. The top-level
// parseUser dispatches each tool_result content block to either the
// task-output helper (appendTaskOutputCompletion) or the standard tool
// completion helper (appendToolResultCompletion). Keeping each block's
// logic in its own helper isolates the task-correlation rules from the
// routine completion path.

package claude

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// parseUser picks up the Claude `user` echo of a prior assistant tool_use.
// The content is a list of `tool_result` blocks; each becomes one
// EventToolComplete keyed by the original tool_use_id.
//
// A user envelope with no tool_result in it is prose, and the only prose
// this layer claims is SCOPED prose — a `parent_tool_use_id` envelope,
// which is the subagent's own conversation rather than the user's. That
// goes to subagentPromptEvents. Unparented prose belongs to the replay
// echo (parse_user_replay.go) and is dropped here.
//
// The `tool_use_result` sibling carries richer structured metadata for some
// tools (e.g. Bash's `exit_code`, stdout/stderr). We surface `exit_code`
// when present so downstream UI can flag command failures without re-parsing
// the text body. When the block content is a list of content blocks (e.g.
// [{"type":"text","text":"..."}]) we stringify the text. JSON marshal
// failures fall through to an empty body — this layer never errors on
// malformed input since the read loop cannot recover a broken line.
func (p *Parser) parseUser(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	msgRaw, ok := raw["message"]
	if !ok {
		return nil, nil
	}

	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return nil, nil
	}

	// user.message.content can be either a plain string or an array of
	// blocks (tool_result, text, image).
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// String content. Nothing at this layer wants a top-level one (the
		// user's own text arrives through the `isReplay` echo), but a
		// SCOPED one is the subagent's opening prompt.
		return subagentPromptEvents(threadID, raw, msg.Content, nil, now), nil
	}
	if !hasBlockOfType(blocks, "tool_result") {
		return subagentPromptEvents(threadID, raw, nil, blocks, now), nil
	}

	// Optional sibling with richer per-tool result metadata. Not all tools
	// populate it and some versions of the CLI omit it entirely.
	toolUseResults := indexToolUseResults(raw["tool_use_result"])

	events := make([]provider.ProviderEvent, 0, len(blocks))
	for _, block := range blocks {
		events = p.appendToolResultBlock(events, threadID, now, line, raw, block, toolUseResults)
	}
	return events, nil
}

// hasBlockOfType reports whether a content array carries a block of the
// given type.
//
// The prompt branch keys on the PRESENCE of a tool_result, not on
// whether the completion path produced anything: a malformed tool_result
// (no `tool_use_id`) emits no event, and re-reading that envelope as
// prose would put the tool's output in a user bubble. Same rule the
// importer's userPromptText applies to the same rows.
func hasBlockOfType(blocks []map[string]json.RawMessage, blockType string) bool {
	for _, block := range blocks {
		if readRawString(block["type"]) == blockType {
			return true
		}
	}
	return false
}

// subagentPromptEvents promotes a SCOPED user envelope carrying prose —
// no tool_result block anywhere in it — to an EventUserText under the
// launching tool_use.
//
// This is the subagent's own conversation: the task prompt the CLI hands
// the agent (`parent_tool_use_id` set, no `isReplay`), and any later
// user-role text delivered into that agent. Triage's parented wire-only
// branch persists it as the nested `user:wire:<uuid>` row, which is where
// the agent pane's opening instructions row and the card's initial-prompt
// line come from. The same rows reach a BACKGROUNDED agent through the
// transcript backfill instead, keyed on the same uuid, so the two paths
// converge on one row.
//
// Top-level envelopes are left alone: an unparented string-content user
// row is the pending-send echo's business (parse_user_replay.go), and
// promoting it here would mint a second, unmatched bubble.
//
// The exclusions mirror the importer's `userPromptText` exactly, because
// the same rows arrive through both doors: `isMeta` bookkeeping, the
// compaction summary, and `isVisibleInTranscriptOnly` context injections
// are CLI machinery, not something the agent was told.
func subagentPromptEvents(
	threadID string,
	raw map[string]json.RawMessage,
	stringContent json.RawMessage,
	blocks []map[string]json.RawMessage,
	now time.Time,
) []provider.ProviderEvent {
	parentToolUseID := readRawString(raw["parent_tool_use_id"])
	if parentToolUseID == "" {
		return nil
	}
	if readBoolValue(raw, "isMeta") || readBoolValue(raw, "isCompactSummary") ||
		readBoolValue(raw, "isVisibleInTranscriptOnly") {
		return nil
	}
	// An envelope with no stable uuid cannot be deduped against a replay
	// or against the backfill's copy of the same row, and triage refuses
	// to mint an id for one. Dropping it is the honest outcome.
	uuid := readRawString(raw["uuid"])
	if uuid == "" {
		return nil
	}

	var text string
	if len(stringContent) > 0 {
		text = readRawString(stringContent)
	} else {
		text = blockTextContent(blocks)
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	meta, err := json.Marshal(map[string]any{"provider_item_id": uuid})
	if err != nil {
		return nil
	}
	return []provider.ProviderEvent{{
		Kind:            provider.EventUserText,
		ThreadID:        threadID,
		Role:            "user",
		Content:         text,
		ContentPresent:  true,
		ParentToolUseID: parentToolUseID,
		Meta:            meta,
		Timestamp:       now,
	}}
}

// blockTextContent concatenates the `text` blocks of a user content
// array. Images and other block kinds carry nothing a row can render, so
// they contribute nothing; a block list with no text at all yields "" and
// the caller drops the envelope.
func blockTextContent(blocks []map[string]json.RawMessage) string {
	var parts []string
	for _, block := range blocks {
		if readRawString(block["type"]) != "text" {
			continue
		}
		if text := readRawString(block["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// appendToolResultBlock emits the tool-lifecycle completion for one
// `tool_result` block and — when the block carries a TaskOutput
// enrichment — an additive `EventBackgroundTaskTerminal` for the
// underlying backgrounded task.
//
// The universal tool-lifecycle invariant (see
// docs/architecture/invariants.md invariant 20) says every `tool_use_id`
// on a `tool_result` emits exactly one `EventToolComplete` for its own
// id. The one carve-out is `TodoWrite`: the matching tool_use never
// produced an `EventToolStart` (it was rerouted to `EventTodoUpdate`),
// so emitting a completion here would be an orphan against a
// non-existent timeline row. Backgrounded placeholders, TaskOutput
// echoes, and every other standard inline result still emit the
// completion. The background placeholder's completion just carries
// `is_background:true` in meta so triage keeps the launch row at
// `status=running` per spec (see
// docs/architecture/turn-lifecycle.md §Tool lifecycle).
//
// TaskOutput (E3 in claude-wire.md) is additive: after the own-id
// completion fires, if `tool_use_result.task.status` is terminal we
// also emit `EventBackgroundTaskTerminal` keyed to the underlying
// backgrounded task's original `tool_use_id`. Triage's
// AppendCompletionItem upsert is idempotent, so any later
// `task_updated` for the same task_id folds into the same sibling row.
func (p *Parser) appendToolResultBlock(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	line []byte,
	raw, block map[string]json.RawMessage,
	toolUseResults map[string]json.RawMessage,
) []provider.ProviderEvent {
	var blockType string
	if err := json.Unmarshal(block["type"], &blockType); err != nil || blockType != "tool_result" {
		return events
	}

	var toolUseID string
	if err := json.Unmarshal(block["tool_use_id"], &toolUseID); err != nil || toolUseID == "" {
		// A tool_result without an ID can't be correlated back to a
		// tool_use, so drop it rather than emit an orphan completion.
		return events
	}

	// TodoWrite carve-out: the tool_use was rerouted to EventTodoUpdate
	// in parse_assistant.go and never produced a tool-call row. Drop the
	// matching tool_result so we don't emit an orphan completion against
	// a non-existent item. Releasing the flag keeps the correlation map
	// bounded across long sessions.
	if p.isTodoWrite(toolUseID) {
		p.clearTodoWrite(toolUseID)
		return events
	}

	// TaskCreate / TaskUpdate carve-out (Claude Code 2.1.150+): the
	// tool_use staged a pending mutation in parse_assistant.go without
	// emitting a row. Apply the mutation against the parser's local
	// task mirror now that the authoritative result has arrived, emit
	// the snapshot through the existing EventTodoUpdate channel so the
	// activity rail widget gets the same shape it consumed under
	// TodoWrite, and drop the matching completion so we don't emit an
	// orphan row.
	// Resolve the structured `tool_use_result` sibling for this
	// block. `indexToolUseResults` succeeds when the wire shape
	// is an array or a map keyed by tool_use_id, but Claude's
	// most common shape — a bare object like
	// `{filePath, structuredPatch, ...}` for Edit/Write/MultiEdit
	// or `{stdout, exit_code, ...}` for Bash — carries no
	// `tool_use_id` field and falls through the indexer with an
	// empty result. The fallback to `raw["tool_use_result"]` covers
	// that case: a single tool_use_result paired with a single
	// tool_result block IS for that block. Used for the standard
	// EventToolComplete meta, the TaskOutput enrichment path below,
	// and the TaskCreate/TaskUpdate carve-out.
	toolUseResultRaw := toolUseResults[toolUseID]
	if len(toolUseResultRaw) == 0 {
		toolUseResultRaw = raw["tool_use_result"]
	}

	if mutation, ok := p.takePendingTaskMutation(toolUseID); ok {
		return p.applyPendingTaskMutation(events, threadID, now, mutation, toolUseID, toolUseResultRaw)
	}

	content := extractToolResultText(block["content"])

	// Always emit `EventToolComplete` for the tool's own id — no
	// short-circuit for TaskOutput or backgrounded placeholders. The
	// per-spec "keep status=running for background" rule is a triage
	// decision keyed off `is_background` in meta, not a parser-level
	// drop of the event.
	//
	// Five INDEPENDENT signals mark a tool_result as a backgrounded
	// placeholder (the real terminal arrives later via the task
	// lifecycle), and any one is sufficient:
	//
	//   1. flaggedAtLaunch — the tool_use carried `run_in_background:true`,
	//      recorded at assistant-parse time in `backgroundToolUses`.
	//   2. markedOnWire — `tool_use_result.backgroundTaskId` is present.
	//      This is Claude's authoritative wire marker and the ONLY signal
	//      when the CLI auto-backgrounds a foreground command that exceeds
	//      its Bash timeout: that input has no `run_in_background` flag, so
	//      (1) is empty. See claude-wire.md §E2.
	//   3. asyncLaunched — the `local_agent` (Task/Agent) launch got the
	//      bare "Async agent launched successfully." ack
	//      (`isAsync:true` / `status:"async_launched"`, no
	//      `run_in_background` in the input and no `backgroundTaskId` on
	//      the wire). Distinct from (2) because it carries no
	//      `backgroundTaskId` at all — its own `agentId` IS the task_id
	//      the later `task_updated`/`task_notification` pair uses. See
	//      claude-wire.md §E5 "Async local_agent launch (bare ack)".
	//      On a SIDECHAIN line (a subagent launching its own async
	//      agent, depth 2) the CLI omits `tool_use_result` entirely, so
	//      this signal has no structured half at all and the ack's own
	//      TEXT is the only evidence — `asyncLaunchAckAgentID` below,
	//      claude-wire.md §E5b.
	//   4. monitorLaunched — the Monitor watch-task launch ack
	//      (`{taskId, timeoutMs, persistent}`, §E7). Like (3) it carries
	//      no `run_in_background` and no `backgroundTaskId`; its `taskId`
	//      is the `local_bash` task the later `task_updated` terminal
	//      routes by. Missing this signal is how a live Monitor-watched
	//      session read as reap-idle (2026-07-28, thread b44a738d).
	//   5. liveAgentTask — this tool_use's local_agent task is still
	//      LIVE (`system/task_started` seen, no terminal `task_updated`
	//      yet). The wire-TYPED twin of (3)'s §E5b text fallback, and
	//      deliberately redundant with it: an awaited agent's real
	//      result always arrives AFTER its terminal task_updated (which
	//      clears the flag) — 0–45ms after, across all 34 awaited
	//      completions in three weeks of wire logs (2026-08-21) — so a
	//      local_agent tool_result landing while its task is live is
	//      definitionally an ack, whatever its text says. This catches
	//      what §E5b's gate refuses (an ack with no extractable
	//      `agentId:` line — safe to promote here precisely because
	//      task_started already recorded the task_id ↔ tool_use_id
	//      route for the terminal) and survives a CLI rewording of the
	//      ack sentinel. Like §E5b it fires only when NO
	//      `tool_use_result` is present — a present structured result
	//      is the sole authority (see the inline-completion bound
	//      below). What it does NOT cover is replay/JSONL parsing
	//      (session files carry no system envelopes), where §E5b's text
	//      match is the only signal — which is why both exist.
	//      local_bash tasks never mark the flag: a foreground Bash
	//      result's ordering against its terminal is not part of the
	//      verified contract (parse_system.go, task_started).
	//
	// ⚠ An INLINE (awaited) agent's real completion also carries
	// `agentId` + `status:"completed"` in its `tool_use_result`, so
	// `toolResultAsyncLaunch` keys ONLY on `isAsync`/`status:"async_launched"`
	// — never on mere `agentId` presence — or every inline agent
	// completion would misclassify as backgrounded.
	//
	// Signals (2)–(4) are decoded from `tool_use_result` in a
	// single pass — the sibling can be megabytes of Bash stdout, so
	// per-signal re-decodes are not acceptable on this path.
	flaggedAtLaunch := p.isBackground(toolUseID)
	backgroundSignals := readToolResultBackgroundSignals(toolUseResultRaw)
	markedOnWire := toolResultBackgrounded(backgroundSignals)
	asyncAgentID, asyncLaunched := toolResultAsyncLaunch(backgroundSignals)
	// §E5b — the sidechain async-launch ack. A subagent launching its
	// OWN async agent (depth 2) gets the identical ack body, but the
	// envelope carries NO `tool_use_result` at all, so every structured
	// signal above misses and the launch would settle in place with the
	// internal ack text as its body. Three conjunctive conditions gate
	// the text fallback, and all three must hold:
	//
	//   a. NO `tool_use_result` on this block. When the structured
	//      sibling exists it is the sole authority — a present-but-not-
	//      async result (an inline agent's real completion, a string
	//      InputValidationError ack) must never be re-read as text.
	//   b. this tool_use was observed as Claude's agent-launch tool
	//      (`Agent`/`Task`, marked in parse_assistant.go). Nothing else
	//      can produce an async-launch ack, and the marker is always in
	//      place: the assistant envelope carrying the launch precedes
	//      its ack on the same sequentially-parsed stream, and async
	//      agents die with their CLI process, so no pre-restart agent
	//      can ack onto a fresh parser.
	//   c. the ack text passes `asyncLaunchAckAgentID` (exact sentinel
	//      prefix + an extractable `agentId:` line). `content` is the
	//      already-flattened body, which concatenates the block array's
	//      TEXT blocks in wire order and drops images — so a prefix
	//      match on it is a prefix match on the first text-bearing
	//      block, without a second decode of a payload that can be
	//      megabytes elsewhere on this path.
	//
	// Failing any of them leaves today's behaviour (an instantly-done
	// card) rather than promoting: an ack whose agent id we cannot
	// recover has nothing to correlate its terminal against, and a card
	// stuck at "running" forever is the worse outcome.
	if !asyncLaunched && len(toolUseResultRaw) == 0 && p.isAgentLaunchTool(toolUseID) {
		asyncAgentID, asyncLaunched = asyncLaunchAckAgentID(content)
	}
	monitorTaskID, monitorLaunched := toolResultMonitorLaunch(backgroundSignals)
	liveAgentTask := len(toolUseResultRaw) == 0 && p.hasLiveAgentTask(toolUseID)
	isBackground := flaggedAtLaunch || markedOnWire || asyncLaunched || monitorLaunched || liveAgentTask
	// §E9 — a FORKED skill's completion. A skill whose frontmatter forks
	// runs as a subagent whose rows land on the parent stream with
	// `parent_tool_use_id` = this Skill tool_use, but it gets no
	// `system/task_started`, no task_id, no `task_progress` and no entry
	// in `background_tasks_changed`. Its ONLY identity statement is this
	// completion: `{success, commandName, status:"forked", agentId}`.
	// Stamping it marks the launch row as an agent node the card and pane
	// can render; an INLINE skill answers `{success, commandName}` with no
	// status and is deliberately left unstamped.
	forkedAgentID, forkedCommandName, skillForked := toolResultSkillFork(backgroundSignals)
	events = appendToolResultCompletion(
		events, threadID, toolUseID, now, line,
		isBackground, monitorLaunched, skillForked, forkedAgentID, forkedCommandName,
		block, content, toolUseResultRaw,
	)
	if skillForked {
		// The fork's agentId is the id a later `can_use_tool.agent_id`
		// or `task_progress` addresses it by (it is the sidechain id in
		// `subagents/agent-<id>.jsonl`), so bind it to the Skill
		// tool_use exactly as an async launch binds its task_id. Without
		// this a fork's approvals would resolve to no scope at all.
		p.rememberTaskToolUse(forkedAgentID, toolUseID)
	}
	// The async / Monitor ack IS the task lifecycle's task_id ↔
	// tool_use_id correlation (normally learned ~4ms earlier from
	// `system/task_started`). Recording it here too means a parser that
	// reconnected and missed task_started can still resolve the later
	// `task_updated`/`task_notification` terminal back to this launch.
	// rememberTaskToolUse is idempotent, so re-deriving an already-known
	// mapping from task_started is a harmless no-op.
	if asyncLaunched && asyncAgentID != "" {
		p.rememberTaskToolUse(asyncAgentID, toolUseID)
	}
	if monitorLaunched {
		p.rememberTaskToolUse(monitorTaskID, toolUseID)
	}
	// ScheduleWakeup ack: no task lifecycle exists behind the pending
	// wakeup timer, so surface it as its own session-level event —
	// triage records the fire time and the idle reaper keeps the
	// session (and with it the in-process timer) alive until it fires.
	if scheduledForUnixMs, ok := toolResultScheduledWakeup(backgroundSignals); ok {
		meta, err := json.Marshal(provider.SessionWakeupMeta{ScheduledForUnixMs: scheduledForUnixMs})
		if err != nil {
			log.Printf("claude: marshal session wakeup meta for %s: %v", toolUseID, err)
		} else {
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventSessionWakeup,
				ThreadID:  threadID,
				ItemID:    toolUseID,
				Meta:      meta,
				Timestamp: now,
			})
		}
	}
	// The tool_use_id → is_background correlation is a one-shot: once the
	// placeholder tool_result echoes, the launch-time flag has served its
	// purpose. Release it so backgroundToolUses doesn't leak across a long
	// session. Keyed on flaggedAtLaunch, not isBackground: the wire-marker
	// path (timeout auto-background) never populated the map, so there is
	// nothing to release for it. The triage row is already tagged
	// `is_background=true`; any later task_updated / TaskOutput writes a
	// sibling via EventBackgroundTaskTerminal and does not re-read this map.
	if flaggedAtLaunch {
		p.clearBackground(toolUseID)
	}

	// Additive TaskOutput enrichment: this lets a later
	// `task_updated` or another TaskOutput poll upsert the same
	// sibling row idempotently in triage.
	events = p.appendTaskOutputCompletion(
		events, threadID, now, line,
		block, content, toolUseResultRaw,
	)

	return events
}

// appendTaskOutputCompletion emits the additive
// `EventBackgroundTaskTerminal` enrichment when the TaskOutput
// `tool_use_result.task` carries a terminal status.
//
// This path is NOT consuming — the caller has already emitted the
// own-id `EventToolComplete` for the TaskOutput tool itself. The
// terminal event carries the backgrounded task's original
// `tool_use_id` (looked up via `taskToolUses[task_id]`) plus the
// richer task payload (exit_code, output_file, summary text). When
// the `tool_use_id` cannot be resolved — e.g. fresh adapter session
// after reconnect with an empty correlation map — the event still
// emits with an empty `ItemID`; triage falls back to
// `items.meta.task_id` lookup.
func (p *Parser) appendTaskOutputCompletion(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	line []byte,
	block map[string]json.RawMessage,
	content string,
	taskOutputMeta json.RawMessage,
) []provider.ProviderEvent {
	taskOutput, ok := extractTaskOutputCompletion(block["content"], taskOutputMeta)
	if !ok {
		return events
	}

	taskRef := p.taskToolUseRef(taskOutput.TaskID)
	originalToolUseID := taskRef.ToolUseID

	status := "completed"
	if taskOutput.IsError {
		status = "failed"
	}
	metaFields := map[string]any{
		"task_id": taskOutput.TaskID,
		"status":  status,
		"source":  "task_output",
	}
	if originalToolUseID != "" {
		metaFields["tool_use_id"] = originalToolUseID
	}
	if taskRef.ParentToolUseID != "" {
		metaFields["parent_tool_use_id"] = taskRef.ParentToolUseID
	}
	if taskOutput.IsError {
		metaFields["is_error"] = true
	}
	if taskOutput.ExitCode != nil {
		metaFields["exit_code"] = *taskOutput.ExitCode
	}
	if taskOutput.OutputFile != "" {
		metaFields["output_file"] = taskOutput.OutputFile
	}
	meta, _ := json.Marshal(metaFields)

	events = append(events, provider.ProviderEvent{
		Kind:            provider.EventBackgroundTaskTerminal,
		ThreadID:        threadID,
		ItemID:          originalToolUseID,
		Content:         firstNonEmpty(taskOutput.Output, taskOutput.Summary, content),
		Meta:            meta,
		ParentToolUseID: taskRef.ParentToolUseID,
		Timestamp:       now,
		Raw:             line,
	})
	// TaskOutput enrichment does not clear backgroundToolUses — the
	// original backgrounded tool_use's placeholder tool_result is what
	// releases that flag (see appendToolResultBlock). This helper runs
	// ON a TaskOutput tool_result whose own tool_use_id is NOT the
	// backgrounded one, so the map entry for `originalToolUseID` is
	// orthogonal to the caller's clear path.
	//
	// Liveness is different: this IS terminal evidence for the
	// underlying task, and signal (5)'s contract is "any terminal
	// disarms". A dropped task_updated (reconnect gap) whose completion
	// surfaces only through TaskOutput polling must still release the
	// flag, or a later no-`tool_use_result` result for the original
	// tool_use would misclassify as a background ack.
	p.clearLiveAgentTask(originalToolUseID)
	return events
}

// appendToolResultCompletion emits the tool-lifecycle completion
// (`EventToolComplete` keyed by the tool_use_id) for every `tool_result`
// block — standard inline, backgrounded placeholder, or TaskOutput. The
// `isBackground` flag is surfaced in meta so triage knows to keep the
// launch row at `status=running` per the background-tool spec. Any
// richer sibling event (e.g. TaskOutput's terminal) rides on top via
// `EventBackgroundTaskTerminal`.
func appendToolResultCompletion(
	events []provider.ProviderEvent,
	threadID, toolUseID string,
	now time.Time,
	line []byte,
	isBackground bool,
	watchTask bool,
	skillForked bool,
	skillForkAgentID string,
	skillForkCommandName string,
	block map[string]json.RawMessage,
	content string,
	toolUseResult json.RawMessage,
) []provider.ProviderEvent {
	var isError bool
	if v, ok := block["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}

	toolResultJSON, marshalErr := json.Marshal(block)
	metaFields := map[string]any{
		"is_error": isError,
	}
	if marshalErr != nil {
		metaFields["tool_result"] = json.RawMessage(`{}`)
		metaFields["tool_result_marshal_error"] = marshalErr.Error()
	} else {
		metaFields["tool_result"] = json.RawMessage(toolResultJSON)
	}
	if len(toolUseResult) > 0 {
		metaFields["tool_use_result"] = json.RawMessage(toolUseResult)
	}
	if isBackground {
		metaFields["is_background"] = true
	}
	if watchTask {
		// A Monitor watch observes; it never produces the result a queued
		// user send might be waiting on. Triage copies this marker onto
		// the launch row so the flush-queue drain ignores watch tasks
		// while every other background consumer (reaper, revert, context
		// repair) still counts them as live work.
		metaFields["watch_task"] = true
	}
	if skillForked {
		// The structural fork marker (claude-wire.md §E9). Triage reads
		// it to treat this Skill row as an agent launch: `agentId` is
		// the fork's own sidechain id and `commandName` the skill name
		// the card labels itself with.
		metaFields["skillFork"] = map[string]string{
			"agentId":     skillForkAgentID,
			"commandName": skillForkCommandName,
		}
	}
	if code, ok := extractExitCode(block["content"], toolUseResult); ok {
		metaFields["exit_code"] = code
	}

	meta, _ := json.Marshal(metaFields)
	return append(events, provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   content,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	})
}

// applyPendingTaskMutation completes a TaskCreate / TaskUpdate
// roundtrip: decode the staged input, decode the result echo,
// mutate the parser's local task mirror, and emit a fresh
// EventTodoUpdate snapshot. A failed mutation (TaskCreate with no
// id in the result, TaskUpdate with success=false, decode errors)
// drops state changes silently and emits no event — the original
// timeline already has no row to clean up because the assistant
// path suppressed the EventToolStart.
//
// The carve-out is symmetric with TodoWrite: no EventToolComplete
// is appended either way, so the tool lifecycle invariant ("every
// EventToolStart pairs with one EventToolComplete") remains intact —
// neither side ever fired.
func (p *Parser) applyPendingTaskMutation(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	mutation pendingTaskMutation,
	toolUseID string,
	toolUseResult json.RawMessage,
) []provider.ProviderEvent {
	switch mutation.op {
	case "create":
		return p.emitTaskCreate(events, threadID, now, mutation, toolUseID, toolUseResult)
	case "update":
		return p.emitTaskUpdate(events, threadID, now, mutation, toolUseID, toolUseResult)
	}
	return events
}

func (p *Parser) emitTaskCreate(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	mutation pendingTaskMutation,
	toolUseID string,
	toolUseResult json.RawMessage,
) []provider.ProviderEvent {
	decoded, ok := decodeTaskCreateInput(mutation.input)
	if !ok {
		return events
	}
	id := taskCreateResultID(toolUseResult)
	if id == "" {
		return events
	}
	meta, err := json.Marshal(provider.TaskCreateMeta{
		TaskID:  id,
		Subject: decoded.Subject,
	})
	if err != nil {
		return events
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTaskCreate,
		ThreadID:        threadID,
		ItemID:          toolUseID,
		ParentToolUseID: mutation.parentToolUseID,
		Meta:            meta,
		Timestamp:       now,
	})
}

func (p *Parser) emitTaskUpdate(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	mutation pendingTaskMutation,
	toolUseID string,
	toolUseResult json.RawMessage,
) []provider.ProviderEvent {
	decoded, ok := decodeTaskUpdateInput(mutation.input)
	if !ok {
		return events
	}
	result, resultOK := decodeTaskUpdateResult(toolUseResult)
	if resultOK && !result.Success {
		return events
	}
	status, deleted := normalizeTaskStatus(decoded.Status)
	meta, err := json.Marshal(provider.TaskUpdateMeta{
		TaskID:  decoded.TaskID,
		Status:  status,
		Subject: decoded.Subject,
		Owner:   decoded.Owner,
		Deleted: deleted,
	})
	if err != nil {
		return events
	}
	return append(events, provider.ProviderEvent{
		Kind:            provider.EventTaskUpdate,
		ThreadID:        threadID,
		ItemID:          toolUseID,
		ParentToolUseID: mutation.parentToolUseID,
		Meta:            meta,
		Timestamp:       now,
	})
}

// extractToolResultText flattens the content of a tool_result block into a
// plain string. The SDK uses either a top-level string or a list of content
// blocks with `{type, text}` shapes; we handle both and concatenate text
// bodies.
func extractToolResultText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return asString
	}

	var asBlocks []map[string]json.RawMessage
	if json.Unmarshal(content, &asBlocks) != nil {
		// Fall back to the raw JSON so we don't silently drop structured
		// content shapes we haven't taught the parser about.
		return string(content)
	}

	var builder []byte
	for _, block := range asBlocks {
		var t string
		if err := json.Unmarshal(block["type"], &t); err != nil {
			continue
		}
		switch t {
		case "text":
			var text string
			if json.Unmarshal(block["text"], &text) == nil {
				builder = append(builder, text...)
			}
		case "image":
			// Images inside tool_result blocks are rare (Read tool for
			// binary files). Skip the payload; downstream consumers that
			// care will read the raw line.
		}
	}
	return string(builder)
}

// toolResultBackgroundSignals is the single-pass decode of the
// `tool_use_result` fields that classify a tool_result as a backgrounded
// placeholder (claude-wire.md §E2), an async local_agent launch ack
// (§E5), a Monitor watch-task launch ack (§E7), or a ScheduleWakeup ack
// (§E8). It exists so appendToolResultBlock decodes the raw sibling
// exactly ONCE per tool_result: `tool_use_result` can be megabytes for
// Bash stdout / Read payloads, and each `map[string]json.RawMessage`
// decode copies value bytes per key — the per-signal readXAtAnyKey
// calls this replaces cost three full decodes on every tool result.
//
// The *Set fields track presence so the defensive array-of-objects
// fallback keeps the readXAtAnyKey family's first-successful-hit
// semantics: a later entry must not overwrite a value an earlier entry
// already decoded. For `persistent` and `timeoutMs` presence IS the
// signal (the Monitor ack carries them for both persistent:true and
// persistent:false launches), so only the Set flag matters.
type toolResultBackgroundSignals struct {
	backgroundTaskID string
	isAsync          bool
	status           string
	agentID          string
	commandName      string
	taskID           string
	scheduledForMs   int64
	stopped          bool

	backgroundTaskIDSet bool
	isAsyncSet          bool
	statusSet           bool
	agentIDSet          bool
	commandNameSet      bool
	taskIDSet           bool
	persistentSet       bool
	timeoutMsSet        bool
	scheduledForSet     bool
	clampedDelaySet     bool
	stoppedSet          bool
}

// readToolResultBackgroundSignals decodes the background-classification
// signals out of a raw `tool_use_result`. Mirrors the readXAtAnyKey
// helpers' shape handling (json_helpers.go): a plain object, or —
// defensively — an array of objects where the first successfully-decoded
// hit per field wins. Malformed input yields the zero value, which
// classifies as "not backgrounded, not async".
func readToolResultBackgroundSignals(toolUseResult json.RawMessage) toolResultBackgroundSignals {
	var signals toolResultBackgroundSignals
	if len(toolUseResult) == 0 {
		return signals
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(toolUseResult, &obj) == nil {
		signals.fill(obj)
		return signals
	}
	var arr []map[string]json.RawMessage
	if json.Unmarshal(toolUseResult, &arr) == nil {
		for _, entry := range arr {
			signals.fill(entry)
		}
	}
	return signals
}

func (s *toolResultBackgroundSignals) fill(obj map[string]json.RawMessage) {
	fillSignalString(obj, "backgroundTaskId", &s.backgroundTaskID, &s.backgroundTaskIDSet)
	fillSignalString(obj, "status", &s.status, &s.statusSet)
	fillSignalString(obj, "agentId", &s.agentID, &s.agentIDSet)
	fillSignalString(obj, "commandName", &s.commandName, &s.commandNameSet)
	fillSignalString(obj, "taskId", &s.taskID, &s.taskIDSet)
	fillSignalBool(obj, "isAsync", &s.isAsync, &s.isAsyncSet)
	fillSignalBool(obj, "stopped", &s.stopped, &s.stoppedSet)
	fillSignalInt64(obj, "scheduledFor", &s.scheduledForMs, &s.scheduledForSet)
	fillSignalKeyPresence(obj, "persistent", &s.persistentSet)
	fillSignalKeyPresence(obj, "timeoutMs", &s.timeoutMsSet)
	fillSignalKeyPresence(obj, "clampedDelaySeconds", &s.clampedDelaySet)
}

// fillSignalString copies obj[key] into dst on the first successful
// string decode, leaving already-set fields alone (first-hit-wins across
// repeated fill calls for the array-of-objects fallback).
func fillSignalString(obj map[string]json.RawMessage, key string, dst *string, set *bool) {
	if *set {
		return
	}
	raw, ok := obj[key]
	if !ok {
		return
	}
	var v string
	if json.Unmarshal(raw, &v) == nil {
		*dst = v
		*set = true
	}
}

// fillSignalBool is fillSignalString for bool-valued signals.
func fillSignalBool(obj map[string]json.RawMessage, key string, dst *bool, set *bool) {
	if *set {
		return
	}
	raw, ok := obj[key]
	if !ok {
		return
	}
	var v bool
	if json.Unmarshal(raw, &v) == nil {
		*dst = v
		*set = true
	}
}

// fillSignalInt64 is fillSignalString for integer-valued signals. JSON
// numbers decode through float64-compatible syntax, so it accepts any
// numeric token and truncates toward zero.
func fillSignalInt64(obj map[string]json.RawMessage, key string, dst *int64, set *bool) {
	if *set {
		return
	}
	raw, ok := obj[key]
	if !ok {
		return
	}
	var v float64
	if json.Unmarshal(raw, &v) == nil {
		*dst = int64(v)
		*set = true
	}
}

// fillSignalKeyPresence records that obj carries key at all — for
// signals where presence itself is the discriminator and the value
// doesn't matter (Monitor's `persistent` is meaningful whether true or
// false).
func fillSignalKeyPresence(obj map[string]json.RawMessage, key string, set *bool) {
	if *set {
		return
	}
	if _, ok := obj[key]; ok {
		*set = true
	}
}

// toolResultBackgrounded reports whether the decoded `tool_use_result`
// signals carry a non-empty `backgroundTaskId` — Claude's authoritative
// wire marker that the command is now running in the background. It is
// set for EVERY backgrounding trigger:
//
//   - `input.run_in_background: true` (model/user asked for it),
//   - assistant-initiated mid-run backgrounding
//     (`tool_use_result.assistantAutoBackgrounded: true`),
//   - the CLI auto-backgrounding a foreground command that exceeds its Bash
//     timeout — the input carries NO `run_in_background` flag, so the
//     launch-time `backgroundToolUses` hint is absent and this marker is the
//     only signal.
//
// The id equals the `task_id` carried by the `system/task_started` +
// `system/task_updated` lifecycle, so the later terminal still writes the
// sibling completion row unchanged. See claude-wire.md §E2.
func toolResultBackgrounded(signals toolResultBackgroundSignals) bool {
	return strings.TrimSpace(signals.backgroundTaskID) != ""
}

// toolResultAsyncLaunch reports whether the decoded `tool_use_result`
// signals are the async-agent-launch ack — the bare "Async agent launched
// successfully." acknowledgment Claude returns IMMEDIATELY for a
// `local_agent` (Task/Agent) launch whose input carries no
// `run_in_background` flag. The real terminal arrives later via
// `system/task_updated` + `system/task_notification`, correlated by
// `agentId` == the task lifecycle's `task_id`. See claude-wire.md §E5
// "Async local_agent launch (bare ack)" and the
// local_agent_async_launch.ndjson fixture.
//
// ⚠ Discriminator subtlety: the shape is
// `{isAsync:true, status:"async_launched", agentId, ...}` and carries NO
// `backgroundTaskId`. An INLINE (awaited) agent's real completion
// `tool_use_result` ALSO carries `agentId` and `status:"completed"` —
// so `agentId`/`status` presence alone is NOT a valid discriminator.
// Only `isAsync:true` or `status == "async_launched"` mark the async
// path; either one observed on the wire today is sufficient, and we
// accept either alone so a future CLI that drops one of the two
// redundant markers still classifies correctly. An ABSENT `isAsync` is
// indistinguishable from `isAsync:false` here — that is safe because
// `status:"async_launched"` is the redundant second discriminator, and
// no shape where both are absent/false can mean async.
func toolResultAsyncLaunch(signals toolResultBackgroundSignals) (agentID string, ok bool) {
	if !signals.isAsync && signals.status != "async_launched" {
		return "", false
	}
	return strings.TrimSpace(signals.agentID), true
}

// toolResultSkillFork reports whether the decoded `tool_use_result` is a
// FORKED skill's completion (claude-wire.md §E9) and, if so, returns the
// fork's agent id and skill name.
//
// The shape is `{success:true, commandName, status:"forked", agentId,
// result}`. `status:"forked"` is the discriminator and it is unique to
// this path — the `local_agent` family spells its statuses
// `async_launched` / `completed` / `failed` / `killed`, and an INLINE
// (non-forking) skill answers `{success, commandName}` with no `status`
// at all. Both `status` and a non-empty `agentId` are required: the id
// is the whole point of the stamp (it is what a later
// `can_use_tool.agent_id` addresses the fork by), so a `forked` result
// without one has nothing to bind and is left unstamped rather than
// stamped empty.
//
// A fork has NO task lifecycle whatsoever — no `task_started`, no
// task_id, no `task_progress`, no `background_tasks_changed` entry — so
// this completion is the only wire statement that the Skill row was an
// agent. `commandName` may legitimately be empty on an older build; the
// stamp still carries the id.
func toolResultSkillFork(signals toolResultBackgroundSignals) (agentID, commandName string, ok bool) {
	if signals.status != "forked" {
		return "", "", false
	}
	agentID = strings.TrimSpace(signals.agentID)
	if agentID == "" {
		return "", "", false
	}
	return agentID, strings.TrimSpace(signals.commandName), true
}

// asyncLaunchAckSentinel is the fixed opening sentence of Claude's
// async-agent launch ack. It is a single literal in the 2.1.237 bundle
// (one occurrence, verified by binary grep) and is emitted verbatim on
// both the top-level and the sidechain ack, which is what makes an
// EXACT PREFIX match on it a usable discriminator where the structured
// `tool_use_result` is absent. Prefix, never contains: arbitrary tool
// output that merely quotes the sentence somewhere in its body — a
// `grep` of this file, a pasted transcript — does not classify.
const asyncLaunchAckSentinel = "Async agent launched successfully."

// asyncLaunchAckAgentIDPrefix is the line prefix carrying the launched
// agent's id inside the ack body:
//
//	agentId: a126ec31b78a8dfc6 (internal ID - do not mention to user. ...)
//
// The id is lowercase hex; its LENGTH is deliberately not asserted (17
// chars observed, but nothing on the wire promises that).
const asyncLaunchAckAgentIDPrefix = "agentId:"

// asyncLaunchAckMaxScanLines bounds the agentId scan. The id is on line
// 2 of every captured ack; the bound exists so the scan stays O(1) on a
// body that passed the sentinel check but is not actually an ack.
const asyncLaunchAckMaxScanLines = 16

// asyncLaunchAckAgentID recognises an async-agent launch ack from the
// tool_result TEXT alone and recovers the launched agent's id — the id
// the later `system/task_updated` + `system/task_notification` pair
// addresses as `task_id`.
//
// This is the §E5b fallback for SIDECHAIN launches, where Claude omits
// the `tool_use_result` envelope entirely (verified 2026-08-19 capture:
// the sidechain ack's only top-level keys are message /
// parent_tool_use_id / session_id / subagent_type / task_description /
// timestamp / type / uuid). It is NEVER consulted when a
// `tool_use_result` is present — see the gate in appendToolResultBlock,
// which also requires the tool_use to have been an agent-launch tool.
//
// Both halves must hold. The sentinel alone would classify the resume
// ack (§E6, which has no agentId line) and any future ack sharing the
// opening sentence; an `agentId:` line alone appears in ordinary agent
// prose. Returning ("", false) means "not promoted", which is exactly
// today's behaviour — an unpromotable ack cannot be lifecycle-
// correlated, and a permanently-running card is worse than an
// instantly-done one.
func asyncLaunchAckAgentID(text string) (agentID string, ok bool) {
	if !strings.HasPrefix(text, asyncLaunchAckSentinel) {
		return "", false
	}
	rest := text
	for line := 0; line < asyncLaunchAckMaxScanLines && rest != ""; line++ {
		var current string
		if idx := strings.IndexByte(rest, '\n'); idx >= 0 {
			current, rest = rest[:idx], rest[idx+1:]
		} else {
			current, rest = rest, ""
		}
		value, found := strings.CutPrefix(current, asyncLaunchAckAgentIDPrefix)
		if !found {
			continue
		}
		if id := leadingLowerHex(strings.TrimLeft(value, " \t")); id != "" {
			return id, true
		}
	}
	return "", false
}

// leadingLowerHex returns the longest lowercase-hex prefix of s (empty
// when s does not start with one). Used to cut the agent id out of the
// ack's `agentId: <id> (internal ID - …)` line without asserting a
// length, and it is what stops a trailing `\r` on a CRLF body from
// becoming part of the id.
func leadingLowerHex(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return s[:i]
	}
	return s
}

// toolResultMonitorLaunch reports whether the decoded `tool_use_result`
// signals are the Monitor watch-task launch ack — the harness tool that
// runs a Bash command as a background task and notifies the model on
// each output event. The ack is `{taskId, timeoutMs, persistent}`
// ("Monitor started (task <id>, …)"), returned IMMEDIATELY while the
// spawned process keeps running under the CLI as a `local_bash` task —
// terminal delivery arrives later via `system/task_updated`, exactly
// like a backgrounded Bash. Captured live 2026-07-28 (AO thread
// b44a738d, session d946175f); see claude-wire.md §E7.
//
// ⚠ Discriminator subtlety: `taskId` alone is NOT sufficient — the
// TaskCreate/TaskUpdate task-list acks (`{success, taskId,
// updatedFields, …}`) also carry a top-level `taskId` and describe a
// bookkeeping row, not a process. The Monitor ack is the only observed
// shape pairing `taskId` with `persistent` / `timeoutMs`, and presence
// of either qualifies (both observed together on every capture; either
// alone still classifies correctly if a future CLI drops one).
// `persistent:false` launches ARE still background — a non-persistent
// Monitor runs until its first matching event or timeout.
func toolResultMonitorLaunch(signals toolResultBackgroundSignals) (taskID string, ok bool) {
	if !signals.taskIDSet || (!signals.persistentSet && !signals.timeoutMsSet) {
		return "", false
	}
	taskID = strings.TrimSpace(signals.taskID)
	if taskID == "" {
		return "", false
	}
	return taskID, true
}

// toolResultScheduledWakeup reports whether the decoded
// `tool_use_result` signals are a ScheduleWakeup ack, and if so the
// absolute fire time. The harness holds the wakeup as an IN-PROCESS
// timer — no task lifecycle, no wire event until the timer fires and
// the stored prompt arrives as a fresh user turn — so this ack is the
// ONLY signal that the session, while looking idle, must be kept
// alive. Shapes (captured live 2026-07-24/28, claude-wire.md §E8):
//
//	schedule: {clampedDelaySeconds, scheduledFor: <epoch-ms>, wasClamped}
//	stop:     {clampedDelaySeconds: 0, scheduledFor: 0, wasClamped, stopped: true}
//
// Returns (0, true) for the stop ack — the caller emits the clearing
// event. `scheduledFor` alone is not accepted (guarding against an
// unrelated future shape reusing the key): the sibling
// `clampedDelaySeconds` / `stopped` keys must corroborate.
func toolResultScheduledWakeup(signals toolResultBackgroundSignals) (scheduledForUnixMs int64, ok bool) {
	if !signals.scheduledForSet || (!signals.clampedDelaySet && !signals.stoppedSet) {
		return 0, false
	}
	if (signals.stoppedSet && signals.stopped) || signals.scheduledForMs <= 0 {
		return 0, true
	}
	return signals.scheduledForMs, true
}

// extractExitCode pulls `exit_code` from either the tool_result block's
// content (Bash emits it inside the structured content) or the optional
// `tool_use_result` sibling. Returns (0, false) when absent.
func extractExitCode(content json.RawMessage, toolUseResult json.RawMessage) (int, bool) {
	// tool_use_result often mirrors the Bash tool's structured output.
	if code, ok := readIntAtAnyKey(toolUseResult, "exit_code", "exitCode"); ok {
		return code, true
	}
	// Some shapes embed the code on the block content itself.
	if code, ok := readIntAtAnyKey(content, "exit_code", "exitCode"); ok {
		return code, true
	}
	return 0, false
}

type taskOutputCompletion struct {
	TaskID     string
	Summary    string
	Output     string
	IsError    bool
	ExitCode   *int
	OutputFile string
}

func extractTaskOutputCompletion(content json.RawMessage, toolUseResult json.RawMessage) (taskOutputCompletion, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(toolUseResult, &payload) != nil {
		return taskOutputCompletion{}, false
	}

	var task map[string]json.RawMessage
	if json.Unmarshal(payload["task"], &task) != nil {
		return taskOutputCompletion{}, false
	}

	taskID := firstNonEmpty(readRawString(task["task_id"]), readRawString(task["taskId"]))
	status := NormalizeTaskTerminalStatus(firstNonEmpty(readRawString(task["status"]), readRawString(payload["status"])))
	if taskID == "" || status == "" {
		return taskOutputCompletion{}, false
	}

	result := taskOutputCompletion{
		TaskID:     taskID,
		Summary:    firstNonEmpty(readRawString(task["description"]), extractToolResultText(content)),
		Output:     extractTaskOutputBody(task),
		IsError:    status != "completed",
		OutputFile: firstNonEmpty(readRawString(task["output_file"]), readRawString(task["outputFile"]), readRawString(payload["output_file"]), readRawString(payload["outputFile"])),
	}

	// exit_code / exitCode can live in any of these places depending
	// on which backgrounded tool the TaskOutput is polling:
	//   - `task.exitCode` / `task.exit_code` — local_bash
	//   - `task.result.{exit_code,exitCode}` — nested structured result
	//   - top-level `tool_use_result.exit_code` — tool-output fallback
	// Check in priority order and return the first hit.
	if exitCode, ok := readIntValueFromTask(task); ok {
		result.ExitCode = &exitCode
	} else if exitCode, ok := readIntAtAnyKey(task["result"], "exit_code", "exitCode"); ok {
		result.ExitCode = &exitCode
	} else if exitCode, ok := readIntAtAnyKey(toolUseResult, "exit_code", "exitCode"); ok {
		result.ExitCode = &exitCode
	}

	return result, true
}

func extractTaskOutputBody(task map[string]json.RawMessage) string {
	for _, key := range []string{"output", "stdout", "stderr"} {
		if value := readRawString(task[key]); value != "" {
			return value
		}
	}

	resultRaw := task["result"]
	if len(resultRaw) == 0 {
		return ""
	}
	if value := readRawString(resultRaw); value != "" {
		return value
	}

	var result map[string]json.RawMessage
	if json.Unmarshal(resultRaw, &result) == nil {
		var parts []string
		for _, key := range []string{"output", "stdout", "stderr", "message", "summary"} {
			if value := readRawString(result[key]); value != "" {
				parts = append(parts, value)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if string(resultRaw) != "null" {
		return string(resultRaw)
	}
	return ""
}

// readIntValueFromTask reads exit_code / exitCode from the task
// object itself. Pulled out of extractTaskOutputCompletion so the
// priority chain stays readable.
func readIntValueFromTask(task map[string]json.RawMessage) (int, bool) {
	for _, key := range []string{"exit_code", "exitCode"} {
		raw, ok := task[key]
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

// indexToolUseResults builds a map from tool_use_id → the structured
// result object. tool_use_result can be a single object, an object
// keyed by tool_use_id, or an array. Returns an empty (but non-nil)
// map when the input is absent so callers can use zero-value lookups
// safely.
func indexToolUseResults(value json.RawMessage) map[string]json.RawMessage {
	results := make(map[string]json.RawMessage)
	if len(value) == 0 {
		return results
	}

	addCandidate := func(candidate json.RawMessage, fallbackID string) {
		if len(candidate) == 0 {
			return
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(candidate, &obj) != nil {
			return
		}
		var id string
		if raw, ok := obj["tool_use_id"]; ok {
			_ = json.Unmarshal(raw, &id)
		}
		if id == "" {
			if raw, ok := obj["toolUseId"]; ok {
				_ = json.Unmarshal(raw, &id)
			}
		}
		if id == "" {
			id = fallbackID
		}
		if id == "" {
			return
		}
		if _, exists := results[id]; !exists {
			results[id] = candidate
		}
	}

	var arr []json.RawMessage
	if json.Unmarshal(value, &arr) == nil {
		for _, entry := range arr {
			addCandidate(entry, "")
		}
		return results
	}

	var obj map[string]json.RawMessage
	if json.Unmarshal(value, &obj) != nil {
		return results
	}
	addCandidate(value, "")
	for key, raw := range obj {
		addCandidate(raw, key)
	}
	return results
}
