package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

const (
	itemKindAssistantText  = "assistant_text"
	itemKindThinking       = "thinking"
	itemKindToolCall       = "tool_call"
	itemKindBackgroundDone = "tool_completion"
	itemKindNotification   = "notification"

	payloadKindToolCallResult = "tool_call_result"

	statusRunning   = "running"
	statusStreaming = "streaming"
	statusCompleted = "completed"
	statusErrored   = "errored"
	// statusKilled is a distinct terminal state for user-initiated stops
	// (Claude's stop_task control_request). It stays separate from
	// statusErrored so the UI can render a gray "Stopped" badge rather
	// than the red "Failed" bucket — the task didn't fail, the user
	// cancelled it. Added alongside the Phase 1 stop_task primitive; see
	// docs/archive/background-tasks-plan.md.
	statusKilled = "killed"
)

type toolStartMeta struct {
	ToolName      string          `json:"toolName"`
	Input         json.RawMessage `json:"input"`
	IsBackground  bool            `json:"is_background"`
	TaskID        string          `json:"task_id"`
	SubagentModel string          `json:"subagent_model"`
}

type toolCompleteMeta struct {
	IsBackground bool            `json:"is_background"`
	IsError      bool            `json:"is_error"`
	ExitCode     *int            `json:"exit_code,omitempty"`
	ItemStatus   string          `json:"item_status,omitempty"`
	TaskID       string          `json:"task_id,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
}

func (r *Router) persistToolCallLaunch(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	if itemID == "" {
		return nil
	}

	meta := decodeToolStartMeta(evt.Meta)
	now := eventTimestampMillis(evt)

	// A "meta update" EventToolStart carries only correlation fields
	// (no toolName, no input). Two emit sites today:
	//   - Claude's `system/task_started` attaches the task_id ↔
	//     tool_use_id mapping so reconnect recovery can correlate
	//     later task_updated / task_notification events via
	//     items.meta.task_id.
	//   - Subagent assistant envelopes attach `subagent_model` to the
	//     parent Task/Agent tool_call so the UI can render
	//     `<agent_type> (<Model>)` in the card header without
	//     plumbing a separate event kind. The model is taken from
	//     `message.model` on the first subagent assistant envelope.
	// Either (or both) field is treated as a targeted merge that
	// preserves the existing summary, tool_name, and status.
	metaUpdateOnly := strings.TrimSpace(meta.ToolName) == "" &&
		len(meta.Input) == 0 &&
		(meta.TaskID != "" || meta.SubagentModel != "")

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
		mergedMeta, err := mergeItemMetaCorrelationFields(existing.Meta, meta.TaskID, meta.SubagentModel)
		if err != nil {
			log.Printf("triage: merge correlation fields into item meta %s: %v", itemID, err)
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
		Meta:         validJSONObjectString(evt.Meta),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if found {
		item = existing
		item.Summary = summary
		item.ParentID = stringsx.FirstNonEmptyTrimmed(eventParentID(evt), existing.ParentID)
		item.ToolName = toolName
		item.IsBackground = existing.IsBackground || meta.IsBackground
		item.Meta = mergeItemMetaJSON(existing.Meta, evt.Meta)
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
		return nil
	}

	launch, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("tool completion lookup %s: %w", itemID, err)
	}
	if !found {
		codexThread, err := r.isCodexThread(evt.ThreadID)
		if err != nil {
			return err
		}
		if !codexThread || !shouldPersistCodexCompletionWithoutLaunch(meta.ToolName) {
			return nil
		}
		return r.persistToolCallCompletedWithoutLaunch(evt, meta)
	}
	if launch.Kind != itemKindToolCall {
		return nil
	}

	// Spec invariant 23 (docs/architecture/turn-lifecycle.md §Force-close
	// safety net): once the turn-complete handler has force-closed a
	// running tool_call to `errored`, the turn is over and any late
	// EventToolComplete is noise — it must not resurrect the row or
	// rewrite the force-close summary. We extend that rule to every
	// non-running terminal (errored / completed / declined): a row that
	// has already settled should not be re-opened by a duplicate /
	// out-of-order completion event. This mirrors the approval-resolved
	// guard in approvals.go, which also refuses to overwrite a terminal
	// status.
	if launch.Status != statusRunning {
		log.Printf("triage: dropping late EventToolComplete for %s (status=%q already terminal)", itemID, launch.Status)
		return nil
	}

	now := eventTimestampMillis(evt)
	codexThread, err := r.isCodexThread(evt.ThreadID)
	if err != nil {
		return err
	}
	if codexThread && shouldSplitCodexToolCompletion(launch.ToolName) {
		return r.persistSplitToolCompletion(launch, evt, meta, now)
	}

	// Backgrounded tool_result is a placeholder — Claude sends it to
	// close the wire-level tool_use block (universal tool-lifecycle
	// invariant 20) but the real terminal for the task arrives later
	// via EventBackgroundTaskTerminal. Per spec:
	// docs/architecture/turn-lifecycle.md §Tool lifecycle, the launch
	// row must STAY running + is_background=true. Refresh the
	// is_background meta on the launch row (in case the start event
	// missed the flag) but don't flip status and don't create a sibling
	// tool_completion — that sibling now lives under
	// handleBackgroundTaskTerminal.
	if launch.IsBackground || meta.IsBackground {
		if meta.IsBackground && !launch.IsBackground {
			launch.IsBackground = true
			launch.UpdatedAt = now
			return r.persistItem(launch, nil)
		}
		// Launch row already correctly flagged; the placeholder
		// completion carries no additional state to persist. The
		// background task terminal (task_updated / TaskOutput) will
		// write the sibling completion row when it arrives.
		return nil
	}

	status := completionStatus(meta)
	launch.Status = status
	launch.Summary = buildCompletionSummary(completionBaseSummary(launch, meta, evt.ItemType), meta)
	if strings.TrimSpace(meta.ToolName) != "" {
		launch.ToolName = strings.TrimSpace(meta.ToolName)
	}
	launch.Meta = mergeItemMetaJSON(launch.Meta, evt.Meta)
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

func (r *Router) persistToolCallCompletedWithoutLaunch(evt provider.ProviderEvent, meta toolCompleteMeta) error {
	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, evt.ItemType, "tool")
	now := eventTimestampMillis(evt)
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("tool completion turn index %s: %w", eventItemID(evt), err)
	}
	item := store.Item{
		ID:        eventItemID(evt),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    completionStatus(meta),
		Summary:   buildCompletionSummary(buildToolCallSummary(toolStartMeta{ToolName: toolName, Input: meta.Input}, evt.ItemType), meta),
		ParentID:  eventParentID(evt),
		ToolName:  toolName,
		Meta:      validJSONObjectString(evt.Meta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload := completionPayload(item.ID, evt, meta, now)
	return r.persistItem(item, payload)
}

func shouldSplitCodexToolCompletion(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "wait_agent", "resume_agent":
		return true
	default:
		return false
	}
}

func shouldPersistCodexCompletionWithoutLaunch(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "send_input", "close_agent":
		return true
	default:
		return false
	}
}

func (r *Router) persistSplitToolCompletion(launch store.Item, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) error {
	launch.Status = statusCompleted
	launch.UpdatedAt = now
	launch.Meta = mergeItemMetaJSON(launch.Meta, evt.Meta)
	if err := r.persistItem(launch, nil); err != nil {
		return err
	}
	completionID := nextToolCompletionID(launch.ID)
	completion := store.Item{
		ID:           completionID,
		ThreadID:     evt.ThreadID,
		TurnIndex:    launch.TurnIndex,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       completionStatus(meta),
		Summary:      buildCompletionSummary(completionBaseSummary(launch, meta, evt.ItemType), meta),
		ParentID:     launch.ParentID,
		CompletionOf: launch.ID,
		ToolName:     launch.ToolName,
		Meta:         validJSONObjectString(evt.Meta),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	payload := completionPayload(completion.ID, evt, meta, now)
	return r.persistItem(completion, payload)
}

func (r *Router) isCodexThread(threadID string) (bool, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return false, fmt.Errorf("lookup thread provider %s: %w", threadID, err)
	}
	return thread.Provider == "codex", nil
}

// backgroundTaskTerminalMeta is the decoded view of
// EventBackgroundTaskTerminal.Meta. Fields mirror what Claude's
// parse_system (for task_updated) and parse_user (for TaskOutput
// enrichment) emit: docs/architecture/turn-lifecycle.md §Task lifecycle.
type backgroundTaskTerminalMeta struct {
	TaskID     string `json:"task_id"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	Status     string `json:"status"`
	Source     string `json:"source,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	OutputFile string `json:"output_file,omitempty"`
	EndTime    int64  `json:"end_time,omitempty"`
}

func decodeBackgroundTaskTerminalMeta(raw json.RawMessage) backgroundTaskTerminalMeta {
	if len(raw) == 0 {
		return backgroundTaskTerminalMeta{}
	}
	var m backgroundTaskTerminalMeta
	if json.Unmarshal(raw, &m) != nil {
		return backgroundTaskTerminalMeta{}
	}
	return m
}

// handleBackgroundTaskTerminal dispatches the additive Claude
// task-lifecycle event by source.
//
// Three phases of "background task ended" are deliberately
// decoupled:
//
//   - source="task_updated", status in {completed, failed}: HOST
//     signalled process exit but the agent has not observed yet.
//     Triage stashes the terminal in
//     pending_background_task_terminals (via
//     stashBackgroundTaskTerminal) so the tray can hide the launch.
//     Reconnect-replay is idempotent (PK is thread_id+task_id). The
//     chat-side `tool_completion` sibling is NOT written here — it
//     comes later via task_notification or TaskOutput drain.
//
//   - source="task_updated", status="killed": user-initiated stop
//     (StopClaudeTask → stop_task control_request → CLI replies
//     task_updated{killed}). The user already knows the process was
//     stopped — there's nothing for the agent to "observe". Write
//     the sibling immediately so chat shows the killed badge without
//     waiting for the next turn's task_notification.
//
//   - source anything else (today: "task_output"): AGENT observed
//     via TaskOutput tool_result. Triage drains the stash if present,
//     merges stash data with the observation, and writes the sibling
//     at the current write head (via writeBackgroundCompletionSibling).
//
// task_notification arrives via a separate event handler
// (handleBackgroundTaskNotification); when its task has a stash it
// drains and writes the sibling through the same shared helper.
//
// Persistence uses maybeDeferOrPersist so a mid-stream terminal queues
// behind the active streaming block (invariant 11 — intended-order
// item_index assignment).
func (r *Router) handleBackgroundTaskTerminal(evt provider.ProviderEvent) error {
	meta := decodeBackgroundTaskTerminalMeta(evt.Meta)
	if meta.Source == "" {
		meta.Source = "task_updated"
	}

	if meta.Source == "task_updated" && meta.Status != "killed" {
		return r.stashBackgroundTaskTerminal(evt, meta)
	}
	return r.observeBackgroundTaskTerminal(evt, meta)
}

// stashBackgroundTaskTerminal records the host-side process exit
// without writing a chat row. The stash row is what the tray query
// joins against to hide the still-running-from-the-agent's-view
// launch (Tray-A: tray reflects process state, not agent state).
//
// Launch resolution is best-effort. A missing launch is acceptable —
// observation may still arrive later (carrying its own task_id) and
// the stash will be drained by id alone.
func (r *Router) stashBackgroundTaskTerminal(evt provider.ProviderEvent, meta backgroundTaskTerminalMeta) error {
	if meta.TaskID == "" {
		log.Printf("triage: task_updated stash dropped — no task_id (thread=%s)", evt.ThreadID)
		return nil
	}

	toolUseID := strings.TrimSpace(evt.ItemID)
	if toolUseID == "" {
		toolUseID = strings.TrimSpace(meta.ToolUseID)
	}
	// Best-effort: if the parser's task_id ↔ tool_use_id map was lost
	// across reconnect, look up the persisted launch via the
	// items.meta.task_id index.
	if toolUseID == "" {
		if launch, found, err := r.findToolCallByTaskID(evt.ThreadID, meta.TaskID); err == nil && found {
			toolUseID = launch.ID
		}
	}

	now := eventTimestampMillis(evt)
	stash := store.PendingBackgroundTaskTerminal{
		ThreadID:   evt.ThreadID,
		TaskID:     meta.TaskID,
		ToolUseID:  toolUseID,
		Status:     stringsxFirst(meta.Status, "completed"),
		OutputFile: meta.OutputFile,
		Source:     meta.Source,
		CreatedAt:  now,
	}
	if meta.ExitCode != nil {
		ec := int64(*meta.ExitCode)
		stash.ExitCode = &ec
	}
	if meta.EndTime != 0 {
		et := meta.EndTime
		stash.EndTime = &et
	}

	if err := r.store.UpsertPendingBackgroundTerminal(stash); err != nil {
		return fmt.Errorf("triage: stash background terminal %s/%s: %w", evt.ThreadID, meta.TaskID, err)
	}

	r.emit("provider:background_task_state", BackgroundTaskStateEvent{
		ThreadID:  evt.ThreadID,
		TaskID:    meta.TaskID,
		LaunchID:  toolUseID,
		State:     "exited",
		UpdatedAt: now,
	})
	return nil
}

// RecoverOrphanedBackgroundTasks walks every persisted backgrounded
// tool_call row that is still `running`, has no completion sibling,
// and has no stash entry — these are launches whose owning provider
// session died with the previous app instance. The agent will never
// observe completion (the session is gone), so we synthesise the
// observation here by writing the chat-side `tool_completion` sibling
// directly with source="session_died" and status="killed".
//
// Called once during App.ServiceStartup, after the store is open and
// before any provider session can spawn. Idempotent: running this twice
// is a no-op because the second pass sees the existing sibling and
// skips the launch via the NOT EXISTS predicate in
// ListOrphanedBackgroundLaunches. Crash-safe: if the process dies
// mid-loop before a sibling is written, the launch row is still
// `running` with no sibling and no stash, so the next boot's sweep
// finds it again — we never write a stash entry for the recovery path,
// so there's no half-state to leak.
//
// Launches whose meta carries no task_id (rare race: tool_use block
// persisted before the task_started envelope) skip the sibling write —
// without a task_id we have no idempotency key.
//
// Returns the count of recovered launches; logs but does not propagate
// per-launch errors so one bad row can't poison the whole sweep.
func (r *Router) RecoverOrphanedBackgroundTasks() (int, error) {
	launches, err := r.store.ListOrphanedBackgroundLaunches()
	if err != nil {
		return 0, fmt.Errorf("triage: list orphaned bg launches: %w", err)
	}
	now := time.Now().UnixMilli()
	recovered := 0
	for _, launch := range launches {
		taskID := taskIDFromItemMeta(launch.Meta)
		if taskID == "" {
			log.Printf("triage: skip orphan recovery for %s/%s (no task_id meta)",
				launch.ThreadID, launch.ID)
			continue
		}

		syntheticEvt := provider.ProviderEvent{
			ThreadID:  launch.ThreadID,
			ItemID:    launch.ID,
			Timestamp: time.UnixMilli(now),
		}
		meta := backgroundTaskTerminalMeta{
			TaskID:    taskID,
			ToolUseID: launch.ID,
			Status:    "killed",
			Source:    "session_died",
		}
		// Write the sibling directly — no stash dance. The orphan
		// query already proved no stash exists for this launch, and
		// writing the sibling is the same terminal step the steady-
		// state observation path takes after draining a stash.
		if err := r.writeBackgroundCompletionSibling(syntheticEvt, meta, false); err != nil {
			log.Printf("triage: synthesise session_died sibling %s/%s: %v", launch.ThreadID, taskID, err)
			continue
		}
		recovered++
	}
	return recovered, nil
}

// observeBackgroundTaskTerminal handles the agent-observation half
// (TaskOutput tool_result enrichment). Drains the stash if any and
// writes the sibling at the current write head.
func (r *Router) observeBackgroundTaskTerminal(evt provider.ProviderEvent, meta backgroundTaskTerminalMeta) error {
	stash, stashFound, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, meta.TaskID)
	if err != nil {
		log.Printf("triage: drain stash for %s/%s: %v", evt.ThreadID, meta.TaskID, err)
	}
	if stashFound {
		mergeStashIntoTerminalMeta(&meta, stash)
	}
	return r.writeBackgroundCompletionSibling(evt, meta, stashFound)
}

// writeBackgroundCompletionSibling writes (or upserts) the
// `tool_completion` sibling row at the current write head and emits
// the tray-state drained event so the frontend can refresh.
//
// Used by:
//   - observeBackgroundTaskTerminal (TaskOutput tool_result drain)
//   - handleBackgroundTaskNotification (task_notification drain)
//   - recoverOrphanedBackgroundTasks (startup synthesised drain)
//
// Launch resolution prefers the event's tool_use_id, then meta, then
// the items.meta.task_id index. When no launch exists, the sibling is
// written standalone with id `complete:by-task:<task_id>` and an empty
// CompletionOf — the frontend renders these as standalone background
// completion cards rather than folding them under a parent launch.
func (r *Router) writeBackgroundCompletionSibling(evt provider.ProviderEvent, meta backgroundTaskTerminalMeta, stashWasDrained bool) error {
	launch, launchFound, err := r.resolveBackgroundTaskLaunch(evt.ThreadID, evt.ItemID, meta.ToolUseID, meta.TaskID)
	if err != nil {
		return err
	}
	if launchFound && launch.Kind != itemKindToolCall {
		// Defensive — task_id index already filters by kind, but a
		// future kind that adopts task_id mustn't be folded in.
		launchFound = false
		launch = store.Item{}
	}

	var notification store.Item
	var notificationFound bool
	if meta.TaskID != "" {
		if item, found, err := r.findTaskNotificationItem(evt.ThreadID, meta.TaskID); err != nil {
			log.Printf("triage: lookup task notification for %s: %v", meta.TaskID, err)
		} else if found {
			notification = item
			notificationFound = true
			notificationMeta := decodeBackgroundTaskNotificationMeta(json.RawMessage(notification.Meta))
			if meta.OutputFile == "" && notificationMeta.OutputFile != "" {
				meta.OutputFile = notificationMeta.OutputFile
			}
		}
	}

	now := eventTimestampMillis(evt)
	status := backgroundTerminalStatus(meta)
	completionID := backgroundCompletionID(launch.ID, meta.TaskID)
	launchTurnIndex := 0
	parentID := ""
	toolName := ""
	launchSummary := ""
	if launchFound {
		launchTurnIndex = launch.TurnIndex
		parentID = launch.ParentID
		toolName = launch.ToolName
		launchSummary = launch.Summary
	}
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launchTurnIndex)
	if err != nil {
		log.Printf("triage: background task terminal turn index %s: %v", completionID, err)
	}

	completion := store.Item{
		ID:           completionID,
		ThreadID:     evt.ThreadID,
		TurnIndex:    turnIndex,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       status,
		Summary:      buildBackgroundTerminalSummary(launchSummary, evt.Content, meta),
		ParentID:     parentID,
		IsBackground: true,
		CompletionOf: launch.ID, // empty when launch missing — standalone row
		ToolName:     toolName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	itemMeta := backgroundCompletionItemMeta(meta, backgroundTerminalHasRichSummary(evt.Content, meta))
	if notificationFound {
		notificationMeta := decodeBackgroundTaskNotificationMeta(json.RawMessage(notification.Meta))
		outputState, readError := notificationOutputState(notification.Meta)
		itemMeta = mergeBackgroundCompletionItemMeta(
			itemMeta,
			backgroundNotificationCompletionMeta(notificationMeta, notification.PayloadID != "", outputState, readError),
		)
	}
	completion.Meta = itemMeta

	// Preserve an already-persisted sibling's created_at and
	// item_index (persistItem keeps item_index on update), but
	// overwrite the mutable fields so a second call with richer
	// payload enriches the row.
	var existing *store.Item
	if persisted, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID); err == nil && ok {
		existing = &persisted
		completion.CreatedAt = persisted.CreatedAt
		completion.TurnIndex = persisted.TurnIndex
		completion.ItemIndex = persisted.ItemIndex
		completion.PayloadID = persisted.PayloadID
		completion.Meta = mergeBackgroundCompletionItemMeta(persisted.Meta, itemMeta)
		if shouldKeepExistingBackgroundStatus(persisted.Meta, meta.Source) {
			completion.Status = persisted.Status
		}
		if shouldKeepExistingBackgroundSummary(persisted.Meta, evt.Content, meta) {
			completion.Summary = persisted.Summary
		}
	} else if err != nil {
		return fmt.Errorf("bg task terminal existing lookup %s: %w", completionID, err)
	}
	if notificationFound && notification.PayloadID != "" {
		completion.PayloadID = notification.PayloadID
	}

	var payload *store.Payload
	payloadKey := launch.ID
	if payloadKey == "" {
		payloadKey = "by-task:" + meta.TaskID
	}
	if completion.PayloadID == "" && meta.OutputFile != "" {
		payload = r.backgroundOutputFilePayload(payloadKey, meta.OutputFile, now)
	}
	if payload == nil && completion.PayloadID == "" {
		payload = backgroundTerminalPayload(payloadKey, evt, meta, now)
	}
	if payload == nil && existing != nil && existing.PayloadID != "" {
		completion.PayloadID = existing.PayloadID
	}

	if err := r.maybeDeferOrPersist(evt.ThreadID, completion, payload); err != nil {
		return err
	}

	if stashWasDrained {
		r.emit("provider:background_task_state", BackgroundTaskStateEvent{
			ThreadID:  evt.ThreadID,
			TaskID:    meta.TaskID,
			LaunchID:  launch.ID,
			State:     "drained",
			UpdatedAt: now,
		})
	}
	return nil
}

// mergeStashIntoTerminalMeta folds the stashed terminal data (host
// truth from task_updated) into the in-flight observation event meta.
// The merge is monotonic and observation-wins: already-set observation
// fields stay set; the stash fills only the gaps. The observation can
// arrive with richer status/exit_code/output_file (e.g. TaskOutput
// tool_result), so we never overwrite a populated field with stale
// stash data.
func mergeStashIntoTerminalMeta(meta *backgroundTaskTerminalMeta, stash store.PendingBackgroundTaskTerminal) {
	if meta.TaskID == "" {
		meta.TaskID = stash.TaskID
	}
	if meta.ToolUseID == "" {
		meta.ToolUseID = stash.ToolUseID
	}
	if meta.Status == "" {
		meta.Status = stash.Status
	}
	if meta.ExitCode == nil && stash.ExitCode != nil {
		ec := int(*stash.ExitCode)
		meta.ExitCode = &ec
	}
	if meta.OutputFile == "" {
		meta.OutputFile = stash.OutputFile
	}
	if meta.EndTime == 0 && stash.EndTime != nil {
		meta.EndTime = *stash.EndTime
	}
}

// backgroundTerminalStatus maps the task-lifecycle status to the
// canonical item status enum. task_updated uses completed | failed |
// killed — TaskOutput uses completed with an is_error / exit_code
// signal. `killed` maps to its own statusKilled (user-initiated stop
// via stop_task control_request); every other non-completed status
// collapses to statusErrored so the UI renders a distinct "failed"
// badge.
func backgroundTerminalStatus(meta backgroundTaskTerminalMeta) string {
	// `killed` takes precedence over the generic IsError / ExitCode
	// flags: the parser sets is_error=true for every non-completed
	// terminal (so triage has a uniform "this row did not succeed"
	// marker), but a user-initiated stop is still distinct from a
	// runtime failure and must render as Stopped, not Failed.
	if meta.Status == "killed" {
		return statusKilled
	}
	if meta.IsError {
		return statusErrored
	}
	if meta.ExitCode != nil && *meta.ExitCode != 0 {
		return statusErrored
	}
	switch meta.Status {
	case "completed":
		return statusCompleted
	case "", "failed", "interrupted", "errored":
		if meta.Status == "" {
			return statusCompleted
		}
		return statusErrored
	default:
		// Unknown provider-side status — fall back to errored so a
		// non-completed state never renders as a successful badge.
		return statusErrored
	}
}

// buildBackgroundTerminalSummary produces the sibling row's summary.
// Prefers the launch summary followed by a short outcome marker so the
// UI keeps the "Bash: long-running" context alongside "-> done" /
// "-> exit 1". Falls back to the event's Content (the human-readable
// description / summary Claude emitted) when the launch had none.
func buildBackgroundTerminalSummary(launchSummary, content string, meta backgroundTaskTerminalMeta) string {
	outcome := backgroundTerminalOutcome(meta)
	summary := strings.TrimSpace(launchSummary)
	if summary == "" {
		summary = strings.TrimSpace(content)
	}
	if outcome == "" {
		if summary == "" {
			return "done"
		}
		return summary
	}
	if summary == "" {
		return outcome
	}
	return summary + " -> " + outcome
}

func backgroundTerminalOutcome(meta backgroundTaskTerminalMeta) string {
	switch {
	case meta.ExitCode != nil:
		return fmt.Sprintf("exit %d", *meta.ExitCode)
	case meta.IsError:
		return "error"
	case meta.Status == "failed":
		return "failed"
	case meta.Status == "killed":
		return "killed"
	case meta.Status == "interrupted":
		return "interrupted"
	case meta.Status == "completed":
		return "done"
	default:
		return ""
	}
}

// backgroundTerminalPayload builds the sibling row's tool_call_result
// payload. Returns nil when the event has neither body content nor
// structured meta fields worth persisting — the store row alone
// carries the status + summary so an empty terminal is still
// renderable.
func backgroundTerminalPayload(itemID string, evt provider.ProviderEvent, meta backgroundTaskTerminalMeta, now int64) *store.Payload {
	hasBody := strings.TrimSpace(evt.Content) != ""
	hasMeta := meta.ExitCode != nil || meta.IsError || meta.OutputFile != "" || meta.EndTime != 0
	if !hasBody && !hasMeta {
		return nil
	}

	header := map[string]any{}
	if meta.ExitCode != nil {
		header["exitCode"] = *meta.ExitCode
	}
	if meta.IsError {
		header["isError"] = true
	}
	if meta.Status != "" {
		header["itemStatus"] = meta.Status
	}
	if meta.OutputFile != "" {
		header["outputFile"] = meta.OutputFile
	}
	if meta.EndTime != 0 {
		header["endTime"] = meta.EndTime
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

// backgroundCompletionTurnIndex returns the turn where a background
// completion sibling should be appended. Background work can outlive
// its launching turn by minutes or hours; when it completes during a
// later turn, the terminal row belongs at the current write head so
// chat history shows when the completion actually arrived. If no turn
// is open, fall back to the newest persisted turn row and the newest
// persisted item turn; older installs and sparse tests may have one
// without the other. The max guard keeps a sparse or freshly-started
// thread from placing a completion before the launch row.
func (r *Router) backgroundCompletionTurnIndex(threadID string, launchTurnIndex int) (int, error) {
	if turnIndex, ok := r.openTurnIndex(threadID); ok {
		if turnIndex < launchTurnIndex {
			return launchTurnIndex, nil
		}
		return turnIndex, nil
	}

	turnIndex := launchTurnIndex
	itemTurnIndex, itemErr := r.store.LastTurnIndex(threadID)
	if itemErr == nil && itemTurnIndex > turnIndex {
		turnIndex = itemTurnIndex
	}

	recentTurns, turnErr := r.store.ListRecentTurns(threadID, 1)
	if turnErr == nil && len(recentTurns) > 0 && recentTurns[0].TurnIndex > turnIndex {
		turnIndex = recentTurns[0].TurnIndex
	}

	if itemErr != nil {
		return turnIndex, itemErr
	}
	if turnErr != nil {
		return turnIndex, turnErr
	}
	return turnIndex, nil
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
	r.emit("provider:item_event", NewItemStreamUpsert(item))
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

func validJSONObjectString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parsed map[string]json.RawMessage
	if json.Unmarshal(raw, &parsed) != nil || parsed == nil {
		return ""
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func mergeItemMetaJSON(existing string, incoming json.RawMessage) string {
	if len(incoming) == 0 {
		return existing
	}
	out, ok := mergeJSONObjectBytes(json.RawMessage(existing), incoming)
	if !ok {
		return existing
	}
	return string(out)
}

func mergeJSONObjectBytes(existing, incoming json.RawMessage) (json.RawMessage, bool) {
	merged := map[string]json.RawMessage{}
	if strings.TrimSpace(string(existing)) != "" {
		if json.Unmarshal(existing, &merged) != nil || merged == nil {
			merged = map[string]json.RawMessage{}
		}
	}
	if len(incoming) > 0 {
		var next map[string]json.RawMessage
		if json.Unmarshal(incoming, &next) != nil || next == nil {
			return nil, false
		}
		for key, value := range next {
			merged[key] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	return out, true
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

func completionBaseSummary(launch store.Item, meta toolCompleteMeta, itemType string) string {
	preview := toolInputPreview(meta.Input)
	if preview == "" {
		return launch.Summary
	}
	toolName := stringsx.FirstNonEmptyTrimmed(meta.ToolName, launch.ToolName, itemType, "tool")
	current := strings.TrimSpace(launch.Summary)
	if current == "" || current == strings.TrimSpace(launch.ToolName) || current == strings.TrimSpace(itemType) || !strings.Contains(current, ":") {
		return toolName + ": " + preview
	}
	return launch.Summary
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
	if evt.Content == "" {
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

// taskIDFromItemMeta extracts the `task_id` field from a persisted
// item's meta JSON. Returns "" when the meta is empty, malformed, or
// missing the field. Used by orphan-recovery to key a synthesized
// session_died stash by the same task_id the live wire path would
// have produced.
func taskIDFromItemMeta(metaJSON string) string {
	if strings.TrimSpace(metaJSON) == "" {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal([]byte(metaJSON), &fields) != nil {
		return ""
	}
	if v, ok := fields["task_id"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
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

// mergeItemMetaCorrelationFields merges optional correlation fields
// (task_id, subagent_model) into an existing items.meta JSON blob,
// preserving every other field. Returns the original string unchanged
// when none of the supplied values would change the blob, so callers
// can skip an unnecessary upsert. Empty arguments are treated as "no
// change for that field" — pass "" to leave the existing value alone.
func mergeItemMetaCorrelationFields(existing, taskID, subagentModel string) (string, error) {
	if taskID == "" && subagentModel == "" {
		return existing, nil
	}
	if existing == "" {
		existing = "{}"
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(existing), &parsed); err != nil {
		// Malformed meta — rebuild from scratch rather than silently
		// carry the broken payload forward.
		parsed = map[string]json.RawMessage{}
	}
	changed := false
	if taskID != "" {
		next, ok, err := setStringFieldIfChanged(parsed, "task_id", taskID)
		if err != nil {
			return existing, err
		}
		if ok {
			parsed = next
			changed = true
		}
	}
	if subagentModel != "" {
		next, ok, err := setStringFieldIfChanged(parsed, "subagent_model", subagentModel)
		if err != nil {
			return existing, err
		}
		if ok {
			parsed = next
			changed = true
		}
	}
	if !changed {
		return existing, nil
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return existing, err
	}
	return string(out), nil
}

// setStringFieldIfChanged writes value into parsed[key] when the
// existing value differs (or is absent / non-string). Returns the
// (possibly mutated in-place) map, a `changed` flag, and any
// json.Marshal error. Encapsulates the "skip upsert when nothing
// changed" check so each correlation field stays a one-liner above.
func setStringFieldIfChanged(parsed map[string]json.RawMessage, key, value string) (map[string]json.RawMessage, bool, error) {
	if raw, ok := parsed[key]; ok {
		var current string
		if json.Unmarshal(raw, &current) == nil && current == value {
			return parsed, false, nil
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return parsed, false, err
	}
	parsed[key] = encoded
	return parsed, true, nil
}
