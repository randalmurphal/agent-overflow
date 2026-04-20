package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

// Parser holds per-session parse state shared across NDJSON lines.
//
// The Claude SDK splits a single tool invocation across two messages: an
// assistant message carrying the `tool_use` block, and a later user message
// echoing the `tool_result` block keyed by the same `tool_use_id`. We need
// to remember per-tool flags (currently `run_in_background`) from the first
// message so the complete-side event carries the same `is_background` hint
// without re-parsing the original input.
//
// Parser is not safe for concurrent use — each Session owns one, and the
// readLoop serializes line parsing. A zero-value Parser is valid; the
// internal map lazily initialises on first write.
type Parser struct {
	// backgroundToolUses flags tool_use IDs that were started with
	// `run_in_background: true` so the matching tool_result event can be
	// tagged the same way.
	backgroundToolUses map[string]bool
	// taskToolUses correlates Claude background task lifecycle messages
	// (task_started/task_updated/TaskOutput) back to the originating
	// tool_use id so we can complete the right timeline row.
	taskToolUses map[string]string
	// completedTasks suppresses duplicate terminal task_updated events.
	completedTasks map[string]struct{}
	// taskOutputTasks suppresses duplicate TaskOutput terminal results and
	// prevents a later task_updated from clobbering richer TaskOutput data.
	taskOutputTasks map[string]struct{}
	// streamBlockTypes tracks partial-message content block types by
	// (parent_tool_use_id,index) so a later content_block_stop can identify
	// which streaming block closed.
	streamBlockTypes map[string]string
}

// NewParser returns an initialised Parser. Callers that only need one-shot
// parsing can use the package-level ParseLine helper instead.
func NewParser() *Parser {
	return &Parser{}
}

// ParseLine parses a single NDJSON line from Claude CLI stdout and returns
// zero or more ProviderEvents. This is the stateless entry point — cross-line
// correlation (e.g. background tool_use → tool_result tagging) is not
// available. Use (*Parser).ParseLine for that.
func ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	return (&Parser{}).ParseLine(threadID, line)
}

// ParseLine on a Parser preserves state across calls so tool-use / tool-result
// pairs can share metadata (e.g. the `is_background` flag).
func (p *Parser) ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var msgType string
	if err := json.Unmarshal(raw["type"], &msgType); err != nil {
		return nil, fmt.Errorf("missing or invalid type field: %w", err)
	}

	now := time.Now()

	switch msgType {
	case "system":
		return p.parseSystem(threadID, raw, now, line)
	case "assistant":
		return p.parseAssistant(threadID, raw, now, line)
	case "user":
		// Claude echoes tool results via the user role. Pick them up so
		// the triage layer can persist tool-call completions instead of
		// relying on the implicit signal at `result` (turn end).
		return p.parseUser(threadID, raw, now, line)
	case "result":
		return parseResult(threadID, raw, now, line)
	case "stream_event":
		return p.parseStreamEvent(threadID, raw, now)
	case "control_request":
		return parseControlRequest(threadID, raw, now, line)
	case "rate_limit_event":
		return parseRateLimitEvent(threadID, raw, now)
	default:
		// Unknown type — skip gracefully.
		return nil, nil
	}
}

// markBackground records that the given tool_use ID was launched with
// `run_in_background: true`. The matching tool_result event will copy
// the flag onto its Meta.
func (p *Parser) markBackground(toolUseID string) {
	if toolUseID == "" {
		return
	}
	if p.backgroundToolUses == nil {
		p.backgroundToolUses = make(map[string]bool)
	}
	p.backgroundToolUses[toolUseID] = true
}

// consumeBackground reports whether the given tool_use ID was started
// in the background AND removes the entry from the map so it doesn't
// pin memory for the rest of the session. The Claude tool_use ↔
// tool_result correlation is one-shot: once the matching user-echo
// arrives we don't need the flag again.
func (p *Parser) isBackground(toolUseID string) bool {
	if toolUseID == "" || p.backgroundToolUses == nil {
		return false
	}
	return p.backgroundToolUses[toolUseID]
}

