package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// codex_background_subagents.go — the spawn/subagent launch state machine of
// the Codex background projection: `spawn_agent` launch trackers, `wait_agent`
// resolution, the live child terminal-status ledger, the
// persisted-launch lookups every other spawn path resolves through, and the
// transcript completion row a terminal spawn synthesizes.
//
// The authorization rule it implements is invariant 25's second signal: a
// spawn is BACKGROUND only while its own `agentsStates` still reports a
// non-terminal child. Transcript presentation is a separate boundary again —
// a child going terminal and Codex draining its answer into parent model
// context remain operational lifecycle facts, never presentation appended to
// the historical spawn event.
//
// Two narrower concerns split out of here and read as their own files:
// codex_background_mailbox.go owns Codex's injected `<subagent_notification>`
// deliveries (FINAL_ANSWER completions and MESSAGE progress activities).
//
// The unified-exec half lives in codex_background_exec.go; the shared
// per-thread state, the two tool-lifecycle entry points that dispatch into
// both, and the file-level doc are in codex_background.go.

// spawnAgentTracker tracks collabAgentToolCall items. hasRunningChildren
// is refreshed on each item/completed envelope carrying agentsStates;
// the spawn_agent tool_call closes immediately on the wire but its
// child thread may outlive the parent turn. The projector stamps
// is_background=true as soon as that completion envelope reports a
// running child.
type spawnAgentTracker struct {
	hasRunningChildren bool
	backgrounded       bool
	receiverThreadIDs  []string
}

type agentTerminalResult struct {
	childID string
	ordinal int
	status  string
	message string
}

type pendingSubagentCompletionEmit struct {
	launchID      string
	status        string
	childResults  []agentTerminalResult
	totalChildren int
	waitCarrierID string
}

type persistedCodexSpawnLaunch struct {
	item store.Item
	meta codexItemMeta
}

type codexBackgroundCompletionOptions struct {
	sharedPayloadID string
	waitCarrierID   string
	completionID    string
}

