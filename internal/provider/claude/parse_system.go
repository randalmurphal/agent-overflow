// Package claude — parser for `system`-type NDJSON lines (init metadata,
// compact_boundary, and the task_started / task_updated / task_notification
// triples that drive Claude's background-task lifecycle).

package claude

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

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
		if taskID == "" || toolUseID == "" {
			return nil, nil
		}
		// Re-emit an EventToolStart carrying task_id so triage can
		// persist the task_id ↔ tool_use_id mapping into items.meta.
		// On reconnect with a fresh in-memory parser, a later
		// task_updated carries only task_id; persisted meta lets triage
		// correlate back to the original tool_use item. The event is
		// minimal (no toolName/input) — triage merges task_id into the
		// existing item meta without clobbering the launch summary.
		meta, _ := json.Marshal(map[string]any{
			"task_id": taskID,
		})
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    toolUseID,
			Meta:      meta,
			Timestamp: now,
		}}, nil

	case "task_updated":
		return p.parseTaskLifecycleEvent(threadID, raw, now)

	case "task_notification":
		return p.parseTaskNotificationEvent(threadID, raw, now)

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
	// An empty tool_use_id here means the in-memory map is empty (fresh
	// adapter session after reconnect) AND the event did not echo the
	// id inline. Emit a completion keyed only by task_id so triage can
	// look the row up via items.meta.task_id. If triage finds no match
	// the event is dropped there.
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

	if toolUseID != "" {
		p.clearBackground(toolUseID)
		// Dedup parallel task_notification for the same tool_use_id so
		// the downstream notification signal is suppressed.
		if !p.markToolUseCompleted(toolUseID) {
			return nil, nil
		}
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   firstNonEmpty(readRawString(patch["description"]), readRawString(raw["summary"])),
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

// parseTaskNotificationEvent handles `system/task_notification`. It is a
// parallel completion signal: Claude sends it out-of-turn to get the
// model's attention on the next turn. The tool_use_id is inline, so this
// event is self-sufficient even without a prior task_started in memory
// (fresh session after reconnect). In the normal path, `task_updated`
// has already fired and added the id to `completedToolUseIDs` — this
// call no-ops.
func (p *Parser) parseTaskNotificationEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	toolUseID := firstNonEmpty(p.taskToolUse(taskID), readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))
	if toolUseID == "" {
		// Without a tool_use_id we can't build a targeted completion.
		// Task_notification without an inline tool_use_id and no
		// task_started in memory cannot be correlated; drop it.
		return nil, nil
	}
	// Per-task dedup covers the fresh-session edge case where
	// task_updated fired with only task_id (no tool_use_id) and could
	// not populate completedToolUseIDs.
	if taskID != "" {
		if _, alreadyCompleted := p.completedTasks[taskID]; alreadyCompleted {
			return nil, nil
		}
	}
	if !p.markToolUseCompleted(toolUseID) {
		return nil, nil
	}
	status := normalizeTaskTerminalStatus(readRawString(raw["status"]))
	if status == "" {
		// Default to completed — notifications only fire for terminal tasks.
		status = "completed"
	}

	metaFields := map[string]any{
		"is_background": true,
	}
	if taskID != "" {
		metaFields["task_id"] = taskID
	}
	if status != "completed" {
		metaFields["is_error"] = true
	}
	meta, _ := json.Marshal(metaFields)

	p.clearBackground(toolUseID)
	if taskID != "" {
		// Record the task in completedTasks so any later task_updated
		// for the same task is suppressed.
		p.markTaskCompleted(taskID)
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   readRawString(raw["summary"]),
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
