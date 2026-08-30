package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// codex_background_subagents.go — the spawn/subagent launch state machine of
// the Codex background projection: `spawn_agent` launch trackers, `wait_agent`
// resolution, the child terminal-status ledger on the launch row, the
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
			case "interrupted", "notFound":
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
	case "interrupted", "notFound":
		return json.RawMessage(`{"item_status":"failed"}`)
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

// observeCodexSubagentStatus handles child-thread lifecycle signals that prove
// a spawned Codex subagent is no longer actively working. Direct child
// lifecycle is live-state evidence only; parent transcript completion is owned
// by typed wait_agent completions or Codex's injected <subagent_notification>.
func (r *Router) observeCodexSubagentStatus(evt provider.ProviderEvent) error {
	parsed := decodeCodexSubagentSignalMeta(evt.Meta)
	childID := strings.TrimSpace(parsed.AgentPath)
	if childID == "" {
		return nil
	}
	status := strings.TrimSpace(parsed.Status)
	if status == "" {
		status = "completed"
	}

	threadID := evt.ThreadID
	launchID := strings.TrimSpace(evt.ItemID)
	if status == "running" || status == "pendingInit" {
		return r.reactivateCodexSpawnChild(threadID, launchID, childID)
	}

	launch, found, err := r.findPersistedCodexSpawnLaunchForStatus(threadID, launchID, childID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	allTerminal, _, err := r.markCodexSpawnChildTerminal(launch.item, launch.meta, childID, status)
	if err != nil {
		return err
	}
	r.observeCodexSpawnChildTerminalInMemory(threadID, launch.item.ID, allTerminal)
	if allTerminal {
		r.emitCodexBackgroundTasksChanged(threadID)
	}
	return nil
}

func (r *Router) reactivateCodexSpawnChild(threadID, launchID, childID string) error {
	launch, found, err := r.findPersistedCodexSpawnLaunchForStatus(threadID, launchID, childID)
	if err != nil || !found {
		return err
	}
	terminalStatuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.item.Meta))
	// A child that was already terminal and is running again started a new turn
	// (`followup_task` or resume). Advance the durable generation so a repeated
	// FINAL_ANSWER remains a distinct completion row.
	resumed := strings.TrimSpace(terminalStatuses[childID]) != ""
	// mergeItemMetaJSON deep-merges maps, so an explicit empty value clears
	// this child logically without requiring a delete sentinel in stored JSON.
	terminalStatuses[childID] = ""
	fields := map[string]any{
		"codex_child_terminal_statuses": terminalStatuses,
		"live_background_active":        true,
	}
	if resumed {
		// The resume generation is what keeps a child that legitimately
		// answers identically twice (followup_task -> "Done." again) on two
		// rows: it is mixed into codexMailboxCompletionID, which is otherwise
		// a pure content hash. The counter is durable so reconnects and live
		// delivery carriers agree on the same identity.
		generations := decodeCodexChildResumeGenerations(json.RawMessage(launch.item.Meta))
		generations[childID]++
		fields["codex_child_resume_generations"] = generations
	}
	extra, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	launch.item.Meta = mergeItemMetaJSON(launch.item.Meta, extra)
	launch.item.Meta, err = itemmeta.MarkCodexBackgroundRuntimeActive(launch.item.Meta)
	if err != nil {
		return fmt.Errorf("reactivate Codex spawn %s: %w", launch.item.ID, err)
	}
	launch.item.UpdatedAt = time.Now().UnixMilli()
	if err := r.persistItem(launch.item, nil); err != nil {
		return err
	}

	r.mu.Lock()
	state := r.codexBackgroundForThread(threadID)
	tracker := state.spawnAgent[launch.item.ID]
	if tracker == nil {
		tracker = &spawnAgentTracker{}
		state.spawnAgent[launch.item.ID] = tracker
	}
	tracker.backgrounded = true
	tracker.hasRunningChildren = true
	tracker.receiverThreadIDs = append([]string(nil), launch.meta.ReceiverThreadIDs...)
	r.mu.Unlock()
	r.emitCodexBackgroundTasksChanged(threadID)
	return nil
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
	meta := decodeCodexItemMeta(json.RawMessage(launch.Meta))
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
	var allTerminal bool
	launch.Meta, allTerminal, _ = MergeCodexSubagentTerminalMeta(launch.Meta, childID, status)
	terminalStatuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.Meta))
	aggregateStatus := aggregateCodexSubagentTerminalStatus(meta.ReceiverThreadIDs, terminalStatuses)
	launch.UpdatedAt = time.Now().UnixMilli()
	if err := r.persistItem(launch, nil); err != nil {
		return allTerminal, aggregateStatus, err
	}
	// A child going terminal is where this spawn's live token counters
	// stop moving, so they become durable here. Every child terminal
	// folds — not just the last — because a spawn with several children
	// keeps ticking after the first one settles, and the fold is
	// order-free: the persisted numbers are the next fold's merge base.
	// Ordered AFTER the persist above so it merges onto the meta that
	// write just landed rather than being clobbered by it.
	if err := r.persistFinalSubagentProgress(launch); err != nil {
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
		case "interrupted", "notFound":
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

// stampCodexItemBackgrounded flips is_background=true on a persisted row. The
// row MUST already exist — the projector only tracks ids that went through
// persistToolCallLaunch, so a missing row is a sign of a race we can't silently
// heal.
func (r *Router) stampCodexItemBackgrounded(threadID, itemID string) error {
	launch, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return fmt.Errorf("codex-background lookup %s: %w", itemID, err)
	}
	if !found {
		log.Printf("triage: codex-background stamp target %s missing on thread %s", itemID, threadID)
		return nil
	}
	if launch.IsBackground {
		return nil
	}
	launch.IsBackground = true
	launch.UpdatedAt = time.Now().UnixMilli()
	return r.persistItem(launch, nil)
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
// Idempotent by stable id (`complete:<launchID>`): a duplicate
// item/completed upserts in place rather than creating a second row.
func (r *Router) synthesizeCodexBackgroundCompletion(evt provider.ProviderEvent, launchID string, opts codexBackgroundCompletionOptions) error {
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, launchID)
	if err != nil {
		return fmt.Errorf("codex-background sibling lookup %s: %w", launchID, err)
	}
	if !found || launch.Kind != itemKindToolCall {
		log.Printf("triage: codex-background sibling no launch row for %s on thread %s", launchID, evt.ThreadID)
		return nil
	}

	tailTurn, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.TurnIndex)
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
	if existing, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID); err == nil && ok {
		completion.CreatedAt = existing.CreatedAt
		completion.TurnIndex = existing.TurnIndex
		completion.ItemIndex = existing.ItemIndex
		completion.Meta = mergeItemMetaJSON(existing.Meta, json.RawMessage(completion.Meta))
		if existing.PayloadID != "" {
			completion.PayloadID = existing.PayloadID
		}
	} else if err != nil {
		return fmt.Errorf("codex-background sibling existing lookup %s: %w", completionID, err)
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