func codexSubagentTerminalMeta(childID, status string, allTerminal bool) json.RawMessage {
	childID = strings.TrimSpace(childID)
	status = strings.TrimSpace(status)
	if status == "" {
		status = "completed"
	}
	meta := map[string]any{
		"codex_child_terminal_statuses": map[string]string{childID: status},
	}
	if allTerminal {
		meta["live_background_active"] = false
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return encoded
}

// resolveSubagentsForWait handles a wait_agent item/completed. Codex's
// completion envelope owns the terminal evidence: agentsStates carries the
// children that produced final statuses for this wait, while receiverThreadIds
// is the same completion-side set on the app-server wire. The original wait
// targets may be preserved separately for display, but they must not be used
// as completion evidence.
func (r *Router) resolveSubagentsForWait(evt provider.ProviderEvent) error {
	meta := decodeCodexItemMeta(evt.Meta)
	if len(meta.AgentsStates) == 0 {
		return nil
	}
	waitCarrierID := strings.TrimSpace(evt.ItemID)

	// Build the set of children the wait reported terminal. A child is
	// terminal when its status is NOT running or pendingInit.
	terminalChildren := make(map[string]agentTerminalResult)
	waitOrder := make(map[string]int, len(meta.ReceiverThreadIDs))
	for index, id := range meta.ReceiverThreadIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		waitOrder[id] = index + 1
	}
	for id, raw := range meta.AgentsStates {
		status := extractAgentStatus(raw)
		switch status {
		case "running", "pendingInit", "":
			continue
		default:
			terminalChildren[id] = agentTerminalResult{
				childID: id,
				status:  status,
				message: extractAgentMessage(raw),
				ordinal: waitOrder[id],
			}
		}
	}

	r.mu.Lock()
	state := r.codexBackgroundIfPresent(evt.ThreadID)
	toEmit := make([]pendingSubagentCompletionEmit, 0)
	emitted := make(map[string]struct{})
	if state != nil {
		for id, tracker := range state.spawnAgent {
			if !tracker.backgrounded {
				continue
			}
			if pending, ok := pendingSubagentWaitEmit(id, tracker.receiverThreadIDs, terminalChildren, waitCarrierID); ok {
				toEmit = append(toEmit, pending)
				emitted[id] = struct{}{}
				delete(state.spawnAgent, id)
			}
		}
	}
	r.mu.Unlock()

	persisted, err := r.persistedSubagentWaitEmits(evt.ThreadID, terminalChildren, waitCarrierID, emitted)
	if err != nil {
		return err
	}
	toEmit = append(toEmit, persisted...)

	for _, p := range toEmit {
		// V2 execution completion is fenced by the child's native turn. A
		// wait without that identity is a separate observed activity, not
		// permission to create another completion or settle a newer run.
		launch, found, err := r.store.GetThreadItem(evt.ThreadID, p.launchID)
		if err != nil {
			return err
		}
		if found {
			current := decodeCodexItemMeta(json.RawMessage(r.codexAgentRuntimeOrLaunch(launch).Meta))
			if current.Runtime != nil && current.Runtime.TurnID != "" {
				continue
			}
		}
		sort.Slice(p.childResults, func(i, j int) bool {
			return p.childResults[i].ordinal < p.childResults[j].ordinal
		})
		for i := range p.childResults {
			p.childResults[i].ordinal = i + 1
		}
		content := formatAgentCompletionMessages(p.childResults, p.totalChildren)
		synthMeta := subagentStatusToItemStatusMeta(p.status)
		synthEvt := provider.ProviderEvent{
			ThreadID:  evt.ThreadID,
			ItemID:    p.launchID,
			Content:   content,
			Meta:      synthMeta,
			Timestamp: evt.Timestamp,
		}
		sharedPayloadID := r.reusableCodexWaitPayloadID(evt, content)
		if err := r.synthesizeCodexBackgroundCompletion(synthEvt, p.launchID, codexBackgroundCompletionOptions{
			sharedPayloadID: sharedPayloadID,
			waitCarrierID:   p.waitCarrierID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func pendingSubagentWaitEmit(launchID string, receiverThreadIDs []string, terminalChildren map[string]agentTerminalResult, waitCarrierID string) (pendingSubagentCompletionEmit, bool) {
	childResults := make([]agentTerminalResult, 0, len(receiverThreadIDs))
	allDone := len(receiverThreadIDs) > 0
	hasTerminal := false
	hasInterrupted := false
	hasErrored := false
	for _, childID := range receiverThreadIDs {
		if terminal, ok := terminalChildren[childID]; ok {
			if terminal.ordinal <= 0 {
				terminal.ordinal = len(childResults) + 1
			}
			childResults = append(childResults, terminal)
			hasTerminal = true
			switch strings.TrimSpace(terminal.status) {
			case "errored":
				hasErrored = true
			case "interrupted", "shutdown", "notFound":
				hasInterrupted = true
			}
		} else {
			allDone = false
		}
	}
	if !allDone || !hasTerminal {
		return pendingSubagentCompletionEmit{}, false
	}
	status := "completed"
	if hasErrored {
		status = "errored"
	} else if hasInterrupted {
		status = "interrupted"
	}
	return pendingSubagentCompletionEmit{
		launchID:      launchID,
		status:        status,
		childResults:  childResults,
		totalChildren: len(receiverThreadIDs),
		waitCarrierID: waitCarrierID,
	}, true
}

func (r *Router) persistedSubagentWaitEmits(
	threadID string,
	terminalChildren map[string]agentTerminalResult,
	waitCarrierID string,
	already map[string]struct{},
) ([]pendingSubagentCompletionEmit, error) {
	if len(terminalChildren) == 0 {
		return nil, nil
	}
	launches, err := r.listPersistedCodexSpawnLaunches(threadID)
	if err != nil {
		return nil, err
	}
	out := make([]pendingSubagentCompletionEmit, 0)
	for _, launch := range launches {
		if _, ok := already[launch.item.ID]; ok {
			continue
		}
		if pending, ok := pendingSubagentWaitEmit(launch.item.ID, launch.meta.ReceiverThreadIDs, terminalChildren, waitCarrierID); ok {
			out = append(out, pending)
		}
	}
	return out, nil
}

// subagentStatusToItemStatusMeta translates a CollabAgentStatus value
// into a minimal Meta blob carrying `item_status` — the key the
// sibling synthesis reads for status / outcome derivation. Unknown
// statuses fall through to "completed" so a newly-introduced Codex
// enum value still produces a terminal sibling row rather than no row
// at all.
func subagentStatusToItemStatusMeta(agentStatus string) json.RawMessage {
	switch agentStatus {
	case "errored":
		return json.RawMessage(`{"item_status":"errored"}`)
	case "interrupted", "shutdown", "notFound":
		return json.RawMessage(`{"item_status":"killed"}`)
	default:
		// "completed", "shutdown", and any future value.
		return json.RawMessage(`{"item_status":"completed"}`)
	}
}

func clearPendingCodexSpawnTrackersLocked(state *codexBackgroundState) bool {
	if state == nil {
		return false
	}
	changed := false
	for id, tracker := range state.spawnAgent {
		if tracker == nil || tracker.backgrounded || tracker.hasRunningChildren || len(tracker.receiverThreadIDs) > 0 {
			continue
		}
		delete(state.spawnAgent, id)
		changed = true
	}
	return changed
}

// observeCodexSubagentStatus updates live execution state and records each
// terminal child turn as a new completion. The spawn row remains unchanged.
func (r *Router) observeCodexSubagentStatus(evt provider.ProviderEvent) error {
	parsed := decodeCodexSubagentSignalMeta(evt.Meta)
	childID, status := strings.TrimSpace(parsed.AgentPath), strings.TrimSpace(parsed.Status)
	if childID == "" || status == "" {
		return nil
	}
	launch, found, err := r.findPersistedCodexSpawnLaunchForStatus(evt.ThreadID, evt.ItemID, childID)
	if err != nil || !found {
		return err
	}
	launch.item = r.codexAgentRuntimeOrLaunch(launch.item)
	var previous struct {
		Runtime codexRuntimeMeta `json:"codex_runtime"`
	}
	if err := json.Unmarshal([]byte(launch.item.Meta), &previous); err != nil {
		return fmt.Errorf("decode child runtime: %w", err)
	}
	runtime := previous.Runtime
	if runtime.StartedAt == 0 && runtime.TurnID == "" {
		runtime.ChildStartIndex = launch.item.ItemIndex
		endIndex, err := r.store.SubagentCompletedChildIndex(evt.ThreadID, launch.item.ID)
		if err != nil {
			return err
		}
		if endIndex > runtime.ChildStartIndex {
			runtime.ChildStartIndex, runtime.ChildEndIndex = endIndex, endIndex
		}
	}
	turnID := strings.TrimSpace(evt.TurnID)
	var activity struct {
		CallID string `json:"activity_call_id"`
	}
	if err := json.Unmarshal(evt.Meta, &activity); err != nil {
		return err
	}
	// Parent-side interrupt has no child turn identity. Its snapshot or the
	// child-scoped lifecycle settles runtime, never this unfenced signal.
	if activity.CallID != "" && turnID == "" && runtime.TurnID != "" {
		return nil
	}
	active := status == "running" || status == "pendingInit"
	if !active && turnID != "" && runtime.TurnID != "" && turnID != runtime.TurnID {
		return nil
	}
	if active {
		// A new execution begins: the previous one's held completion is
		// written now, answerless, so its row precedes the new run's rows.
		if err := r.flushPendingCodexCompletion(evt.ThreadID, launch.item.ID); err != nil {
			return err
		}
	}
	now := eventTimestampMillis(evt)
	if active && (runtime.StartedAt == 0 || (turnID != "" && turnID != runtime.TurnID) || (runtime.Status != "running" && runtime.Status != "pendingInit")) {
		runtime.StartedAt = now
		if runtime.ChildEndIndex > runtime.ChildStartIndex {
			runtime.ChildStartIndex = runtime.ChildEndIndex
		}
	}
	if turnID != "" {
		runtime.TurnID = turnID
	}
	if parsed.StartedAt > 0 {
		runtime.StartedAt = parsed.StartedAt
	} else if parsed.Recovered && active {
		runtime.StartedAt = 0
	}
	if !active {
		endIndex, found, err := r.store.MaxItemIndexForTurn(evt.ThreadID, launch.item.TurnIndex)
		if err != nil {
			return fmt.Errorf("snapshot Codex execution history: %w", err)
		}
		if found {
			runtime.ChildEndIndex = endIndex
		}
		if runtime.TurnID != "" {
			completed, found, err := r.store.GetThreadItem(evt.ThreadID, codexExecutionCompletionID(launch.item.ID, runtime.TurnID, nil))
			if err != nil {
				return err
			}
			if found {
				saved := decodeCodexItemMeta(json.RawMessage(completed.Meta))
				if saved.Runtime != nil {
					runtime = *saved.Runtime
					if status == "idle" {
						status = runtime.Status
					}
				}
				now = completed.CreatedAt
			}
		}
	}
	runtime.Status, runtime.UpdatedAt, runtime.ActiveFlags = status, now, parsed.ActiveFlags
	statuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.item.Meta))
	generations := decodeCodexChildResumeGenerations(json.RawMessage(launch.item.Meta))
	if active {
		if statuses[childID] != "" {
			generations[childID]++
		}
		statuses[childID] = ""
	} else {
		statuses[childID] = status
	}
	anyActive := !allCodexSpawnChildrenTerminal(launch.meta.ReceiverThreadIDs, statuses)
	if anyActive && !active {
		runtime.Status = "running"
	}
	fields, err := json.Marshal(map[string]any{"codex_runtime": runtime, "codex_child_terminal_statuses": statuses, "codex_child_resume_generations": generations, "live_background_active": anyActive})
	if err != nil {
		return err
	}
	launch.item.Meta = mergeItemMetaJSON(launch.item.Meta, fields)
	launch.item.UpdatedAt = now
	if err := r.setCodexAgentRuntime(launch.item); err != nil {
		return err
	}

	r.mu.Lock()
	state := r.codexBackgroundForThread(evt.ThreadID)
	if anyActive {
		state.spawnAgent[launch.item.ID] = &spawnAgentTracker{backgrounded: true, hasRunningChildren: true, receiverThreadIDs: launch.meta.ReceiverThreadIDs}
	} else {
		delete(state.spawnAgent, launch.item.ID)
	}
	r.mu.Unlock()
	terminal := status == "completed" || status == "errored" || status == "interrupted" || status == "shutdown" || status == "notFound"
	if terminal && !anyActive && (!parsed.Recovered || runtime.TurnID != "") {
		completionID := codexExecutionCompletionID(launch.item.ID, runtime.TurnID, generations)
		completionMeta, err := json.Marshal(map[string]any{
			"item_status": aggregateCodexSubagentTerminalStatus(launch.meta.ReceiverThreadIDs, statuses), "codex_runtime": runtime,
			"codex_execution_started_at":        runtime.StartedAt,
			"codex_execution_completed_at":      now,
			"codex_execution_child_start_index": runtime.ChildStartIndex,
			"codex_execution_child_end_index":   runtime.ChildEndIndex,
		})
		if err != nil {
			return err
		}
		completion := evt
		completion.Meta = completionMeta
		pending := pendingCodexCompletion{evt: completion, completionID: completionID}
		if r.shouldDeferCodexCompletion(evt.ThreadID, launch.item, runtime.TurnID, status, parsed.Recovered) {
			// The answer is on its way (codex_answer_completion.go); the
			// row is written when it lands, with the answer as payload.
			r.deferCodexCompletion(evt.ThreadID, launch.item.ID, pending)
		} else if err := r.persistPendingCodexCompletion(launch.item.ID, pending, ""); err != nil {
			return err
		}
	}

	r.emitBackgroundTasksChangedNudge(evt.ThreadID)
	return nil
}

type codexRuntimeMeta struct {
	ChildStartIndex int      `json:"childStartIndex"`
	ChildEndIndex   int      `json:"childEndIndex"`
	TurnID          string   `json:"turnId"`
	Status          string   `json:"status"`
	StartedAt       int64    `json:"startedAt"`
	UpdatedAt       int64    `json:"updatedAt"`
	ActiveFlags     []string `json:"activeFlags"`
}

func (r *Router) findPersistedCodexSpawnLaunchForStatus(threadID, launchID, childID string) (persistedCodexSpawnLaunch, bool, error) {
	if launchID == "" {
		return r.findPersistedCodexSpawnLaunch(threadID, "", childID, true)
	}
	launch, found, err := r.store.GetThreadItem(threadID, launchID)
	if err != nil || !found {
		return persistedCodexSpawnLaunch{}, false, err
	}
	if launch.Kind != itemKindToolCall || launch.ToolName != "collab_agent" {
		return persistedCodexSpawnLaunch{}, false, nil
	}
	meta := decodeCodexItemMeta(json.RawMessage(r.codexAgentRuntimeOrLaunch(launch).Meta))
	if strings.TrimSpace(childID) != "" && !containsString(meta.ReceiverThreadIDs, childID) {
		return persistedCodexSpawnLaunch{}, false, nil
	}
	return persistedCodexSpawnLaunch{item: launch, meta: meta}, true, nil
}

func (r *Router) observeCodexSpawnChildTerminalInMemory(threadID, launchID string, allTerminal bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.codexBackgroundIfPresent(threadID)
	if state == nil {
		return
	}
	tracker := state.spawnAgent[launchID]
	if tracker == nil {
		return
	}
	if allTerminal {
		tracker.hasRunningChildren = false
		delete(state.spawnAgent, launchID)
	}
}

func (r *Router) markCodexSpawnChildTerminal(launch store.Item, meta codexItemMeta, childID, status string) (bool, string, error) {
	launch = r.codexAgentRuntimeOrLaunch(launch)
	var allTerminal bool
	launch.Meta, allTerminal, _ = MergeCodexSubagentTerminalMeta(launch.Meta, childID, status)
	terminalStatuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.Meta))
	aggregateStatus := aggregateCodexSubagentTerminalStatus(meta.ReceiverThreadIDs, terminalStatuses)
	launch.UpdatedAt = time.Now().UnixMilli()
	if err := r.setCodexAgentRuntime(launch); err != nil {
		return allTerminal, aggregateStatus, err
	}

	return allTerminal, aggregateStatus, nil
}