func (p *Parser) clearBackground(toolUseID string) {
	if toolUseID == "" || p.backgroundToolUses == nil {
		return
	}
	delete(p.backgroundToolUses, toolUseID)
}

func (p *Parser) rememberTaskToolUse(taskID, toolUseID string) {
	if taskID == "" || toolUseID == "" {
		return
	}
	if p.taskToolUses == nil {
		p.taskToolUses = make(map[string]string)
	}
	p.taskToolUses[taskID] = toolUseID
}

func (p *Parser) taskToolUse(taskID string) string {
	if taskID == "" || p.taskToolUses == nil {
		return ""
	}
	return p.taskToolUses[taskID]
}

func (p *Parser) clearTask(taskID string) {
	if taskID == "" {
		return
	}
	if p.taskToolUses != nil {
		delete(p.taskToolUses, taskID)
	}
	if p.completedTasks != nil {
		delete(p.completedTasks, taskID)
	}
	if p.taskOutputTasks != nil {
		delete(p.taskOutputTasks, taskID)
	}
}

func (p *Parser) markTaskCompleted(taskID string) bool {
	if taskID == "" {
		return false
	}
	if p.completedTasks == nil {
		p.completedTasks = make(map[string]struct{})
	}
	if _, ok := p.completedTasks[taskID]; ok {
		return false
	}
	p.completedTasks[taskID] = struct{}{}
	return true
}

func (p *Parser) hasTaskOutput(taskID string) bool {
	if taskID == "" || p.taskOutputTasks == nil {
		return false
	}
	_, ok := p.taskOutputTasks[taskID]
	return ok
}

func (p *Parser) markTaskOutput(taskID string) bool {
	if taskID == "" {
		return false
	}
	if p.taskOutputTasks == nil {
		p.taskOutputTasks = make(map[string]struct{})
	}
	if _, ok := p.taskOutputTasks[taskID]; ok {
		return false
	}
	p.taskOutputTasks[taskID] = struct{}{}
	return true
}

func (p *Parser) rememberStreamBlock(parentToolUseID string, index int, blockType string) {
	if blockType == "" {
		return
	}
	if p.streamBlockTypes == nil {
		p.streamBlockTypes = make(map[string]string)
	}
	p.streamBlockTypes[streamBlockKey(parentToolUseID, index)] = blockType
}

func (p *Parser) takeStreamBlock(parentToolUseID string, index int) string {
	if p.streamBlockTypes == nil {
		return ""
	}
	key := streamBlockKey(parentToolUseID, index)
	blockType := p.streamBlockTypes[key]
	delete(p.streamBlockTypes, key)
	return blockType
}

func streamBlockKey(parentToolUseID string, index int) string {
	return fmt.Sprintf("%s:%d", parentToolUseID, index)
}

func (p *Parser) parseSystem(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var subtype string
	if err := json.Unmarshal(raw["subtype"], &subtype); err != nil {
		return nil, nil // no subtype — skip
	}

	switch subtype {
	case "init":
		meta, _ := json.Marshal(extractSessionInfo(raw))
		return []provider.ProviderEvent{{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
			Raw:       line,
		}}, nil

	case "session_state_changed":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "session_state_changed",
			Meta:      raw["data"],
			Timestamp: now,
		}}, nil

	case "tool_progress":
		// Streaming tool progress is intentionally dropped. The chat rewrite
		// renders successive tool_call summary upserts rather than a parallel
		// progress-event channel.
		return nil, nil

	case "compact_boundary":
		meta := extractCompactBoundaryMeta(raw)
		return []provider.ProviderEvent{{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "api_retry":
		meta := raw["data"]
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "retrying",
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "task_started":
		taskID := readRawString(raw["task_id"])
		toolUseID := firstNonEmpty(readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))
		p.rememberTaskToolUse(taskID, toolUseID)
		return nil, nil

	case "task_updated":
		return p.parseTaskLifecycleEvent(threadID, raw, now)

	case "task_notification":
		// Notifications are informational only. They follow task_updated /
		// TaskOutput and must never drive lifecycle.
		return nil, nil

	// Explicitly skipped subtypes — no action, no error.
	case "hook_started", "hook_progress", "hook_response",
		"notification",
		"files_persisted",
		"tool_use_summary",
		"memory_recall",
		"local_command_output",
		"task_progress":
		return nil, nil

	default:
		// Unknown system subtype — skip.
		return nil, nil
	}
}

