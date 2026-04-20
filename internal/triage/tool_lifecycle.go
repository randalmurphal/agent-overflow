package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const (
	itemKindToolCall       = "tool_call"
	itemKindBackgroundDone = "tool_completion"

	payloadKindToolCallResult = "tool_call_result"

	statusRunning   = "running"
	statusCompleted = "completed"
	statusErrored   = "errored"
)

type toolStartMeta struct {
	ToolName     string          `json:"toolName"`
	Input        json.RawMessage `json:"input"`
	IsBackground bool            `json:"is_background"`
	TaskID       string          `json:"task_id"`
}

type toolCompleteMeta struct {
	IsBackground bool   `json:"is_background"`
	IsError      bool   `json:"is_error"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	ItemStatus   string `json:"item_status,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
}

func (r *Router) persistToolCallLaunch(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	if itemID == "" {
		return nil
	}

	meta := decodeToolStartMeta(evt.Meta)
	now := eventTimestampMillis(evt)

	// A "meta update" EventToolStart carries only task_id (no toolName,
	// no input) — Claude's `system/task_started` uses it to attach the
	// task_id ↔ tool_use_id mapping to an existing tool_call row so
	// reconnect recovery can correlate later task_updated / task_notification
	// events via items.meta.task_id. Treat this as a targeted meta merge
	// that preserves the existing summary and tool_name.
	metaUpdateOnly := strings.TrimSpace(meta.ToolName) == "" &&
		len(meta.Input) == 0 &&
		meta.TaskID != ""

	existing, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("tool launch lookup %s: %w", itemID, err)
	}
	if found && existing.Kind != itemKindToolCall {
		return nil
	}

	if metaUpdateOnly {
		if !found {
			// No existing row to annotate. The tool_use block must have
			// been dropped (fresh session) — drop the meta update rather
			// than fabricate a ghost tool_call row.
			return nil
		}
		mergedMeta, err := mergeItemMetaTaskID(existing.Meta, meta.TaskID)
		if err != nil {
			log.Printf("triage: merge task_id into item meta %s: %v", itemID, err)
			return nil
		}
		if mergedMeta == existing.Meta {
			return nil
		}
		existing.Meta = mergedMeta
		existing.UpdatedAt = now
		return r.persistItem(existing, nil)
	}

	toolName := firstNonEmptyString(strings.TrimSpace(meta.ToolName), strings.TrimSpace(evt.ItemType), "tool")
	summary := buildToolCallSummary(meta, evt.ItemType)
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("tool launch turn index %s: %w", itemID, err)
	}

	item := store.Item{
		ID:           itemID,
		ThreadID:     evt.ThreadID,
		TurnIndex:    turnIndex,
		Kind:         itemKindToolCall,
		Role:         "assistant",
		Status:       statusRunning,
		Summary:      summary,
		ParentID:     eventParentID(evt),
		IsBackground: meta.IsBackground,
		ToolName:     toolName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if found {
		item = existing
		item.Summary = summary
		item.ParentID = firstNonEmptyString(eventParentID(evt), existing.ParentID)
		item.ToolName = toolName
		item.IsBackground = existing.IsBackground || meta.IsBackground
		if existing.Status == "" {
			item.Status = statusRunning
		}
		if existing.Decision == "" {
			item.Decision = r.takeApprovalDecision(evt.ThreadID, itemID)
		}
		item.UpdatedAt = now
	} else {
		item.Decision = r.takeApprovalDecision(evt.ThreadID, itemID)
	}

	return r.persistItem(item, nil)
}

