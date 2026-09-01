package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
)

const (
	itemKindAssistantText = "assistant_text"
	itemKindThinking      = "thinking"
	// itemKindCompactionReasoning is the live "compact" tail: the claudetui
	// compaction summarizer's reasoning, streamed (and settled) as its own
	// top-level row just above the `compaction` divider. It rides the same
	// streaming machinery as thinking (active-block maps, tail-bounded persist,
	// settle) but renders with its own icon/label, and is created only for
	// EventThinking carrying provider.CompactionReasoningScope. See
	// handleCompactionReasoning.
	itemKindCompactionReasoning = "compaction_reasoning"
	itemKindToolCall            = "tool_call"
	itemKindBackgroundDone      = "tool_completion"
	itemKindNotification        = "notification"
	itemKindUserText            = "user_text"
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
	// ItemKindError is the generic provider / dispatch failure row. It is
	// the one kind the sidebar's Failed pill matches, which is why an
	// emitted row of this kind also fires thread:error_notice
	// (Router.emitErrorNotice) — api_error deliberately does not. Exported
	// because internal/app persists three failure rows of this kind (send,
	// steer, flush dispatch) and the notice keys on the exact string.
	ItemKindError = "error"

	payloadKindToolCallResult = "tool_call_result"
	payloadKindCommandOutput  = "command_output"

	statusRunning   = "running"
	statusStreaming = "streaming"
	statusCompleted = "completed"
	statusErrored   = "errored"
	// statusKilled is a distinct terminal state for provider-reported
	// stopped/killed tasks. It stays separate from statusErrored so the
	// UI can render a gray "Stopped" badge rather than the red "Failed"
	// bucket.
	statusKilled = "killed"
	// statusDeclined is a refusal, terminal and distinct from statusErrored:
	// nothing failed, the tool was not allowed to run. The same word is the
	// `items.decision` vocabulary for the same outcome (see decisionDeclined),
	// which is why the two are pinned to one another rather than each spelled
	// out at every site.
	statusDeclined = "declined"
)

// isMetaUpdateOnly reports whether this EventToolStart only annotates an
// already-persisted tool_call row (merging correlation metadata that
// arrived on a later signal) rather than launching a new tool. Two forms:
//   - an explicit meta_update_only flag (Codex spawn-agent receiver labels);
//   - the implicit shape — no toolName, no input, carrying one of the
//     correlation keys: task_id and/or parent_tool_use_id from Claude's
//     system/task_started (they ride together), or subagent_model from a
//     subagent assistant envelope.
//
// The handleToolStart gate and persistToolCallLaunch MUST agree on this. If
// the gate under-detects, the event runs settleStreamingBeforeTimelineBoundary
// and wrongly settles whatever streaming text is live — for the subagent
// model-stamp (ItemID = parent Agent tool_use_id, no ParentToolUseID, so it
// settles the MAIN scope) that fragments the main message mid-stream whenever
// a backgrounded subagent reports in. If persist under-detects it fabricates a
// duplicate launch row. One predicate keeps the two from drifting.
func (m ToolStartMeta) isMetaUpdateOnly() bool {
	if m.MetaUpdateOnly {
		return true
	}
	return strings.TrimSpace(m.ToolName) == "" &&
		len(m.Input) == 0 &&
		(m.TaskID != "" || m.SubagentModel != "" || m.ParentToolUseID != "")
}