func (p *Parser) parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
			Signature string          `json:"signature,omitempty"`
		} `json:"content"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage,omitempty"`
	}

	// The message payload is under "message" key for assistant type.
	if rawMsg, ok := raw["message"]; ok {
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			return nil, fmt.Errorf("parse assistant message: %w", err)
		}
	} else {
		// Might be flat — try parsing raw directly.
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, nil
		}
	}

	// Top-level parent_tool_use_id links subagent (Task-tool) child messages
	// to their parent Task tool use. It's not always present, and only a
	// string when it is.
	var parentToolUseID string
	if v, ok := raw["parent_tool_use_id"]; ok {
		_ = json.Unmarshal(v, &parentToolUseID)
	}

	var events []provider.ProviderEvent

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventTextDelta,
				ThreadID:        threadID,
				ItemID:          msg.ID,
				Content:         block.Text,
				Role:            "assistant",
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})

		case "tool_use":
			if block.Name == "ExitPlanMode" {
				planMarkdown := extractExitPlanModePlan(block.Input)
				if planMarkdown == "" {
					continue
				}
				events = append(events, provider.ProviderEvent{
					Kind:            provider.EventProposedPlan,
					ThreadID:        threadID,
					ItemID:          block.ID,
					ItemType:        block.Name,
					Content:         planMarkdown,
					ParentToolUseID: parentToolUseID,
					Timestamp:       now,
				})
				continue
			}

			isBackground := hasRunInBackground(block.Input)
			if isBackground {
				p.markBackground(block.ID)
			}

			meta := marshalToolMeta(block.Name, block.Input, isBackground)
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventToolStart,
				ThreadID:        threadID,
				ItemID:          block.ID,
				ItemType:        block.Name,
				Meta:            meta,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})

		case "thinking":
			var meta json.RawMessage
			if block.Signature != "" {
				meta, _ = json.Marshal(map[string]any{
					"signature": block.Signature,
				})
			}
			events = append(events, provider.ProviderEvent{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				ItemID:          msg.ID,
				Content:         block.Thinking,
				Meta:            meta,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			})
		}
	}

	// Emit token usage if present.
	if msg.Usage != nil {
		usageMeta, _ := json.Marshal(provider.TokenUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		})
		events = append(events, provider.ProviderEvent{
			Kind:            provider.EventTokenUsage,
			ThreadID:        threadID,
			Meta:            usageMeta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		})
	}

	return events, nil
}

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
		var blockType string
		if err := json.Unmarshal(block["type"], &blockType); err != nil || blockType != "tool_result" {
			continue
		}

		var toolUseID string
		if err := json.Unmarshal(block["tool_use_id"], &toolUseID); err != nil || toolUseID == "" {
			// A tool_result without an ID can't be correlated back to a
			// tool_use, so drop it rather than emit an orphan completion.
			continue
		}

		var isError bool
		if v, ok := block["is_error"]; ok {
			_ = json.Unmarshal(v, &isError)
		}

		content := extractToolResultText(block["content"])

		taskOutputMeta := toolUseResults[toolUseID]
		if len(taskOutputMeta) == 0 {
			taskOutputMeta = raw["tool_use_result"]
		}
		if taskOutput, ok := extractTaskOutputCompletion(block["content"], taskOutputMeta); ok {
			originalToolUseID := p.taskToolUse(taskOutput.TaskID)
			if originalToolUseID != "" && p.markTaskOutput(taskOutput.TaskID) {
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
			}
			continue
		}

		if p.isBackground(toolUseID) {
			// Claude echoes a placeholder tool_result for backgrounded tools
			// before the real terminal task lifecycle lands. Treat it as
			// informational only; task_updated / TaskOutput emit the
			// authoritative completion later.
			continue
		}

		metaFields := map[string]any{
			"is_error": isError,
		}
		if code, ok := extractExitCode(block["content"], toolUseResults[toolUseID]); ok {
			metaFields["exit_code"] = code
		}

		meta, _ := json.Marshal(metaFields)
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    toolUseID,
			Content:   content,
			Meta:      meta,
			Timestamp: now,
			Raw:       line,
		})
	}

	return events, nil
}

