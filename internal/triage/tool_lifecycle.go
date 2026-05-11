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
	itemKindUserText       = "user_text"
	// itemKindAPIRetry is the live-updating retry indicator. One row
	// per turn, deterministic id `retry:<turnIndex>` so re-attempts
	// upsert in place. Status flips from running to completed when a
	// forward-progress event arrives for the thread.
	itemKindAPIRetry = "api_retry"
	// itemKindAPIError is a retry-exhausted assistant API error. The
	// item.Meta carries the SDK error enum so the frontend can branch
	// on `rate_limit` / `authentication_failed` / ... and render the
	// matching actionable copy.
	itemKindAPIError = "api_error"

	payloadKindToolCallResult = "tool_call_result"

	statusRunning   = "running"
	statusStreaming = "streaming"
	statusCompleted = "completed"
	statusErrored   = "errored"
	// statusKilled is a distinct terminal state for provider-reported
	// stopped/killed tasks. It stays separate from statusErrored so the
	// UI can render a gray "Stopped" badge rather than the red "Failed"
	// bucket.
	statusKilled = "killed"
)

type toolStartMeta struct {
	ToolName        string          `json:"toolName"`
	Input           json.RawMessage `json:"input"`
	MetaUpdateOnly  bool            `json:"meta_update_only"`
	IsBackground    bool            `json:"is_background"`
	TaskID          string          `json:"task_id"`
	SubagentModel   string          `json:"subagent_model"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
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

	// A "meta update" EventToolStart targets an already-persisted
	// tool_call row and merges metadata that arrived on a later signal.
	// Known emit sites today:
	//   - Claude's `system/task_started` attaches the task_id ↔
	//     tool_use_id mapping so reconnect recovery can correlate
	//     later task_updated / task_notification events via
	//     items.meta.task_id.
	//   - Subagent assistant envelopes attach `subagent_model` to the
	//     parent Task/Agent tool_call so the UI can render
	//     `<agent_type> (<Model>)` in the card header without
	//     plumbing a separate event kind. The model is taken from
	//     `message.model` on the first subagent assistant envelope.
	//   - Codex child-thread metadata reads enrich spawn_agent rows with
	//     receiver labels once `thread/read` exposes agentNickname /
	//     agentRole for the child thread.
	// These updates preserve the existing status and payload. If the
	// update carries toolName/input, the persisted summary/meta are also
	// refreshed so the UI can render the new metadata immediately.
	metaUpdateOnly := meta.MetaUpdateOnly || strings.TrimSpace(meta.ToolName) == "" &&
		len(meta.Input) == 0 &&
		(meta.TaskID != "" || meta.SubagentModel != "" || meta.ParentToolUseID != "")

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
		if meta.ToolName != "" {
			existing.ToolName = meta.ToolName
		}
		if summary := buildToolCallSummary(meta, evt.ItemType); meta.MetaUpdateOnly && strings.TrimSpace(summary) != "" {
			existing.Summary = summary
		}
		parentToolUseID := stringsx.FirstNonEmptyTrimmed(eventParentID(evt), meta.ParentToolUseID)
		baseMeta := mergeItemMetaJSON(existing.Meta, evt.Meta)
		mergedMeta, err := mergeItemMetaCorrelationFields(baseMeta, itemMetaCorrelationFields{
			TaskID:          meta.TaskID,
			SubagentModel:   meta.SubagentModel,
			ParentToolUseID: parentToolUseID,
		})
		if err != nil {
			log.Printf("triage: merge correlation fields into item meta %s: %v", itemID, err)
			return nil
		}
		parentChanged := parentToolUseID != "" && existing.ParentID == ""
		if mergedMeta == existing.Meta && !parentChanged {
			return nil
		}
		existing.Meta = mergedMeta
		if parentChanged {
			existing.ParentID = parentToolUseID
		}
		existing.UpdatedAt = now
		// Re-shape after the merge: a metaUpdateOnly never carries
		// heavy input bytes today, but trimming is idempotent and the
		// existing.InputPayloadID is preserved by shapeToolItemMeta so
		// the launch's payload stays canonical.
		inputPayload := r.shapeToolItemMeta(&existing, now)
		return r.persistItemWithInputPayload(existing, nil, inputPayload)
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

	// Promote heavy tool inputs (Edit/Write/MultiEdit/NotebookEdit
	// content) out of items.meta into a sibling tool_call_input
	// payload so the persisted row + the live emit stay small. On a
	// re-discovered launch (item.InputPayloadID already set),
	// shapeToolItemMeta drops the freshly-extracted payload; the
	// original launch's payload is canonical.
	inputPayload := r.shapeToolItemMeta(&item, now)
	return r.persistItemWithInputPayload(item, nil, inputPayload)
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
		r.settleStreamingBeforeTimelineBoundary(evt, "completion-only tool")
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
	if codexThread && isCodexSpawnAgentLaunch(launch, evt.Meta) {
		launch.Status = completionStatus(meta)
		launch.Summary = buildCompletionSummary(completionBaseSummary(launch, meta, evt.ItemType), meta)
		if meta.IsBackground {
			launch.IsBackground = true
		}
		launch.Meta = mergeItemMetaJSON(launch.Meta, evt.Meta)
		launch.UpdatedAt = now
		launchInputPayload := r.shapeToolItemMeta(&launch, now)
		return r.persistItemWithInputPayload(launch, nil, launchInputPayload)
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
	// Re-shape the merged meta so a completion event whose meta still
	// carries heavy input bytes (Codex curated input) doesn't re-bloat
	// the row. shapeToolItemMeta returns nil when launch.InputPayloadID
	// is already set; for tools registered with PromoteToPayload whose
	// launch row never carried an input payload, this is the recovery
	// path that promotes the merged input bytes into a sibling payload.
	inputPayload := r.shapeToolItemMeta(&launch, now)

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

	payload := completionPayloadForLaunch(launch, evt, meta, now)
	switch {
	case payload == nil:
		return r.persistItemWithInputPayload(launch, nil, inputPayload)
	case launch.PayloadID == "":
		return r.persistItemWithInputPayload(launch, payload, inputPayload)
	case launch.PayloadKind == payloadKindToolCallResult:
		payload.ID = launch.PayloadID
		return r.persistItemWithInputPayload(launch, payload, inputPayload)
	default:
		return r.persistItemWithInputPayload(launch, nil, inputPayload)
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
	payload := completionPayloadForTool(item.ID, toolName, commandFromInput(meta.Input), evt, meta, now)
	inputPayload := r.shapeToolItemMeta(&item, now)
	return r.persistItemWithInputPayload(item, payload, inputPayload)
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
	case "collab_agent", "send_input", "close_agent":
		return true
	default:
		return false
	}
}

func isCodexSpawnAgentLaunch(launch store.Item, completionMeta json.RawMessage) bool {
	if launch.ToolName != "collab_agent" {
		return false
	}
	launchMeta := decodeCodexItemMeta(json.RawMessage(launch.Meta))
	if launchMeta.Tool == "spawn_agent" {
		return true
	}
	return decodeCodexItemMeta(completionMeta).Tool == "spawn_agent"
}

func (r *Router) persistSplitToolCompletion(launch store.Item, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) error {
	if launch.ToolName == "wait_agent" {
		evt.Meta = preserveCodexWaitLaunchReceiverTargets(launch.Meta, evt.Meta)
	}
	launch.Status = statusCompleted
	launch.UpdatedAt = now
	launch.Meta = mergeItemMetaJSON(launch.Meta, evt.Meta)
	// Re-shape after the merge for the same reason as
	// persistToolCallCompletion: a registered tool whose launch row
	// never carried an input payload may still have heavy bytes after
	// the merge, and shapeToolItemMeta promotes them.
	launchInputPayload := r.shapeToolItemMeta(&launch, now)
	if err := r.persistItemWithInputPayload(launch, nil, launchInputPayload); err != nil {
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
	payload := completionPayloadForLaunch(completion, evt, meta, now)
	inputPayload := r.shapeToolItemMeta(&completion, now)
	return r.persistItemWithInputPayload(completion, payload, inputPayload)
}

func preserveCodexWaitLaunchReceiverTargets(launchMeta string, completeMeta json.RawMessage) json.RawMessage {
	launchReceiverIDs := receiverThreadIDsFromItemMeta(json.RawMessage(launchMeta))
	if len(launchReceiverIDs) == 0 {
		return completeMeta
	}
	var decoded map[string]json.RawMessage
	if len(completeMeta) == 0 || json.Unmarshal(completeMeta, &decoded) != nil || decoded == nil {
		decoded = map[string]json.RawMessage{}
	}
	var input map[string]json.RawMessage
	if raw, ok := decoded["input"]; ok {
		_ = json.Unmarshal(raw, &input)
	}
	if input == nil {
		input = map[string]json.RawMessage{}
	}
	encodedReceivers, err := json.Marshal(launchReceiverIDs)
	if err != nil {
		return completeMeta
	}
	input["receiverThreadIds"] = encodedReceivers
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return completeMeta
	}
	decoded["input"] = encodedInput
	encodedMeta, err := json.Marshal(decoded)
	if err != nil {
		return completeMeta
	}
	return encodedMeta
}

func receiverThreadIDsFromItemMeta(meta json.RawMessage) []string {
	var decoded struct {
		Input struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
		} `json:"input"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &decoded) != nil {
		return nil
	}
	return decoded.Input.ReceiverThreadIDs
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
	TaskID          string `json:"task_id"`
	ToolUseID       string `json:"tool_use_id,omitempty"`
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	Status          string `json:"status"`
	Source          string `json:"source,omitempty"`
	IsError         bool   `json:"is_error,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	OutputFile      string `json:"output_file,omitempty"`
	EndTime         int64  `json:"end_time,omitempty"`
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
//     stashBackgroundTaskTerminal). The launch row stays visible in
//     the tray; the stash drives `ListPendingBackgroundCompletionsAsItems`
//     which synthesizes a tray-only `tool_completion` companion so the
//     tray can render "completed" immediately. Reconnect-replay is
//     idempotent (PK is thread_id+task_id). The chat-side
//     `tool_completion` sibling is NOT written here — it comes later
//     via task_notification or TaskOutput drain.
//
//   - source="task_updated", status="killed": Claude reports that the
//     provider killed/stopped the background process. For visible
//     launches this includes user stops (StopClaudeTask → stop_task
//     control_request → CLI replies task_updated{killed}), and should
//     render immediately without waiting for a later task_notification.
//     If the launch is not visible in this parent thread, the signal
//     is dropped by writeBackgroundCompletionSibling rather than
//     creating a standalone orphan row.
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
// tool_call row that is still `running` and has no completion sibling.
// These are launches whose owning provider session died with the
// previous app instance — the agent will never observe completion (the
// session is gone), so we synthesise the observation here by writing
// the chat-side `tool_completion` sibling directly.
//
// If a `pending_background_task_terminals` stash row exists for the
// launch, we drain it and merge its data (status/exit_code/output_file)
// into the synthesized meta so the recovered sibling reflects the real
// outcome the host captured. When there's no stash we fall back to
// status="killed" — the host never reported an exit, so "we killed it
// at app shutdown" is the closest truthful state. The sibling carries
// `source="session_died"` in both cases so the frontend can render the
// distinct provenance.
//
// Called once during App.ServiceStartup, after the store is open and
// before any provider session can spawn. Idempotent: running this twice
// is a no-op because the second pass sees the existing sibling and
// skips the launch via the NOT EXISTS predicate in
// ListOrphanedBackgroundLaunches. Crash-safe: if the process dies
// mid-loop before a sibling is written, the launch row is still
// `running` with no sibling. The stash drain is atomic, so a crash
// after drain but before sibling write leaves the launch as a stashless
// orphan that the next boot's sweep recovers with status="killed".
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
			Source:    "session_died",
		}
		stash, stashFound, err := r.store.TakePendingBackgroundTerminal(launch.ThreadID, taskID)
		if err != nil {
			log.Printf("triage: drain orphan stash %s/%s: %v", launch.ThreadID, taskID, err)
		}
		if stashFound {
			mergeStashIntoTerminalMeta(&meta, stash)
		} else {
			// No host-reported outcome — the launch was running when the
			// app died. "killed" is the closest truthful state.
			meta.Status = "killed"
		}
		if err := r.writeBackgroundCompletionSibling(syntheticEvt, meta, stashFound); err != nil {
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
// the items.meta.task_id index. A launch is required: Claude can emit
// task lifecycle signals for background work owned by a subagent whose
// private transcript was never projected into the parent thread. Those
// signals are real, but they are not parent-level tool rows.
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
	if !launchFound {
		return nil
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
	launchTurnIndex := launch.TurnIndex
	parentID := stringsx.FirstNonEmptyTrimmed(launch.ParentID, eventParentID(evt), meta.ParentToolUseID)
	toolName := launch.ToolName
	launchSummary := launch.Summary
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
		CompletionOf: launch.ID,
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
	if completion.PayloadID == "" && meta.OutputFile != "" {
		payload = r.backgroundOutputFilePayload(launch, meta.OutputFile, meta.ExitCode, now)
	}
	if payload == nil && completion.PayloadID == "" {
		payload = backgroundTerminalPayload(launch, evt, meta, now)
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
// signal. `killed` maps to its own statusKilled for provider-reported
// stopped/killed tasks; every other non-completed status collapses to
// statusErrored so the UI renders a distinct "failed" badge.
func backgroundTerminalStatus(meta backgroundTaskTerminalMeta) string {
	// `killed` takes precedence over the generic IsError / ExitCode
	// flags: the parser sets is_error=true for every non-completed
	// terminal (so triage has a uniform "this row did not succeed"
	// marker), but a provider stop/kill is still distinct from a
	// runtime failure and must render as Stopped, not Failed.
	//
	// `stopped` is the SDK-normalized form of `killed` — `print.ts`
	// upstream maps the XML-form `killed` to `stopped` for SDK
	// consumers (claude-code-source-code/src/cli/print.ts:2042-2047).
	// In normal flow `task_updated{killed}` is routed through observe
	// with the raw `killed`, and task_notification's enrich path
	// doesn't recompute Status — so this case is only reached on a
	// notification-only sequence (no preceding task_updated). The
	// defensive mapping renders Stopped instead of collapsing to the
	// errored `default` branch.
	if meta.Status == "killed" || meta.Status == "stopped" {
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

// backgroundTerminalPayload builds the sibling row's payload. Command
// launches get command_output so Bash/exec rows share the same UI;
// non-command background tasks keep the generic tool_call_result body.
// Returns nil when the event has neither body content nor structured
// meta fields worth persisting — the store row alone carries the
// status + summary so an empty terminal is still renderable.
func backgroundTerminalPayload(launch store.Item, evt provider.ProviderEvent, meta backgroundTaskTerminalMeta, now int64) *store.Payload {
	hasBody := strings.TrimSpace(evt.Content) != ""
	hasMeta := meta.ExitCode != nil || meta.IsError || meta.OutputFile != "" || meta.EndTime != 0
	if !hasBody && !hasMeta {
		return nil
	}
	itemID := launch.ID
	if isCommandOutputLaunch(launch) {
		code := 0
		if meta.ExitCode != nil {
			code = *meta.ExitCode
		}
		commandMeta := ExtractCommandOutputMetaWithError(evt.Content, commandFromLaunch(launch), code, "")
		if commandMeta.ErrorMessage == "" && meta.IsError {
			commandMeta.ErrorMessage = compactCommandErrorMessage(evt.Content)
		}
		commandMetaJSON, err := json.Marshal(commandMeta)
		if err != nil {
			commandMetaJSON = []byte("{}")
		}
		return &store.Payload{
			ID:        "command-output:" + itemID,
			Kind:      "command_output",
			Meta:      string(commandMetaJSON),
			Data:      []byte(evt.Content),
			CreatedAt: now,
		}
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
	cumulative := ExtractCommandOutputMetaWithError(string(data), prior.Command, prior.ExitCode, prior.ErrorMessage)
	cumulative.OutputState = prior.OutputState
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

func isToolStartMetaUpdateOnly(raw json.RawMessage) bool {
	return decodeToolStartMeta(raw).MetaUpdateOnly
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
			if existingValue, ok := merged[key]; ok {
				if nested, nestedOK := mergeJSONObjectBytes(existingValue, value); nestedOK {
					merged[key] = nested
					continue
				}
			}
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
	case meta.ItemStatus == "errored":
		return "(errored)"
	case meta.ItemStatus == "killed":
		return "(killed)"
	case meta.ItemStatus == "declined":
		return "(declined)"
	default:
		return ""
	}
}

func completionStatus(meta toolCompleteMeta) string {
	if meta.IsError || meta.ItemStatus == "failed" || meta.ItemStatus == "errored" || meta.ItemStatus == "killed" {
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

func commandFromInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil {
		return ""
	}
	if raw, ok := obj["command"]; ok {
		var command string
		if json.Unmarshal(raw, &command) == nil {
			return strings.TrimSpace(command)
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

func completionPayloadForLaunch(launch store.Item, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) *store.Payload {
	return completionPayloadForTool(launch.ID, launch.ToolName, commandFromLaunch(launch), evt, meta, now)
}

func completionPayloadForTool(itemID string, toolName string, command string, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) *store.Payload {
	if isCommandOutputToolName(toolName) {
		return commandCompletionPayload(itemID, command, evt, meta, now)
	}
	return completionPayload(itemID, evt, meta, now)
}

func commandCompletionPayload(itemID string, command string, evt provider.ProviderEvent, meta toolCompleteMeta, now int64) *store.Payload {
	if evt.Content == "" {
		return nil
	}
	header := map[string]any{}
	if strings.TrimSpace(command) != "" {
		header["command"] = strings.TrimSpace(command)
	}
	if meta.ExitCode != nil {
		header["exitCode"] = *meta.ExitCode
		header["exit_code"] = *meta.ExitCode
	}
	if meta.IsError {
		header["is_error"] = true
	}
	if meta.ItemStatus != "" {
		header["itemStatus"] = meta.ItemStatus
	}
	if len(evt.Meta) > 0 {
		if merged, ok := mergeJSONObjectBytes(marshalJSONObjectOrEmpty(header), evt.Meta); ok {
			headerJSON := buildPayloadMeta("command_output", provider.ProviderEvent{
				Content: evt.Content,
				Meta:    merged,
			})
			return &store.Payload{
				ID:        "command-output:" + itemID,
				Kind:      "command_output",
				Meta:      headerJSON,
				Data:      []byte(evt.Content),
				CreatedAt: now,
			}
		}
	}
	headerJSONBytes, err := json.Marshal(header)
	if err != nil {
		headerJSONBytes = []byte("{}")
	}
	return &store.Payload{
		ID:   "command-output:" + itemID,
		Kind: "command_output",
		Meta: buildPayloadMeta("command_output", provider.ProviderEvent{
			Content: evt.Content,
			Meta:    headerJSONBytes,
		}),
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
}

func marshalJSONObjectOrEmpty(fields map[string]any) json.RawMessage {
	data, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
}

func isCommandOutputToolName(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "Bash", "command_execution", "commandExecution", "exec_command":
		return true
	default:
		return false
	}
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
// itemMetaCorrelationFields is the meta-only correlation payload triage
// merges into an existing tool_call row. Empty fields mean "leave the
// existing value alone".
type itemMetaCorrelationFields struct {
	TaskID          string
	SubagentModel   string
	ParentToolUseID string
}

// mergeItemMetaCorrelationFields merges optional correlation fields into
// an existing items.meta JSON blob, preserving every other field. Returns
// the original string unchanged when none of the supplied values would
// change the blob, so callers can skip an unnecessary upsert.
func mergeItemMetaCorrelationFields(existing string, fields itemMetaCorrelationFields) (string, error) {
	if fields.TaskID == "" && fields.SubagentModel == "" && fields.ParentToolUseID == "" {
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
	if fields.TaskID != "" {
		next, ok, err := setStringFieldIfChanged(parsed, "task_id", fields.TaskID)
		if err != nil {
			return existing, err
		}
		if ok {
			parsed = next
			changed = true
		}
	}
	if fields.SubagentModel != "" {
		next, ok, err := setStringFieldIfChanged(parsed, "subagent_model", fields.SubagentModel)
		if err != nil {
			return existing, err
		}
		if ok {
			parsed = next
			changed = true
		}
	}
	if fields.ParentToolUseID != "" {
		next, ok, err := setStringFieldIfChanged(parsed, "parent_tool_use_id", fields.ParentToolUseID)
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