func (r *Router) persistToolCallLaunch(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	if itemID == "" {
		return nil
	}

	meta := DecodeToolStartMeta(evt.Meta)
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
	metaUpdateOnly := meta.isMetaUpdateOnly()

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
		if summary := BuildToolCallSummary(meta, evt.ItemType); meta.MetaUpdateOnly && strings.TrimSpace(summary) != "" {
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
	summary := BuildToolCallSummary(meta, evt.ItemType)

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
		Meta:         StoredToolCallMeta(evt.ItemType, toolName, evt.Meta),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if found {
		item = existing
		item.Summary = summary
		item.ParentID = stringsx.FirstNonEmptyTrimmed(eventParentID(evt), existing.ParentID)
		item.ToolName = toolName
		item.IsBackground = existing.IsBackground || meta.IsBackground
		item.Meta = MergeStoredToolCallMeta(existing.Meta, evt.ItemType, toolName, evt.Meta)
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

	// AskUserQuestion / request_user_input rows render question + option
	// text through ChatMarkdown, so validate any path-shaped tokens in
	// the question body and option labels/descriptions/previews against
	// the workspace. Validated allowlist lands on item.Meta.pathRefs.
	// `meta.input.questions[]` is the same shape on both Claude
	// (AskUserQuestion) and Codex (request_user_input).
	if texts := userInputValidationTexts(toolName, item.Meta); len(texts) > 0 {
		r.enrichPathRefsFromTexts(evt.ThreadID, &item, texts...)
	}

	// Promote heavy tool inputs (Edit/Write/MultiEdit/NotebookEdit
	// content) out of items.meta into a sibling tool_call_input
	// payload so the persisted row + the live emit stay small. On a
	// re-discovered launch (item.InputPayloadID already set),
	// shapeToolItemMeta drops the freshly-extracted payload; the
	// original launch's payload is canonical.
	inputPayload := r.shapeToolItemMeta(&item, now)
	if err := r.persistItemWithInputPayload(item, nil, inputPayload); err != nil {
		return err
	}
	return r.persistProvisionalSubagentPrompt(item, meta, now)
}

// userInputValidationTexts returns the set of human-readable text
// sources from an AskUserQuestion / request_user_input meta that the
// frontend will render through ChatMarkdown. `q.question` and
// `option.preview` are the only two fields that flow through
// ChatMarkdown today (see AskUserQuestionCard.svelte); `option.label`
// and `option.description` render as plain `<p>` so they have no
// linkifier to feed. Returning extra strings here would pay regex +
// stat cost for tokens whose match would never be displayed as a link.
//
// Returns empty when the tool isn't a user-input prompt or the meta
// is missing the questions array. A malformed JSON payload is logged
// (matching the policy in mergePathRefsIntoMeta) and skipped — the
// row still persists, just without an allowlist.
func userInputValidationTexts(toolName, metaJSON string) []string {
	switch strings.TrimSpace(toolName) {
	case "AskUserQuestion", "request_user_input":
	default:
		return nil
	}
	if strings.TrimSpace(metaJSON) == "" {
		return nil
	}
	var top struct {
		Input struct {
			Questions []provider.UserInputQuestion `json:"questions"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &top); err != nil {
		log.Printf("triage: pathlinks user-input meta unmarshal: %v", err)
		return nil
	}
	if len(top.Input.Questions) == 0 {
		return nil
	}
	texts := make([]string, 0, len(top.Input.Questions)*2)
	for _, question := range top.Input.Questions {
		if q := strings.TrimSpace(question.Question); q != "" {
			texts = append(texts, q)
		}
		for _, option := range question.Options {
			if preview := strings.TrimSpace(option.Preview); preview != "" {
				texts = append(texts, preview)
			}
		}
	}
	return texts
}

func (r *Router) persistToolCallCompletion(evt provider.ProviderEvent) error {
	itemID := eventItemID(evt)
	meta := DecodeToolCompleteMeta(evt.Meta)

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
		r.settleStreamingBeforeTimelineBoundary(evt, "completion-only tool", settleAllScopesIfUnscoped)
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
	if codexThread && ShouldSplitCodexToolCompletion(launch.ToolName) {
		return r.persistSplitToolCompletion(launch, evt, meta, now)
	}
	if codexThread && isCodexSpawnAgentLaunch(launch, evt.Meta) {
		launch.Status = CompletionStatus(meta)
		launch.Summary = BuildCompletionSummary(CompletionBaseSummary(launch, meta, evt.ItemType), meta)
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
			launch.Summary = r.resumeCarrierSummary(evt.ThreadID, launch.Summary, launch.Meta)
			if meta.WatchTask {
				// Selective one-key merge — the full completion meta
				// (tool_result echo, tool_use_result) must NOT bloat the
				// launch row; only the watch marker matters downstream
				// (HasQueueBlockingBackgroundToolCall filters on it).
				launch.Meta = mergeItemMetaJSON(launch.Meta, []byte(`{"watch_task":true}`))
			}
			return r.persistItem(launch, nil)
		}
		// Launch row already background-flagged. Usually the placeholder
		// completion carries no additional state — but the watch marker
		// can still be new: §E7's real Monitor launch carries no
		// run_in_background (the ack is what backgrounds it), and a
		// future CLI that marks the launch up front would land in THIS
		// arm and silently lose watch-ness, which the flush-queue drain
		// reads (HasQueueBlockingBackgroundToolCall). Merge it whenever
		// the completion asserts it and the row lacks it, whichever arm
		// the flag arrived through.
		if meta.WatchTask && !launchIsWatchTask(launch) {
			launch.UpdatedAt = now
			launch.Meta = mergeItemMetaJSON(launch.Meta, []byte(`{"watch_task":true}`))
			return r.persistItem(launch, nil)
		}
		// The background task terminal (task_updated / TaskOutput) will
		// write the sibling completion row when it arrives.
		return nil
	}

	status := CompletionStatus(meta)
	launch.Status = status
	launch.Summary = BuildCompletionSummary(CompletionBaseSummary(launch, meta, evt.ItemType), meta)
	if strings.TrimSpace(meta.ToolName) != "" {
		launch.ToolName = strings.TrimSpace(meta.ToolName)
	}
	launch.Meta = MergeStoredToolCallMeta(launch.Meta, evt.ItemType, launch.ToolName, evt.Meta)
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
		if err := r.rebuildCommandOutputMeta(launch.ThreadID, launch.PayloadID); err != nil {
			// Meta jitter is a presentation concern; do not fail the
			// turn because we couldn't fix it. Log and continue so
			// the tool lifecycle completes cleanly.
			log.Printf("triage: rebuild command_output meta %s: %v", launch.PayloadID, err)
		}
	}

	// Advisor result body renders through ChatMarkdown when the row is
	// expanded. Validate path-shaped tokens in evt.Content against the
	// workspace so the linkifier has an allowlist for the advisor body.
	// The validator runs on the completion event's content (the full
	// result text); advisor runs inline (no streaming) so this is the
	// only persist site that sees the body.
	if strings.EqualFold(launch.ToolName, "advisor") && evt.Content != "" {
		r.enrichPathRefsFromTexts(evt.ThreadID, &launch, evt.Content)
	}

	payload := completionPayloadForLaunch(launch, evt, meta, now)
	var persistErr error
	switch {
	case payload == nil:
		persistErr = r.persistItemWithInputPayload(launch, nil, inputPayload)
	case launch.PayloadID == "":
		persistErr = r.persistItemWithInputPayload(launch, payload, inputPayload)
	case launch.PayloadKind == payloadKindToolCallResult:
		payload.ID = launch.PayloadID
		persistErr = r.persistItemWithInputPayload(launch, payload, inputPayload)
	default:
		persistErr = r.persistItemWithInputPayload(launch, nil, inputPayload)
	}
	if persistErr != nil {
		return persistErr
	}
	// This row just went terminal. An AWAITED agent launch settles HERE
	// and nowhere else — no completion sibling, no child terminal — so
	// this is the one moment its live counters can become durable.
	return r.persistFinalSubagentProgressIfLaunch(launch)
}

// persistFinalSubagentProgress folds a settling LAUNCH row's last live
// progress tick onto the row (docs/specs/agent-visibility.md: ticks are
// live UI state, the final numbers persist). Skipped outright when no
// tick was ever seen for the id, which is what keeps an ordinary tool
// result and a second drain of the same background terminal from paying
// a write to restate numbers nobody changed.
//
// The AUTHORITATIVE-usage terminal (Claude's task_notification) does not
// come through here: it carries counters of its own even when no tick
// was seen, so it calls persistSubagentFinalProgress directly. The
// helper is order-free, so either arriving first lands the same row.
//
// Callers are the terminals that settle a launch and already KNOW their
// row is one: the background completion sibling and the Codex child
// terminal. The inline tool completion, which settles every ordinary
// tool too, goes through persistFinalSubagentProgressIfLaunch instead.
func (r *Router) persistFinalSubagentProgress(launch store.Item) error {
	if r == nil {
		return nil
	}
	if _, live := r.PeekSubagentProgress(launch.ThreadID, launch.ID); !live {
		return nil
	}
	return r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{})
}

// persistFinalSubagentProgressIfLaunch is persistFinalSubagentProgress
// plus the structural launch probe, for the one terminal that also
// settles ordinary tools. `Store.IsSubagentLaunch` is the provider-
// neutral predicate the store already anchors subagent cards on (a
// tool_call that other rows are attributed to) rather than a list of
// launch tool names this package would have to keep in sync with two
// providers. It runs only after a live tick has been found, so a plain
// Read/Edit result never reaches the store for it.
func (r *Router) persistFinalSubagentProgressIfLaunch(launch store.Item) error {
	if r == nil || r.store == nil {
		return nil
	}
	if _, live := r.PeekSubagentProgress(launch.ThreadID, launch.ID); !live {
		return nil
	}
	isLaunch, err := r.store.IsSubagentLaunch(launch.ThreadID, launch.ID)
	if err != nil {
		return fmt.Errorf("triage: subagent launch probe %s: %w", launch.ID, err)
	}
	if !isLaunch {
		// A tick named a row nothing is attributed to. Drop the live
		// entry rather than stamp agent counters onto a plain tool
		// card — and rather than leave it to be swept at thread
		// cleanup, where it would still be reported as live progress.
		r.TakeSubagentProgress(launch.ThreadID, launch.ID)
		return nil
	}
	return r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{})
}

func (r *Router) persistToolCallCompletedWithoutLaunch(evt provider.ProviderEvent, meta ToolCompleteMeta) error {
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
		Status:    CompletionStatus(meta),
		Summary:   BuildCompletionSummary(BuildToolCallSummary(ToolStartMeta{ToolName: toolName, Input: meta.Input}, evt.ItemType), meta),
		ParentID:  eventParentID(evt),
		ToolName:  toolName,
		Meta:      StoredToolCallMeta(evt.ItemType, toolName, evt.Meta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload := CompletionPayloadForTool(item.ID, toolName, commandFromInput(meta.Input), evt, meta, now)
	inputPayload := r.shapeToolItemMeta(&item, now)
	return r.persistItemWithInputPayload(item, payload, inputPayload)
}

// shouldPersistCodexCompletionWithoutLaunch names the Codex tools whose
// item/completed is the WHOLE lifecycle — no item/started row precedes them —
// and which therefore have to mint their own top-level row.
//
// MultiAgentV2's `send_input` item is completion-only too. It remains an
// independent chronological activity row: a message sent now must never mutate
// the historical spawn event that established the child.
func shouldPersistCodexCompletionWithoutLaunch(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "collab_agent", "close_agent", "send_input":
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

func (r *Router) persistSplitToolCompletion(launch store.Item, evt provider.ProviderEvent, meta ToolCompleteMeta, now int64) error {
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
	completionID := ToolCompletionID(launch.ID)
	completion := store.Item{
		ID:           completionID,
		ThreadID:     evt.ThreadID,
		TurnIndex:    launch.TurnIndex,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       CompletionStatus(meta),
		Summary:      BuildCompletionSummary(CompletionBaseSummary(launch, meta, evt.ItemType), meta),
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
	input["requestedReceiverThreadIds"] = encodedReceivers
	if receiverAgents := receiverAgentsFromItemMeta(json.RawMessage(launchMeta)); len(receiverAgents) > 0 {
		if encodedAgents, err := json.Marshal(receiverAgents); err == nil {
			input["requestedReceiverAgents"] = encodedAgents
		}
	}
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

// snapshotCodexWaitStartReceivers fills the one gap left by Codex V2's wait
// events: some wait starts omit their target list even though the parent is
// visibly waiting on active children. Explicit wire/raw targets always win.
// The persisted live-launch projection is ordered, reconnect-safe, and already
// reflects child lifecycle updates, so it is the stable source for this display
// snapshot. Completion keeps the snapshot through
// preserveCodexWaitLaunchReceiverTargets above.
func (r *Router) snapshotCodexWaitStartReceivers(evt provider.ProviderEvent) (provider.ProviderEvent, error) {
	if evt.ItemType != "wait_agent" || waitMetaHasReceiverTargets(evt.Meta) {
		return evt, nil
	}
	launches, err := r.store.ListLiveCodexSubagentLaunches(evt.ThreadID)
	if err != nil {
		return evt, fmt.Errorf("snapshot Codex wait receivers: %w", err)
	}
	parentID := eventParentID(evt)
	seen := make(map[string]struct{})
	receiverThreadIDs := make([]string, 0, len(launches))
	receiverAgents := make([]codexWaitReceiverAgent, 0, len(launches))
	for _, launch := range launches {
		if strings.TrimSpace(launch.ParentID) != parentID {
			continue
		}
		terminalStatuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.Meta))
		agentsByThreadID := receiverAgentsByThreadID(json.RawMessage(launch.Meta))
		for _, receiverThreadID := range receiverThreadIDsFromItemMeta(json.RawMessage(launch.Meta)) {
			receiverThreadID = strings.TrimSpace(receiverThreadID)
			if receiverThreadID == "" || strings.TrimSpace(terminalStatuses[receiverThreadID]) != "" {
				continue
			}
			if _, duplicate := seen[receiverThreadID]; duplicate {
				continue
			}
			seen[receiverThreadID] = struct{}{}
			receiverThreadIDs = append(receiverThreadIDs, receiverThreadID)
			if agent, ok := agentsByThreadID[receiverThreadID]; ok {
				receiverAgents = append(receiverAgents, agent)
			}
		}
	}
	if len(receiverThreadIDs) == 0 {
		return evt, nil
	}
	input := map[string]any{"receiverThreadIds": receiverThreadIDs}
	if len(receiverAgents) > 0 {
		input["receiverAgents"] = receiverAgents
	}
	extra, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		return evt, fmt.Errorf("encode Codex wait receiver snapshot: %w", err)
	}
	merged, ok := mergeJSONObjectBytes(evt.Meta, extra)
	if !ok {
		return evt, nil
	}
	evt.Meta = merged
	return evt, nil
}

func waitMetaHasReceiverTargets(meta json.RawMessage) bool {
	var decoded struct {
		Input struct {
			ReceiverThreadIDs          []string `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string `json:"requestedReceiverThreadIds"`
		} `json:"input"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &decoded) != nil {
		return false
	}
	return len(decoded.Input.ReceiverThreadIDs) > 0 || len(decoded.Input.RequestedReceiverThreadIDs) > 0
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

type codexWaitReceiverAgent struct {
	ThreadID      string `json:"threadId"`
	AgentNickname string `json:"agentNickname,omitempty"`
	AgentRole     string `json:"agentRole,omitempty"`
}

func receiverAgentsFromItemMeta(meta json.RawMessage) []codexWaitReceiverAgent {
	var decoded struct {
		Input struct {
			ReceiverThreadIDs []string                 `json:"receiverThreadIds"`
			ReceiverAgents    []codexWaitReceiverAgent `json:"receiverAgents"`
			NewAgentNickname  string                   `json:"newAgentNickname"`
			NewAgentRole      string                   `json:"newAgentRole"`
		} `json:"input"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &decoded) != nil {
		return nil
	}
	agents := decoded.Input.ReceiverAgents
	if len(agents) == 0 && len(decoded.Input.ReceiverThreadIDs) == 1 &&
		(strings.TrimSpace(decoded.Input.NewAgentNickname) != "" || strings.TrimSpace(decoded.Input.NewAgentRole) != "") {
		agents = []codexWaitReceiverAgent{{
			ThreadID:      decoded.Input.ReceiverThreadIDs[0],
			AgentNickname: strings.TrimSpace(decoded.Input.NewAgentNickname),
			AgentRole:     strings.TrimSpace(decoded.Input.NewAgentRole),
		}}
	}
	return agents
}

func receiverAgentsByThreadID(meta json.RawMessage) map[string]codexWaitReceiverAgent {
	agentsByThreadID := make(map[string]codexWaitReceiverAgent)
	for _, agent := range receiverAgentsFromItemMeta(meta) {
		agent.ThreadID = strings.TrimSpace(agent.ThreadID)
		if agent.ThreadID != "" {
			agentsByThreadID[agent.ThreadID] = agent
		}
	}
	return agentsByThreadID
}

func (r *Router) isCodexThread(threadID string) (bool, error) {
	// Narrow column read — this runs on every tool completion, where
	// GetThread's derived-sidebar-state subqueries would be wasted work.
	provider, _, err := r.store.GetThreadProviderWorkspace(threadID)
	if err != nil {
		return false, fmt.Errorf("lookup thread provider %s: %w", threadID, err)
	}
	return provider == "codex", nil
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
	// NotificationSummary is INTERNAL, never on the wire (`json:"-"`):
	// a task_notification carries its summary as the event's Content,
	// not inside this meta shape. terminalMetaFromNotification is its
	// only writer, so the sibling row a notification's own stash drain
	// creates carries the caption from its first upsert rather than
	// gaining it a write later.
	NotificationSummary string `json:"-"`
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
//     stashBackgroundTaskTerminal); the stash row carries
//     exit_code / end_time / output_file forward so the later
//     observation-event drain can merge the host's outcome into
//     the persisted sibling. Reconnect-replay is idempotent (PK is
//     thread_id+task_id). The chat-side `tool_completion` sibling
//     is NOT written here — it comes later via task_notification
//     (structured envelope or synthetic <task-notification> XML)
//     or TaskOutput drain. The tray simply continues rendering the
//     launch as "running" until the observation arrives; in practice
//     task_updated and the observation arrive in the same wire flush
//     batch so the gap is sub-perceptual.
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

	r.emit(eventchan.ProviderBackgroundTaskState, BackgroundTaskStateEvent{
		ThreadID:  evt.ThreadID,
		TaskID:    meta.TaskID,
		LaunchID:  toolUseID,
		State:     "exited",
		UpdatedAt: now,
	})
	// The state event above is loopback-only (it names the local command's
	// output-derived state), so a remote client learns this transition only
	// here. Same nudge, same refetch, no command data.
	r.emitBackgroundTasksChangedNudge(evt.ThreadID)
	return nil
}

// RecoverOrphanedBackgroundTasks walks every persisted Claude
// backgrounded tool_call row that is still `running`, still live, and
// has no completion sibling. These are launches whose owning provider
// session died with the previous app instance — the agent will never
// observe completion (the session is gone), so we synthesise the
// observation here by writing the chat-side `tool_completion` sibling
// directly. Covers both `claude` (headless) and `claude-tui`; the
// latter's launches carry no task_id, which is fine — the sibling is
// keyed off the launch id (backgroundCompletionID), not the task_id.
//
// If a `pending_background_task_terminals` stash row exists for the
// launch (only possible when it has a task_id), we drain it and merge
// its data (status/exit_code/output_file) into the synthesized meta so
// the recovered sibling reflects the real outcome the host captured.
// When there's no stash we fall back to status="killed" — the host
// never reported an exit, so "we killed it at app shutdown" is the
// closest truthful state. The sibling carries `source="session_died"`
// in both cases so the frontend can render the distinct provenance.
//
// Called once during App.ServiceStartup, after the store is open and
// before any provider session can spawn. Idempotent: running this twice
// is a no-op because the second pass sees the existing sibling and
// skips the launch via the NOT EXISTS predicate in
// ListRecoverableClaudeBackgroundLaunches. Crash-safe: if the process dies
// mid-loop before a sibling is written, the launch row is still
// `running` with no sibling. The stash drain is atomic, so a crash
// after drain but before sibling write leaves the launch as a stashless
// orphan that the next boot's sweep recovers with status="killed".
//
// Returns the count of recovered launches; logs but does not propagate
// per-launch errors so one bad row can't poison the whole sweep.
func (r *Router) RecoverOrphanedBackgroundTasks() (int, error) {
	launches, err := r.store.ListRecoverableClaudeBackgroundLaunches()
	if err != nil {
		return 0, fmt.Errorf("triage: list recoverable Claude bg launches: %w", err)
	}
	now := time.Now().UnixMilli()
	recovered := 0
	for _, launch := range launches {
		// task_id may be empty: claude-tui launches carry is_background
		// without a task_id (no task_started reconstruction). The sibling
		// is keyed off the launch id, so recovery stays idempotent either
		// way; the task_id only gates the (task_id-keyed) stash drain.
		taskID := taskIDFromItemMeta(launch.Meta)

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
		stashFound := false
		if taskID != "" {
			stash, found, err := r.store.TakePendingBackgroundTerminal(launch.ThreadID, taskID)
			if err != nil {
				log.Printf("triage: drain stranded background-terminal stash %s/%s: %v", launch.ThreadID, taskID, err)
			}
			if found {
				stashFound = true
				mergeStashIntoTerminalMeta(&meta, stash)
			}
		}
		if !stashFound {
			// No host-reported outcome — the launch was running when the
			// app died. "killed" is the closest truthful state.
			meta.Status = "killed"
		}
		if err := r.writeBackgroundCompletionSibling(syntheticEvt, meta, stashFound); err != nil {
			log.Printf("triage: synthesise session_died sibling %s/%s: %v", launch.ThreadID, launch.ID, err)
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
//
// No-ops (no sibling row, no drained emit) when the resolved launch is
// not backgrounded — inline/awaited launches complete in place via
// their own tool_result. See the IsBackground gate below.
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
	if !launch.IsBackground {
		// Inline/awaited launches (including local_agent subagents
		// launched WITHOUT run_in_background) complete in place via
		// their own tool_result — the launch row itself flips to
		// completed with the agent's real result text. Claude still
		// emits the full task lifecycle (task_started/task_updated/
		// task_notification) for these launches too (task_started
		// fires for every Bash/Task, foreground and background — see
		// provider/claude/CLAUDE.md §task_started), which is how this
		// signal reaches here at all. Writing a sibling for a
		// foreground launch would produce a redundant "-> done" row
		// alongside the already-completed launch. The caller already
		// drained any stash before reaching this point (the desired
		// side effect for the foreground case), so returning here only
		// skips the row write, not the drain.
		//
		// This also skips the `provider:background_task_state{drained}`
		// emit below, but that is safe: the event is a pure UI nudge
		// (BackgroundTaskStateEvent doc comment, turn_events.go) and
		// Store.ListLiveBackgroundTasks — the tray's source of truth —
		// filters on `is_background = 1` in every branch, so a
		// foreground launch was never tray-visible and has no stale
		// tray state to refresh.
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
	// Looked up BEFORE the meta is built: the caption below may only
	// ride the sibling's FIRST write. A later write (a task_updated
	// status flip after a TaskOutput drain already created the row)
	// adding it would grow a mounted card, which the row contract
	// forbids — same rule as the enrich path.
	var existing *store.Item
	if persisted, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID); err != nil {
		return fmt.Errorf("bg task terminal existing lookup %s: %w", completionID, err)
	} else if ok {
		existing = &persisted
	}
	if existing != nil {
		// First-write-only, enforced at the one writer regardless of
		// caller: the drain path stamps this field before it can know
		// whether a sibling already exists (a stash can outlive a
		// TaskOutput-written sibling), and a caption landing on a
		// mounted card grows it — see captionForSiblingWrite.
		meta.NotificationSummary = ""
	}
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
			backgroundNotificationCompletionMeta(
				notificationMeta, notification.PayloadID != "", outputState, readError,
				// The persisted notification row is where the summary
				// lives on the notification-FIRST order (bell arrived,
				// sibling created later by a TaskOutput drain). First
				// write only, never for a watch task, and never when
				// the notification carries an output_file (the report
				// becomes the payload — see captionForSiblingWrite).
				captionForSiblingWrite(existing, launch, notificationMeta.OutputFile, notification.Summary),
			),
		)
	}
	completion.Meta = itemMeta

	// Preserve an already-persisted sibling's created_at and
	// item_index (persistItem keeps item_index on update), but
	// overwrite the mutable fields so a second call with richer
	// payload enriches the row. (The lookup itself ran above, before
	// the meta build, so the caption could see it.)
	if existing != nil {
		persisted := *existing
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

	// The sibling is the background launch's terminal (the launch row
	// itself stays `running` forever — invariant 24), so this is where
	// its live counters become durable.
	if err := r.persistFinalSubagentProgress(launch); err != nil {
		return err
	}

	if stashWasDrained {
		r.emit(eventchan.ProviderBackgroundTaskState, BackgroundTaskStateEvent{
			ThreadID:  evt.ThreadID,
			TaskID:    meta.TaskID,
			LaunchID:  launch.ID,
			State:     "drained",
			UpdatedAt: now,
		})
	}
	// Unconditional, unlike the drained event above: the completion sibling
	// is what drops this launch out of CountLiveRunningBackgroundToolCalls,
	// so the workspace-change lock's answer just changed whether or not a
	// stash was drained on the way. That lock gates irreversible actions and
	// reads only wildcard channels, so this is the frame it waits on.
	r.emitBackgroundTasksChangedNudge(evt.ThreadID)
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

// resumeCarrierSummary rewrites a resume carrier's Summary to the
// agent-centric form ("Agent: <description>") so the row — and the
// "-> done" completion sibling buildBackgroundTerminalSummary derives
// from it — reads as the resumed agent's own completion instead of
// "SendMessage -> done". Only triggers when metaJSON carries
// resumes_tool_use_id (stamped by the parser's task_started resume
// path — see ToolStartMeta.ResumesToolUseID); a carrier whose original
// launch predates this parser instance (the reconnect edge in
// parse_system.go's task_started case) has no resumes_tool_use_id and
// keeps its default launch Summary, since there is no anchor id to
// resolve and using bare description alone risks mislabeling a
// non-resume edge case that happened to hit the same detection path.
//
// Prefers the original launch row's own Summary (it already reads
// "Agent: <description>" from its own launch) so any later
// normalization of that format stays in one place; falls back to
// "Agent: " + description when the original row lookup misses (e.g.
// retention already pruned it).
func (r *Router) resumeCarrierSummary(threadID, currentSummary, metaJSON string) string {
	resumeMeta := DecodeToolStartMeta(json.RawMessage(metaJSON))
	if resumeMeta.ResumesToolUseID == "" {
		return currentSummary
	}
	if original, found, err := r.store.GetThreadItem(threadID, resumeMeta.ResumesToolUseID); err != nil {
		log.Printf("triage: resume carrier original-launch lookup %s: %v", resumeMeta.ResumesToolUseID, err)
	} else if found && strings.TrimSpace(original.Summary) != "" {
		return original.Summary
	}
	if resumeMeta.Description != "" {
		// Intentional duplication of the "Agent: <preview>" shape the
		// launch path derives via BuildToolCallSummary+toolInputPreview
		// — there is no input JSON here to feed that pipeline, only the
		// bare description string off the rebind task_started. Bound it
		// the same way toolInputPreview bounds every other summary
		// (80 runes, newlines stripped) so a model-chosen description
		// can't write an unbounded items.summary.
		return "Agent: " + truncatePreview(resumeMeta.Description, 80)
	}
	return currentSummary
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
		commandMeta := ExtractCommandOutputMetaWithError(evt.Content, CommandFromLaunch(launch), code, "")
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
func (r *Router) rebuildCommandOutputMeta(threadID, payloadID string) error {
	data, err := r.store.GetPayloadData(threadID, payloadID)
	if err != nil {
		return fmt.Errorf("read payload for command_output meta rebuild: %w", err)
	}
	pm, err := r.store.GetPayloadMeta(threadID, payloadID)
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
	return r.store.UpdatePayloadMeta(threadID, payloadID, string(cumulativeJSON))
}

func (r *Router) turnIndexForEvent(evt provider.ProviderEvent) (int, error) {
	if evt.ParentToolUseID != "" {
		return r.turnIndexForScope(evt.ThreadID, eventParentID(evt))
	}
	return r.currentTurnIndex(evt.ThreadID)
}

func (r *Router) turnIndexForScope(threadID, scope string) (int, error) {
	if scope != "" {
		parent, found, err := r.store.GetThreadItem(threadID, strings.TrimSpace(scope))
		if err != nil {
			return 0, err
		}
		if found {
			return parent.TurnIndex, nil
		}
	}
	return r.currentTurnIndex(threadID)
}

func (r *Router) emitItemUpsert(item store.Item) {
	r.emit(eventchan.ProviderItemEvent, NewItemStreamUpsert(item))
}

func isToolStartMetaUpdateOnly(raw json.RawMessage) bool {
	return DecodeToolStartMeta(raw).isMetaUpdateOnly()
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

func MergeStoredToolCallMeta(existing string, itemType string, toolName string, incoming json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(incoming, &obj) != nil || obj == nil {
		return existing
	}
	return MergeStoredToolCallMetaObject(existing, itemType, toolName, obj)
}

// StoredToolCallMetaObject returns the persisted representation of an
// already-decoded tool-event meta object. The input map is never mutated.
// omitKeys names importer-only routing fields that must not reach storage.
func StoredToolCallMetaObject(itemType string, toolName string, incoming map[string]json.RawMessage, omitKeys ...string) string {
	if incoming == nil {
		return ""
	}
	obj := cloneRawMessageMap(incoming)
	for _, key := range omitKeys {
		delete(obj, key)
	}
	if isCodexFileChangeItem(itemType) {
		delete(obj, "item")
	}
	itemmeta.TrimToolResultEchoObject(toolName, obj)
	encoded, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// MergeStoredToolCallMetaObject is the decoded-object form of
// MergeStoredToolCallMeta. Shaping happens before encoding, so large provider
// result echoes are decoded once and discarded before the merge scans bytes.
func MergeStoredToolCallMetaObject(existing string, itemType string, toolName string, incoming map[string]json.RawMessage, omitKeys ...string) string {
	shaped := StoredToolCallMetaObject(itemType, toolName, incoming, omitKeys...)
	if shaped == "" {
		return existing
	}
	return mergeItemMetaJSON(existing, json.RawMessage(shaped))
}

func cloneRawMessageMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
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

func toolInputPreview(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil {
		return ""
	}
	// MultiAgentV2 collaboration message arguments are encrypted by the
	// model service. Canonical activity rows carry activityKind; never let a
	// stale or future adapter copy of that opaque value enter item summaries.
	if _, isV2Activity := obj["activityKind"]; isV2Activity {
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
	if max <= 0 {
		return ""
	}

	var b strings.Builder
	b.Grow(min(len(s), max))
	runeCount := 0
	started := false
	truncated := false

	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		if !started {
			if unicode.IsSpace(r) {
				continue
			}
			started = true
		}
		if runeCount >= max {
			truncated = true
			break
		}
		b.WriteRune(r)
		runeCount++
	}

	preview := strings.TrimSpace(b.String())
	if !truncated {
		return preview
	}
	if max == 1 {
		return "…"
	}

	var truncatedPreview strings.Builder
	truncatedPreview.Grow(max)
	written := 0
	for _, r := range preview {
		if written >= max-1 {
			break
		}
		truncatedPreview.WriteRune(r)
		written++
	}
	truncatedPreview.WriteString("…")
	return truncatedPreview.String()
}

func completionPayloadForLaunch(launch store.Item, evt provider.ProviderEvent, meta ToolCompleteMeta, now int64) *store.Payload {
	return CompletionPayloadForTool(launch.ID, launch.ToolName, CommandFromLaunch(launch), evt, meta, now)
}

func isCommandOutputToolName(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "Bash", "command_execution", "commandExecution", "exec_command":
		return true
	default:
		return false
	}
}

func completionPayload(itemID string, evt provider.ProviderEvent, meta ToolCompleteMeta, now int64) *store.Payload {
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
	if preview := truncatePreview(evt.Content, 240); preview != "" {
		header["preview"] = preview
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
// missing the field. Used by startup recovery to key the synthetic
// completion sibling by the same task_id the live wire path would have
// produced.
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
// index idx_items_meta_task_id) so the lookup is O(log N) instead of the
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