// hasRunInBackground returns true when the tool input JSON contains
// `"run_in_background": true`. Malformed JSON is treated as absent —
// this is a best-effort hint, not a correctness-critical value.
func hasRunInBackground(input json.RawMessage) bool {
	if len(input) == 0 {
		return false
	}
	var parsed struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &parsed); err != nil {
		return false
	}
	return parsed.RunInBackground
}

// marshalToolMeta builds the EventToolStart Meta payload. We omit
// `is_background` when false so pipelines downstream don't have to
// distinguish "explicitly foreground" from "unknown" — absence is the
// default.
func marshalToolMeta(toolName string, input json.RawMessage, isBackground bool) json.RawMessage {
	fields := map[string]any{
		"toolName": toolName,
		"input":    input,
	}
	if isBackground {
		fields["is_background"] = true
	}
	out, _ := json.Marshal(fields)
	return out
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

// readIntAtAnyKey returns the first integer-valued field in `data` matching
// one of `keys`. Works on both plain objects and objects nested inside an
// array (returning the first hit).
func readIntAtAnyKey(data json.RawMessage, keys ...string) (int, bool) {
	if len(data) == 0 {
		return 0, false
	}

	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		for _, key := range keys {
			if v, ok := obj[key]; ok {
				var n int
				if json.Unmarshal(v, &n) == nil {
					return n, true
				}
			}
		}
		return 0, false
	}

	var arr []map[string]json.RawMessage
	if json.Unmarshal(data, &arr) == nil {
		for _, entry := range arr {
			for _, key := range keys {
				if v, ok := entry[key]; ok {
					var n int
					if json.Unmarshal(v, &n) == nil {
						return n, true
					}
				}
			}
		}
	}
	return 0, false
}

func readRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (p *Parser) parseTaskLifecycleEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	if taskID == "" {
		return nil, nil
	}
	if p.hasTaskOutput(taskID) {
		return nil, nil
	}

	var patch map[string]json.RawMessage
	if json.Unmarshal(raw["patch"], &patch) != nil {
		return nil, nil
	}
	status := normalizeTaskTerminalStatus(firstNonEmpty(
		readRawString(patch["status"]),
		readRawString(raw["status"]),
	))
	if status == "" {
		return nil, nil
	}

	toolUseID := firstNonEmpty(p.taskToolUse(taskID), readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))
	if toolUseID == "" {
		return nil, nil
	}
	if !p.markTaskCompleted(taskID) {
		return nil, nil
	}

	metaFields := map[string]any{
		"is_background": true,
		"task_id":       taskID,
	}
	if status != "completed" {
		metaFields["is_error"] = true
	}
	if endTime, ok := readIntAtAnyKey(raw["patch"], "end_time", "endTime"); ok {
		metaFields["end_time"] = endTime
	}
	meta, _ := json.Marshal(metaFields)

	p.clearBackground(toolUseID)

	return []provider.ProviderEvent{{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   firstNonEmpty(readRawString(patch["description"]), readRawString(raw["summary"])),
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

func normalizeTaskTerminalStatus(status string) string {
	switch status {
	case "completed":
		return "completed"
	case "failed", "killed", "error", "errored", "interrupted", "stopped":
		return "failed"
	default:
		return ""
	}
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

func parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var isError bool
	if v, ok := raw["is_error"]; ok {
		_ = json.Unmarshal(v, &isError)
	}

	if isError {
		var errMsg string
		if v, ok := raw["error"]; ok {
			_ = json.Unmarshal(v, &errMsg)
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errMsg,
			Timestamp: now,
			Raw:       line,
		}}, nil
	}

	var events []provider.ProviderEvent

	// Extract usage/cost data from the result summary.
	usage := extractResultUsage(raw)
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		usageMeta, _ := json.Marshal(usage)
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      usageMeta,
			Timestamp: now,
		})
	}

	events = append(events, provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Timestamp: now,
		Raw:       line,
	})

	return events, nil
}

