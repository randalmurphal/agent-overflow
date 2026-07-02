// Package claude — parser for `user`-type NDJSON lines. The top-level
// parseUser dispatches each tool_result content block to either the
// task-output helper (appendTaskOutputCompletion) or the standard tool
// completion helper (appendToolResultCompletion). Keeping each block's
// logic in its own helper isolates the task-correlation rules from the
// routine completion path.

package claude

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// parseUser picks up the Claude `user` echo of a prior assistant tool_use.
// The content is a list of `tool_result` blocks (other user-role messages
// have a string content and aren't interesting at this layer). Each block
// becomes one EventToolComplete keyed by the original tool_use_id.
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

	// user.message.content can be either a plain string or an array of blocks.
	// User-authored block messages contain text/image blocks; only tool_result
	// blocks are interesting at this layer.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, nil
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
	// Three INDEPENDENT signals mark a tool_result as a backgrounded
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
	//
	// ⚠ An INLINE (awaited) agent's real completion also carries
	// `agentId` + `status:"completed"` in its `tool_use_result`, so
	// `toolResultAsyncLaunch` keys ONLY on `isAsync`/`status:"async_launched"`
	// — never on mere `agentId` presence — or every inline agent
	// completion would misclassify as backgrounded.
	//
	// Signals (2) and (3) are decoded from `tool_use_result` in a
	// single pass — the sibling can be megabytes of Bash stdout, so
	// per-signal re-decodes are not acceptable on this path.
	flaggedAtLaunch := p.isBackground(toolUseID)
	backgroundSignals := readToolResultBackgroundSignals(toolUseResultRaw)
	markedOnWire := toolResultBackgrounded(backgroundSignals)
	asyncAgentID, asyncLaunched := toolResultAsyncLaunch(backgroundSignals)
	isBackground := flaggedAtLaunch || markedOnWire || asyncLaunched
	events = appendToolResultCompletion(
		events, threadID, toolUseID, now, line,
		isBackground,
		block, content, toolUseResultRaw,
	)
	// The async ack IS the task lifecycle's task_id ↔ tool_use_id
	// correlation (normally learned ~4ms earlier from
	// `system/task_started`). Recording it here too means a parser that
	// reconnected and missed task_started can still resolve the later
	// `task_updated`/`task_notification` terminal back to this launch.
	// rememberTaskToolUse is idempotent, so re-deriving an already-known
	// mapping from task_started is a harmless no-op.
	if asyncLaunched && asyncAgentID != "" {
		p.rememberTaskToolUse(asyncAgentID, toolUseID)
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
// placeholder (claude-wire.md §E2) or an async local_agent launch ack
// (§E5). It exists so appendToolResultBlock decodes the raw sibling
// exactly ONCE per tool_result: `tool_use_result` can be megabytes for
// Bash stdout / Read payloads, and each `map[string]json.RawMessage`
// decode copies value bytes per key — the per-signal readXAtAnyKey
// calls this replaces cost three full decodes on every tool result.
//
// The *Set fields track presence so the defensive array-of-objects
// fallback keeps the readXAtAnyKey family's first-successful-hit
// semantics: a later entry must not overwrite a value an earlier entry
// already decoded.
type toolResultBackgroundSignals struct {
	backgroundTaskID string
	isAsync          bool
	status           string
	agentID          string

	backgroundTaskIDSet bool
	isAsyncSet          bool
	statusSet           bool
	agentIDSet          bool
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
	if !s.isAsyncSet {
		if raw, ok := obj["isAsync"]; ok {
			var v bool
			if json.Unmarshal(raw, &v) == nil {
				s.isAsync = v
				s.isAsyncSet = true
			}
		}
	}
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