func aggregateCodexSubagentTerminalStatus(receiverThreadIDs []string, terminalStatuses map[string]string) string {
	hasInterrupted := false
	for _, childID := range receiverThreadIDs {
		status := strings.TrimSpace(terminalStatuses[strings.TrimSpace(childID)])
		switch status {
		case "errored":
			return "errored"
		case "interrupted", "shutdown", "notFound":
			hasInterrupted = true
		}
	}
	if hasInterrupted {
		return "interrupted"
	}
	return "completed"
}

func (r *Router) listPersistedCodexSpawnLaunches(threadID string) ([]persistedCodexSpawnLaunch, error) {
	launches, err := r.store.ListIncompleteCodexSubagentLaunches(threadID)
	if err != nil {
		return nil, fmt.Errorf("codex-background list spawn launches for %s: %w", threadID, err)
	}
	out := make([]persistedCodexSpawnLaunch, 0, len(launches))
	for _, launch := range launches {
		if launch.ToolName != "collab_agent" {
			continue
		}
		meta := decodeCodexItemMeta(json.RawMessage(launch.Meta))
		out = append(out, persistedCodexSpawnLaunch{item: launch, meta: meta})
	}
	return out, nil
}

func (r *Router) findPersistedCodexSpawnLaunch(threadID, launchID, childID string, requireChild bool) (persistedCodexSpawnLaunch, bool, error) {
	if launchID != "" {
		launch, found, err := r.store.GetIncompleteCodexSubagentLaunch(threadID, launchID)
		if err != nil {
			return persistedCodexSpawnLaunch{}, false, err
		}
		if !found {
			return persistedCodexSpawnLaunch{}, false, nil
		}
		meta := decodeCodexItemMeta(json.RawMessage(launch.Meta))
		if requireChild && !containsString(meta.ReceiverThreadIDs, childID) {
			return persistedCodexSpawnLaunch{}, false, nil
		}
		return persistedCodexSpawnLaunch{item: launch, meta: meta}, true, nil
	}

	launches, err := r.listPersistedCodexSpawnLaunches(threadID)
	if err != nil {
		return persistedCodexSpawnLaunch{}, false, err
	}
	for _, launch := range launches {
		if requireChild && !containsString(launch.meta.ReceiverThreadIDs, childID) {
			continue
		}
		if containsString(launch.meta.ReceiverThreadIDs, childID) {
			return launch, true, nil
		}
	}
	return persistedCodexSpawnLaunch{}, false, nil
}

