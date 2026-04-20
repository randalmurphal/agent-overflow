// Package claude — parser for `user`-type NDJSON lines. The top-level
// parseUser dispatches each tool_result content block to either the
// task-output helper (appendTaskOutputCompletion) or the standard tool
// completion helper (appendToolResultCompletion). Keeping each block's
// logic in its own helper isolates the task-correlation rules from the
// routine completion path.

package claude

import (
	"encoding/json"
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

	// user.message.content can be either a plain string (a user-typed
	// message echoed back) or an array of blocks. Only the array form
	// carries tool_result.
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

// appendToolResultBlock classifies one tool_result block: either a
// task-output completion (background Task tool), a backgrounded-tool
// placeholder (dropped), or a standard inline tool completion. The
// function returns the events slice with any produced events appended;
// blocks that should not emit an event (wrong type, missing id,
// placeholder echo) return the slice unchanged.
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

	content := extractToolResultText(block["content"])

	taskOutputMeta := toolUseResults[toolUseID]
	if len(taskOutputMeta) == 0 {
		taskOutputMeta = raw["tool_use_result"]
	}
	if updated, handled := p.appendTaskOutputCompletion(
		events, threadID, now, line,
		block, content, taskOutputMeta,
	); handled {
		return updated
	}

	if p.isBackground(toolUseID) {
		// Claude echoes a placeholder tool_result for backgrounded tools
		// before the real terminal task lifecycle lands. Treat it as
		// informational only; task_updated / TaskOutput emit the
		// authoritative completion later.
		return events
	}

	return appendToolResultCompletion(
		events, threadID, toolUseID, now, line,
		block, content, toolUseResults[toolUseID],
	)
}

// appendTaskOutputCompletion handles the task_id-carrying "Task tool"
// completion path: when the block's tool_use_result carries a
// TaskOutput payload we look up the original tool_use_id via the
// parser's in-memory task map and emit a background-flagged
// EventToolComplete. The second return reports whether the block was
// consumed by this helper; false means "not a task-output shape, try
// the standard path."
func (p *Parser) appendTaskOutputCompletion(
	events []provider.ProviderEvent,
	threadID string,
	now time.Time,
	line []byte,
	block map[string]json.RawMessage,
	content string,
	taskOutputMeta json.RawMessage,
) ([]provider.ProviderEvent, bool) {
	taskOutput, ok := extractTaskOutputCompletion(block["content"], taskOutputMeta)
	if !ok {
		return events, false
	}

	originalToolUseID := p.taskToolUse(taskOutput.TaskID)
	if originalToolUseID == "" || !p.markTaskOutput(taskOutput.TaskID) {
		// Already emitted (dedupe) or no matching tool_use — either way
		// the block is fully consumed; we just don't emit.
		return events, true
	}

	metaFields := map[string]any{
		"is_background": true,
		"task_id":       taskOutput.TaskID,
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
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    originalToolUseID,
		Content:   firstNonEmpty(taskOutput.Summary, content),
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	})
	p.clearBackground(originalToolUseID)
	return events, true
}

// appendToolResultCompletion emits the standard inline tool completion
// shape: EventToolComplete keyed by the original tool_use_id, with
// is_error / exit_code surfaced into meta. Used for every non-background,
// non-task tool_result.
func appendToolResultCompletion(
	events []provider.ProviderEvent,
	threadID, toolUseID string,
	now time.Time,
	line []byte,
	block map[string]json.RawMessage,
	content string,
	toolUseResult json.RawMessage,
) []provider.ProviderEvent {
	var isError bool
	if v, ok := block["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}

	metaFields := map[string]any{
		"is_error": isError,
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
	status := normalizeTaskTerminalStatus(firstNonEmpty(readRawString(task["status"]), readRawString(payload["status"])))
	if taskID == "" || status == "" {
		return taskOutputCompletion{}, false
	}

	result := taskOutputCompletion{
		TaskID:     taskID,
		Summary:    firstNonEmpty(readRawString(task["description"]), extractToolResultText(content)),
		IsError:    status != "completed",
		OutputFile: firstNonEmpty(readRawString(task["output_file"]), readRawString(task["outputFile"]), readRawString(payload["output_file"]), readRawString(payload["outputFile"])),
	}

	if exitCode, ok := readIntAtAnyKey(task["result"], "exit_code", "exitCode"); ok {
		result.ExitCode = &exitCode
	} else if exitCode, ok := readIntAtAnyKey(toolUseResult, "exit_code", "exitCode"); ok {
		result.ExitCode = &exitCode
	}

	return result, true
}

// indexToolUseResults builds a map from tool_use_id → the structured
// result object. Mirrors the forge adapter's behavior: tool_use_result can
// be a single object, an object keyed by tool_use_id, or an array. Returns
// an empty (but non-nil) map when the input is absent so callers can use
// zero-value lookups safely.
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