func (r *Router) persistToolCallCompletion(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	meta := decodeToolCompleteMeta(evt.Meta)

	if itemID == "" {
		// Fresh-session task_updated carries only task_id (no inline
		// tool_use_id) — the adapter's in-memory map is empty so it
		// emits a completion keyed by meta.task_id and lets triage
		// resolve back to the tool_call row whose meta.task_id matches.
		if meta.TaskID == "" {
			return nil
		}
		resolved, ok, err := r.findToolCallByTaskID(evt.ThreadID, meta.TaskID)
		if err != nil {
			return fmt.Errorf("tool completion task_id lookup %s: %w", meta.TaskID, err)
		}
		if !ok {
			log.Printf("triage: task_updated for task_id=%q on thread %s had no matching tool_call item; dropping", meta.TaskID, evt.ThreadID)
			return nil
		}
		itemID = resolved.ID
	}

	launch, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("tool completion lookup %s: %w", itemID, err)
	}
	if !found || launch.Kind != itemKindToolCall {
		return nil
	}

	now := eventTimestampMillis(evt)
	status := completionStatus(meta)

	if launch.IsBackground || meta.IsBackground {
		completionID := nextToolCompletionID(launch.ID)
		turnIndex, err := r.currentTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("background completion turn index %s: %w", completionID, err)
		}

		completion := store.Item{
			ID:           completionID,
			ThreadID:     evt.ThreadID,
			TurnIndex:    turnIndex,
			Kind:         itemKindBackgroundDone,
			Role:         "assistant",
			Status:       status,
			Summary:      buildBackgroundCompletionSummary(launch.Summary, meta),
			ParentID:     launch.ParentID,
			IsBackground: true,
			CompletionOf: launch.ID,
			ToolName:     launch.ToolName,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if existing, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID); err == nil && ok {
			completion = existing
			completion.Status = status
			completion.Summary = buildBackgroundCompletionSummary(launch.Summary, meta)
			completion.ParentID = launch.ParentID
			completion.IsBackground = true
			completion.CompletionOf = launch.ID
			completion.ToolName = launch.ToolName
			completion.UpdatedAt = now
		} else if err != nil {
			return fmt.Errorf("background completion lookup %s: %w", completionID, err)
		}

		return r.maybeDeferOrPersist(evt.ThreadID, completion, completionPayload(launch.ID, evt, meta, now))
	}

	launch.Status = status
	launch.Summary = buildCompletionSummary(launch.Summary, meta)
	launch.UpdatedAt = now

	payload := completionPayload(launch.ID, evt, meta, now)
	switch {
	case payload == nil:
		return r.persistItem(launch, nil)
	case launch.PayloadID == "":
		return r.persistItem(launch, payload)
	case launch.PayloadKind == payloadKindToolCallResult:
		payload.ID = launch.PayloadID
		return r.persistItem(launch, payload)
	default:
		return r.persistItem(launch, nil)
	}
}

func (r *Router) turnIndexForEvent(evt provider.ProviderEvent) (int, error) {
	if evt.ParentToolUseID != "" {
		parent, found, err := r.store.GetThreadItem(evt.ThreadID, eventParentID(evt))
		if err != nil {
			return 0, err
		}
		if found {
			return parent.TurnIndex, nil
		}
	}
	return r.currentTurnIndex(evt.ThreadID)
}

func (r *Router) emitItemUpsert(item store.Item) {
	r.emit("provider:item_upsert", item)
}

func decodeToolStartMeta(raw json.RawMessage) toolStartMeta {
	if len(raw) == 0 {
		return toolStartMeta{}
	}
	var m toolStartMeta
	if json.Unmarshal(raw, &m) != nil {
		return toolStartMeta{}
	}
	return m
}

func decodeToolCompleteMeta(raw json.RawMessage) toolCompleteMeta {
	if len(raw) == 0 {
		return toolCompleteMeta{}
	}
	var m toolCompleteMeta
	if json.Unmarshal(raw, &m) != nil {
		return toolCompleteMeta{}
	}
	return m
}

func buildToolCallSummary(meta toolStartMeta, itemType string) string {
	name := strings.TrimSpace(meta.ToolName)
	if name == "" {
		name = strings.TrimSpace(itemType)
	}
	if name == "" {
		name = "tool"
	}
	preview := toolInputPreview(meta.Input)
	if preview == "" {
		return name
	}
	return name + ": " + preview
}

func buildCompletionSummary(launchSummary string, meta toolCompleteMeta) string {
	suffix := completionSuffix(meta)
	if suffix == "" {
		return launchSummary
	}
	return launchSummary + " " + suffix
}

func buildBackgroundCompletionSummary(launchSummary string, meta toolCompleteMeta) string {
	outcome := backgroundCompletionOutcome(meta)
	if outcome == "" {
		return launchSummary
	}
	if launchSummary == "" {
		return outcome
	}
	return launchSummary + " -> " + outcome
}