// stampCodexItemBackgrounded initializes the live execution from its recorded spawn.
func (r *Router) stampCodexItemBackgrounded(threadID, itemID string) error {
	launch, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return fmt.Errorf("codex-background lookup %s: %w", itemID, err)
	}
	if !found {
		log.Printf("triage: codex-background stamp target %s missing on thread %s", itemID, threadID)
		return nil
	}
	current := r.codexAgentRuntimeOrLaunch(launch)
	if decodeCodexItemMeta(json.RawMessage(current.Meta)).Runtime != nil {
		return nil
	}
	current.Meta = mergeItemMetaJSON(current.Meta, json.RawMessage(fmt.Sprintf(`{"live_background_active":true,"codex_runtime":{"status":"running","startedAt":%d}}`, launch.CreatedAt)))
	if err := r.setCodexAgentRuntime(current); err != nil {
		return err
	}
	r.emitBackgroundTasksChangedNudge(threadID)
	return nil
}

// synthesizeCodexBackgroundCompletion writes the tool_completion
// sibling row for a backgrounded Codex item. Unprompted tray
// notifications are deferred through the interrupt queue so a mid-stream
// completion queues behind the active text/reasoning block. Explicit wait
// completions persist immediately because the wait carrier is the
// timeline boundary that should own the indented completion row. The
// sibling lands at the LATEST turn's tail (not the launching turn) —
// Codex subagents can complete long after their spawn row, across many turns,
// and the row must appear where the timeline's write-head is at completion
// time.
//
// Stable execution ids make repeated completion signals a no-op.
func (r *Router) synthesizeCodexBackgroundCompletion(evt provider.ProviderEvent, launchID string, opts codexBackgroundCompletionOptions) error {
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, launchID)
	if err != nil {
		return fmt.Errorf("codex-background sibling lookup %s: %w", launchID, err)
	}
	if !found || launch.Kind != itemKindToolCall {
		log.Printf("triage: codex-background sibling no launch row for %s on thread %s", launchID, evt.ThreadID)
		return nil
	}

	tailTurn, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.TurnIndex, launch.ParentID)
	if err != nil {
		// Fall back to the launch turn rather than drop the row. A
		// store read failure here is rare, but the launching turn is
		// still a valid home for the sibling if the write-head cannot
		// be resolved.
		log.Printf("triage: codex-background completion turn index %s: %v", launchID, err)
	}

	now := eventTimestampMillis(evt)

	completionID := strings.TrimSpace(opts.completionID)
	if completionID == "" {
		completionID = ToolCompletionID(launch.ID)
	}
	completion := store.Item{
		ID:           completionID,
		ThreadID:     evt.ThreadID,
		TurnIndex:    tailTurn,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       codexBackgroundCompletionStatus(evt.Meta),
		Summary:      buildCodexBackgroundCompletionSummary(launch.Summary, evt.Meta),
		ParentID:     launch.ParentID,
		IsBackground: true,
		CompletionOf: launch.ID,
		ToolName:     launch.ToolName,
		Meta:         validJSONObjectString(addCodexWaitCarrierMeta(evt.Meta, opts.waitCarrierID)),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if launch.PayloadID != "" && launch.PayloadKind == "command_output" {
		completion.PayloadID = launch.PayloadID
	}
	if _, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID); err == nil && ok {
		return nil
	} else if err != nil {
		return fmt.Errorf("codex-background sibling existing lookup %s: %w", completionID, err)
	}

	if launch.ToolName == "collab_agent" {
		current := r.codexAgentRuntimeOrLaunch(launch)
		// Snapshot profile and progress once onto this completion. Neither
		// current runtime nor later child rows may enrich it afterwards.
		completion.Meta = mergeItemMetaJSON(current.Meta, json.RawMessage(completion.Meta))
		completion.Meta = mergeItemMetaJSON(completion.Meta, json.RawMessage(`{"codex_live_projection":false}`))
		if progress, ok := r.PeekSubagentProgress(evt.ThreadID, launchID); ok {
			progress.Activity = ""
			fields, err := json.Marshal(map[string]any{subagentProgressMetaKey: progress})
			if err != nil {
				return err
			}
			completion.Meta = mergeItemMetaJSON(completion.Meta, fields)
		}
	}
	if launch.ToolName == "collab_agent" {
		var bounds struct {
			Start *int `json:"codex_execution_child_start_index"`
			End   *int `json:"codex_execution_child_end_index"`
		}
		if err := json.Unmarshal([]byte(completion.Meta), &bounds); err != nil {
			return err
		}
		if bounds.Start != nil && bounds.End != nil {
			completion.Meta, err = r.store.SnapshotSubagentExecutionMeta(evt.ThreadID, launchID, completion.Meta, launch.TurnIndex, *bounds.Start, *bounds.End)
			if err != nil {
				return fmt.Errorf("snapshot Codex completion aggregates: %w", err)
			}
		}
	}

	payload := attachCodexBackgroundCompletionPayload(&completion, launch, evt, now, opts.sharedPayloadID)

	if strings.TrimSpace(opts.waitCarrierID) != "" {
		return r.persistItem(completion, payload)
	}
	return r.maybeDeferOrPersist(evt.ThreadID, completion, payload)
}