// extractResultUsage parses token usage from a Claude result message.
// It checks both "usage" (flat format) and "modelUsage" (per-model format)
// and aggregates total_cost_usd when present.
func extractResultUsage(raw map[string]json.RawMessage) provider.TokenUsage {
	var usage provider.TokenUsage

	// Try flat "usage" object first.
	if v, ok := raw["usage"]; ok {
		var u struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		}
		if json.Unmarshal(v, &u) == nil {
			usage.InputTokens = u.InputTokens
			usage.OutputTokens = u.OutputTokens
			usage.CacheReadInputTokens = u.CacheReadInputTokens
			usage.CacheCreationInputTokens = u.CacheCreationInputTokens
		}
	}

	// Aggregate from "modelUsage" if flat usage was empty.
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		if v, ok := raw["modelUsage"]; ok {
			var models map[string]struct {
				InputTokens              int     `json:"inputTokens"`
				OutputTokens             int     `json:"outputTokens"`
				CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
				CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
				CostUSD                  float64 `json:"costUSD"`
			}
			if json.Unmarshal(v, &models) == nil {
				for _, m := range models {
					usage.InputTokens += m.InputTokens
					usage.OutputTokens += m.OutputTokens
					usage.CacheReadInputTokens += m.CacheReadInputTokens
					usage.CacheCreationInputTokens += m.CacheCreationInputTokens
					usage.TotalCostUSD += m.CostUSD
				}
			}
		}
	}

	// Override cost with explicit total_cost_usd if present.
	if v, ok := raw["total_cost_usd"]; ok {
		var cost float64
		if json.Unmarshal(v, &cost) == nil && cost > 0 {
			usage.TotalCostUSD = cost
		}
	}

	return usage
}

// parseStreamEvent handles the `stream_event` envelope produced by the
// Claude CLI when `--include-partial-messages` is enabled. Unlike the
// assistant-message path, partial messages preserve content block
// boundaries, so we emit explicit start/stop events for text/thinking blocks
// and remember the block type by (parent_tool_use_id,index) so a later stop
// can settle the right streaming item.
func (p *Parser) parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	eventRaw := raw["data"]
	if len(eventRaw) == 0 {
		eventRaw = raw["event"]
	}
	if len(eventRaw) == 0 {
		return nil, nil
	}

	var eventObj map[string]json.RawMessage
	if json.Unmarshal(eventRaw, &eventObj) != nil {
		return nil, nil
	}

	eventType := firstNonEmpty(readRawString(eventObj["type"]), readRawString(raw["event"]))
	if eventType == "" {
		return nil, nil
	}

	parentToolUseID := readRawString(raw["parent_tool_use_id"])

	switch eventType {
	case "content_block_start":
		index, _ := readIntAtAnyKey(eventRaw, "index")
		var block map[string]json.RawMessage
		if json.Unmarshal(eventObj["content_block"], &block) != nil {
			return nil, nil
		}
		blockType := readRawString(block["type"])
		if blockType == "" {
			return nil, nil
		}
		p.rememberStreamBlock(parentToolUseID, index, blockType)
		meta, _ := json.Marshal(map[string]any{
			"index":         index,
			"blockType":     blockType,
			"content_block": json.RawMessage(eventObj["content_block"]),
		})
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStart,
			ThreadID:        threadID,
			Meta:            meta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_stop":
		index, _ := readIntAtAnyKey(eventRaw, "index")
		blockType := p.takeStreamBlock(parentToolUseID, index)
		meta, _ := json.Marshal(map[string]any{
			"index":     index,
			"blockType": blockType,
		})
		return []provider.ProviderEvent{{
			Kind:            provider.EventContentBlockStop,
			ThreadID:        threadID,
			Meta:            meta,
			ParentToolUseID: parentToolUseID,
			Timestamp:       now,
		}}, nil

	case "content_block_delta":
		deltaRaw, ok := eventObj["delta"]
		if !ok {
			return nil, nil
		}
		var delta struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		}
		if json.Unmarshal(deltaRaw, &delta) != nil {
			return nil, nil
		}
		switch delta.Type {
		case "text_delta":
			if delta.Text == "" {
				return nil, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventTextDelta,
				ThreadID:        threadID,
				Content:         delta.Text,
				Role:            "assistant",
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, nil
		case "thinking_delta":
			if delta.Thinking == "" {
				return nil, nil
			}
			return []provider.ProviderEvent{{
				Kind:            provider.EventThinking,
				ThreadID:        threadID,
				Content:         delta.Thinking,
				ParentToolUseID: parentToolUseID,
				Timestamp:       now,
			}}, nil
		default:
			return nil, nil
		}
	default:
		return nil, nil
	}
}