func backgroundCompletionOutcome(meta toolCompleteMeta) string {
	switch {
	case meta.ExitCode != nil:
		return fmt.Sprintf("exit %d", *meta.ExitCode)
	case meta.IsError:
		return "error"
	case meta.ItemStatus == "failed":
		return "failed"
	case meta.ItemStatus == "killed":
		return "killed"
	case meta.ItemStatus == "declined":
		return "declined"
	default:
		return "done"
	}
}

func completionSuffix(meta toolCompleteMeta) string {
	switch {
	case meta.IsError:
		return "(error)"
	case meta.ExitCode != nil && *meta.ExitCode != 0:
		return fmt.Sprintf("(exit %d)", *meta.ExitCode)
	case meta.ItemStatus == "failed":
		return "(failed)"
	case meta.ItemStatus == "killed":
		return "(killed)"
	case meta.ItemStatus == "declined":
		return "(declined)"
	default:
		return ""
	}
}

func completionStatus(meta toolCompleteMeta) string {
	if meta.IsError || meta.ItemStatus == "failed" || meta.ItemStatus == "killed" {
		return statusErrored
	}
	if meta.ItemStatus == "declined" {
		return "declined"
	}
	return statusCompleted
}

func toolInputPreview(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"} {
		if raw, ok := obj[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return truncatePreview(s, 80)
			}
		}
	}
	return ""
}

func truncatePreview(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func completionPayload(itemID string, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) *store.Payload {
	if evt.Content == "" && len(evt.Meta) == 0 {
		return nil
	}
	header := map[string]any{}
	if meta.ExitCode != nil {
		header["exitCode"] = *meta.ExitCode
	}
	if meta.IsError {
		header["isError"] = true
	}
	if meta.ItemStatus != "" {
		header["itemStatus"] = meta.ItemStatus
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		headerJSON = []byte("{}")
	}
	return &store.Payload{
		ID:        "tool-call-result:" + itemID,
		Kind:      payloadKindToolCallResult,
		Meta:      string(headerJSON),
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
}

// findToolCallByTaskID scans the thread's items for the tool_call row
// whose meta.task_id matches. Used by background completion routing
// when the adapter's in-memory task_id ↔ tool_use_id map has been lost
// (reconnect with a fresh parser). Returns (Item{}, false, nil) when
// no row matches so callers can log-and-drop without surfacing a user
// error.
func (r *Router) findToolCallByTaskID(threadID, taskID string) (store.Item, bool, error) {
	if taskID == "" {
		return store.Item{}, false, nil
	}
	items, err := r.store.ListItems(threadID)
	if err != nil {
		return store.Item{}, false, err
	}
	for _, it := range items {
		if it.Kind != itemKindToolCall {
			continue
		}
		if metaContainsTaskID(it.Meta, taskID) {
			return it, true, nil
		}
	}
	return store.Item{}, false, nil
}

// metaContainsTaskID reports whether the persisted items.meta JSON has
// a top-level "task_id" field equal to taskID. Empty / malformed meta
// returns false rather than an error — item meta is provider-scoped
// and we don't want a stray value to blow up background routing.
func metaContainsTaskID(rawMeta, taskID string) bool {
	if rawMeta == "" || taskID == "" {
		return false
	}
	var parsed map[string]json.RawMessage
	if json.Unmarshal([]byte(rawMeta), &parsed) != nil {
		return false
	}
	raw, ok := parsed["task_id"]
	if !ok {
		return false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value == taskID
}

// mergeItemMetaTaskID merges a task_id value into an existing
// items.meta JSON blob. Returns the original string unchanged when
// task_id is already present with the same value (so the caller can
// skip an unnecessary upsert). Other meta fields are preserved.
func mergeItemMetaTaskID(existing, taskID string) (string, error) {
	if taskID == "" {
		return existing, nil
	}
	if existing == "" {
		existing = "{}"
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(existing), &parsed); err != nil {
		// Malformed meta — rebuild with just the task_id rather than
		// silently carry the broken payload forward.
		parsed = map[string]json.RawMessage{}
	}
	if raw, ok := parsed["task_id"]; ok {
		var current string
		if json.Unmarshal(raw, &current) == nil && current == taskID {
			return existing, nil
		}
	}
	encoded, err := json.Marshal(taskID)
	if err != nil {
		return existing, err
	}
	parsed["task_id"] = encoded
	out, err := json.Marshal(parsed)
	if err != nil {
		return existing, err
	}
	return string(out), nil
}
