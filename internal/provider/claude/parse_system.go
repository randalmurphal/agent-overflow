// Package claude — parser for `system`-type NDJSON lines (init metadata,
// compact_boundary, and the task_started / task_updated / task_notification
// triples that drive Claude's background-task lifecycle).

package claude

import (
	"encoding/json"
	"strings"
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
		info := extractSessionInfo(raw)
		// Remember the model id so result usage can be priced without a
		// round-trip to the store. p is nil only in the package-level
		// ParseLine helper's test-only fast path; in that path we can't
		// price anyway, so skip the assignment.
		if p != nil {
			p.model = strings.TrimSpace(info.Model)
		}
		meta, _ := json.Marshal(info)
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
		// Normalize the SDK's `data` payload into a shape both providers
		// share: `attempt` (1-indexed), `max_retries`, and an `error`
		// string. Triage uses these to render the timeline retry row
		// (hiding attempts < 4, mirroring Claude Code's TUI). The raw
		// `data` is preserved under `wire` for forensics.
		retryMeta := buildClaudeAPIRetryMeta(raw["data"])
		return []provider.ProviderEvent{{
			Kind:      provider.EventAPIRetry,
			ThreadID:  threadID,
			Meta:      retryMeta,
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

// parseTaskLifecycleEvent handles `system/task_updated`. A terminal
// `patch.status` (`completed`, `failed`, `killed`) emits
// `EventBackgroundTaskTerminal` for the backgrounded task — the
// authoritative basic-terminal signal for the task lifecycle. A later
// TaskOutput enrichment for the same task idempotently upserts through
// triage with richer payload. Non-terminal `patch.status` values
// (`pending`, `running`) are no-ops; dedup is triage's job.
// See docs/references/claude-wire.md §task_updated and
// docs/architecture/turn-lifecycle.md §Task lifecycle.
func (p *Parser) parseTaskLifecycleEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	if taskID == "" {
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

	// An empty tool_use_id here means the in-memory map is empty (fresh
	// adapter session after reconnect) AND the event did not echo the
	// id inline. Emit a terminal keyed only by task_id so triage can
	// look the row up via items.meta.task_id. If triage finds no match
	// the event is dropped there.
	toolUseID := firstNonEmpty(p.taskToolUse(taskID), readRawString(raw["tool_use_id"]), readRawString(raw["toolUseId"]))

	metaFields := map[string]any{
		"task_id": taskID,
		"status":  status,
		"source":  "task_updated",
	}
	if toolUseID != "" {
		metaFields["tool_use_id"] = toolUseID
	}
	if status != "completed" {
		metaFields["is_error"] = true
	}
	if endTime, ok := readIntAtAnyKey(raw["patch"], "end_time", "endTime"); ok {
		metaFields["end_time"] = endTime
	}
	meta, _ := json.Marshal(metaFields)

	return []provider.ProviderEvent{{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   firstNonEmpty(readRawString(patch["description"]), readRawString(raw["summary"])),
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

// parseTaskNotificationEvent surfaces Claude's non-lifecycle
// `system/task_notification` attention signal. This event must never be
// interpreted as task completion; triage persists it as a lightweight
// notification row and may read the referenced output_file into SQLite
// for later expansion on an already-terminal sibling row.
func (p *Parser) parseTaskNotificationEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	taskID := readRawString(raw["task_id"])
	if taskID == "" {
		return nil, nil
	}
	toolUseID := firstNonEmpty(
		readRawString(raw["tool_use_id"]),
		readRawString(raw["toolUseId"]),
		p.taskToolUse(taskID),
	)
	status := strings.TrimSpace(firstNonEmpty(readRawString(raw["status"]), readRawString(raw["patch"])))
	summary := readRawString(raw["summary"])
	outputFile := firstNonEmpty(readRawString(raw["output_file"]), readRawString(raw["outputFile"]))

	metaFields := map[string]any{
		"task_id": taskID,
		"source":  "task_notification",
	}
	if toolUseID != "" {
		metaFields["tool_use_id"] = toolUseID
	}
	if status != "" {
		metaFields["status"] = status
	}
	if outputFile != "" {
		metaFields["output_file"] = outputFile
	}
	meta, _ := json.Marshal(metaFields)

	return []provider.ProviderEvent{{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		Content:   summary,
		Meta:      meta,
		Timestamp: now,
	}}, nil
}

func normalizeTaskTerminalStatus(status string) string {
	switch status {
	case "completed":
		return "completed"
	case "killed":
		// Preserve `killed` distinctly so triage can render a gray "Stopped"
		// badge instead of the generic red "Failed" bucket. The CLI emits
		// this on the follow-up task_updated that fires after a successful
		// stop_task control_request.
		return "killed"
	case "failed", "error", "errored", "interrupted", "stopped":
		return "failed"
	default:
		return ""
	}
}

// buildClaudeAPIRetryMeta normalizes a `system.api_retry.data` payload
// into the shared {attempt, max_retries, error} EventAPIRetry meta
// shape. The SDK's `data.error` is an object whose `.message` field
// carries the human-readable copy; we pull the message string up so
// triage can pass it to the row summary verbatim. Missing fields stay
// zero-valued so the triage handler treats them as "unknown" rather
// than fabricating a label.
func buildClaudeAPIRetryMeta(rawData json.RawMessage) json.RawMessage {
	fields := map[string]any{}
	if len(rawData) == 0 {
		return nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(rawData, &data) != nil {
		return rawData
	}
	if attempt, ok := readIntValue(data, "attempt"); ok {
		fields["attempt"] = attempt
	}
	if maxRetries, ok := readIntValue(data, "max_retries", "maxRetries"); ok {
		fields["max_retries"] = maxRetries
	}
	if errMsg := readNestedErrorMessage(data); errMsg != "" {
		fields["error"] = errMsg
	}
	if retryAfter, ok := readIntValue(data, "retry_after_ms", "retryAfterMs"); ok {
		fields["retry_after_ms"] = retryAfter
	}
	fields["wire"] = rawData
	out, err := json.Marshal(fields)
	if err != nil {
		return rawData
	}
	return out
}

// readNestedErrorMessage pulls the human copy out of a Claude
// `system.api_retry.data.error` field. The wire shape we've observed
// is `error: { message: string, name?: string }` — so we look for
// `.message` first, then fall through to a flat string in case the
// SDK ever switches to that shape.
func readNestedErrorMessage(data map[string]json.RawMessage) string {
	raw, ok := data["error"]
	if !ok {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil && asString != "" {
		return asString
	}
	var asObj map[string]json.RawMessage
	if json.Unmarshal(raw, &asObj) != nil {
		return ""
	}
	return readRawString(asObj["message"])
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