func attachCodexBackgroundCompletionPayload(
	completion *store.Item,
	launch store.Item,
	evt provider.ProviderEvent,
	now int64,
	sharedPayloadID string,
) *store.Payload {
	if completion.PayloadID == "" && sharedPayloadID != "" {
		completion.PayloadID = sharedPayloadID
		return nil
	}
	if completion.PayloadID != "" && completion.PayloadID != launch.PayloadID && strings.TrimSpace(evt.Content) == "" {
		return nil
	}

	payload := completionPayload(completion.ID, evt, DecodeToolCompleteMeta(evt.Meta), now)
	if payload == nil {
		return nil
	}
	if completion.PayloadID == "" {
		return payload
	}
	if completion.PayloadID == launch.PayloadID {
		return nil
	}
	payload.ID = completion.PayloadID
	return payload
}

func (r *Router) reusableCodexWaitPayloadID(evt provider.ProviderEvent, content string) string {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(content) != strings.TrimSpace(evt.Content) {
		return ""
	}
	waitRow, found, err := r.store.GetThreadItem(evt.ThreadID, ToolCompletionID(evt.ItemID))
	if err != nil {
		log.Printf("triage: codex-background wait payload lookup %s: %v", evt.ItemID, err)
		return ""
	}
	if !found {
		waitRow, found, err = r.store.GetThreadItem(evt.ThreadID, evt.ItemID)
		if err != nil {
			log.Printf("triage: codex-background wait payload lookup %s: %v", evt.ItemID, err)
			return ""
		}
	}
	if !found || waitRow.PayloadKind != payloadKindToolCallResult {
		return ""
	}
	return waitRow.PayloadID
}