// parseControlRequest handles the wire format:
// {"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"permission_suggestions":[...]}}
//
// `permission_suggestions` is preserved as opaque JSON so downstream code
// (UI / approval handling) can surface the Claude SDK suggestions.
func parseControlRequest(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var requestID string
	if v, ok := raw["request_id"]; ok {
		_ = json.Unmarshal(v, &requestID)
	}

	reqRaw, ok := raw["request"]
	if !ok {
		return nil, nil
	}

	var req struct {
		Subtype               string          `json:"subtype"`
		ToolName              string          `json:"tool_name"`
		ToolUseID             string          `json:"tool_use_id"`
		ToolUseIDCamel        string          `json:"toolUseId"`
		Input                 json.RawMessage `json:"input"`
		PermissionSuggestions json.RawMessage `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(reqRaw, &req); err != nil {
		return nil, nil
	}

	if req.Subtype != "can_use_tool" {
		return nil, nil
	}

	toolUseID := req.ToolUseID
	if toolUseID == "" {
		toolUseID = req.ToolUseIDCamel
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:             requestID,
		ThreadID:              threadID,
		ToolUseID:             toolUseID,
		ToolName:              req.ToolName,
		Description:           fmt.Sprintf("Allow %s?", req.ToolName),
		Input:                 req.Input,
		Title:                 req.ToolName,
		PermissionSuggestions: req.PermissionSuggestions,
	})

	return []provider.ProviderEvent{{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		ItemID:    requestID,
		Meta:      approvalMeta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

func extractExitPlanModePlan(input json.RawMessage) string {
	var payload struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return ""
	}
	return payload.Plan
}

// parseRateLimitEvent handles Claude's rate_limit_event message type.
func parseRateLimitEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	infoRaw, ok := raw["rate_limit_info"]
	if !ok {
		return nil, nil
	}

	var info struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	}
	if json.Unmarshal(infoRaw, &info) != nil {
		return nil, nil
	}

	entry := provider.RateLimitEntry{
		LimitID:   info.RateLimitType,
		LimitName: info.RateLimitType,
		ResetsAt:  info.ResetsAt,
	}
	if info.Status != "allowed" {
		entry.UsedPercent = 100
	}

	snapshot := provider.RateLimitsSnapshot{
		Provider:  string(provider.Claude),
		Limits:    []provider.RateLimitEntry{entry},
		UpdatedAt: now.UnixMilli(),
	}
	meta, _ := json.Marshal(snapshot)

	return []provider.ProviderEvent{{
		Kind:      provider.EventRateLimits,
		ThreadID:  threadID,
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

// extractSessionInfo reads fields from the init message top level.
func extractSessionInfo(raw map[string]json.RawMessage) provider.SessionInfo {
	var info provider.SessionInfo

	if v, ok := raw["session_id"]; ok {
		json.Unmarshal(v, &info.SessionID)
	}
	if v, ok := raw["model"]; ok {
		json.Unmarshal(v, &info.Model)
	}
	if v, ok := raw["cwd"]; ok {
		json.Unmarshal(v, &info.CWD)
	}
	if v, ok := raw["tools"]; ok {
		json.Unmarshal(v, &info.Tools)
	}
	if v, ok := raw["claude_code_version"]; ok {
		json.Unmarshal(v, &info.Version)
	}
	if v, ok := raw["slash_commands"]; ok {
		json.Unmarshal(v, &info.SlashCommands)
	}

	return info
}
