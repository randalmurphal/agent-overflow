package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
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
	// UpdateOnly is set by the Codex item/updated emitter to mark this
	// as an in-place refresh of an existing tool_call. When true, triage
	// must mutate only Summary/Meta and NEVER reset status back to
	// "running" for a row that already reached a terminal state.
	UpdateOnly bool `json:"update_only"`
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

	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, evt.ItemType, "tool")
	summary := buildToolCallSummary(meta, evt.ItemType)

	// Codex item/updated is an in-place refresh (not a fresh start). If the
	// existing row has already completed/errored/declined, only refresh
	// Summary/Meta — never flip status back to "running".
	if meta.UpdateOnly {
		if !found {
			// No row yet — the update is meaningless without a launch row
			// to annotate. Drop silently rather than fabricate one; the
			// subsequent item/completed will create the row if needed.
			return nil
		}
		updated := existing
		if strings.TrimSpace(summary) != "" {
			updated.Summary = summary
		}
		if toolName != "" {
			updated.ToolName = toolName
		}
		updated.UpdatedAt = now
		return r.persistItem(updated, nil)
	}

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
		item.ParentID = stringsx.FirstNonEmptyTrimmed(eventParentID(evt), existing.ParentID)
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

	// Command-output payloads accumulate meta jitter across the streaming
	// hot path: every delta rewrites the payload's meta using just the
	// delta's content, so the collapsed card's "N lines" counter bounces
	// around instead of converging on the total. Rebuild the meta once at
	// completion from the cumulative payload data so the final card
	// reflects the full command output. This is one full blob read per
	// command — acceptable overhead at turn boundary — and only touches
	// the payload.meta column (data blob stays put).
	if launch.PayloadID != "" && launch.PayloadKind == "command_output" {
		if err := r.rebuildCommandOutputMeta(launch.PayloadID); err != nil {
			// Meta jitter is a presentation concern; do not fail the
			// turn because we couldn't fix it. Log and continue so
			// the tool lifecycle completes cleanly.
			log.Printf("triage: rebuild command_output meta %s: %v", launch.PayloadID, err)
		}
	}

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

// rebuildCommandOutputMeta recomputes command_output payload meta from
// the cumulative payload data. Used at EventToolComplete to counteract
// the per-delta meta jitter in the streaming append path — each delta
// updates meta from the delta alone (line count of THIS chunk), so the
// final collapsed card would otherwise show the last chunk's count
// rather than the total. Reading the full blob once here and writing
// fresh meta is cheap compared to the per-delta savings we keep by not
// reading the blob back on every append.
func (r *Router) rebuildCommandOutputMeta(payloadID string) error {
	data, err := r.store.GetPayloadData(payloadID)
	if err != nil {
		return fmt.Errorf("read payload for command_output meta rebuild: %w", err)
	}
	pm, err := r.store.GetPayloadMeta(payloadID)
	if err != nil {
		return fmt.Errorf("read existing command_output meta: %w", err)
	}
	// Preserve whatever command / exitCode the streaming path captured
	// on the last delta — they do not accumulate, so the last-seen
	// value is already the correct terminal value.
	var prior CommandOutputMeta
	_ = json.Unmarshal([]byte(pm.Meta), &prior)
	cumulative := ExtractCommandOutputMeta(string(data), prior.Command, prior.ExitCode)
	cumulativeJSON, err := json.Marshal(cumulative)
	if err != nil {
		return fmt.Errorf("marshal cumulative command_output meta: %w", err)
	}
	if string(cumulativeJSON) == pm.Meta {
		return nil
	}
	return r.store.UpdatePayloadMeta(payloadID, string(cumulativeJSON))
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

// findToolCallByTaskID resolves the tool_call row on the thread whose
// persisted items.meta JSON carries a matching task_id. Used by the
// background completion router when the adapter's in-memory
// task_id ↔ tool_use_id map has been lost (reconnect with a fresh
// parser). Delegates to the store's indexed query (partial expression
// index from migration v17) so the lookup is O(log N) instead of the
// former O(items) scan + per-row JSON unmarshal.
//
// The store query does not filter by kind — the partial index already
// narrows to rows carrying a task_id, which today are only ever
// tool_call rows. The explicit kind guard below stays as a defence in
// depth so a future unrelated kind that adopts task_id can't silently
// be mistaken for a background tool completion. Returns (Item{}, false,
// nil) when no row matches so callers can log-and-drop without
// surfacing a user error.
func (r *Router) findToolCallByTaskID(threadID, taskID string) (store.Item, bool, error) {
	item, found, err := r.store.FindToolCallItemByTaskID(threadID, taskID)
	if err != nil || !found {
		return store.Item{}, false, err
	}
	if item.Kind != itemKindToolCall {
		return store.Item{}, false, nil
	}
	return item, true, nil
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
