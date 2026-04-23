package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// codex_background.go — Codex-specific background-terminal projection.
//
// Invariant 25: is_background=true on a Codex item is set ONLY from a
// wire-typed signal, never from event-ordering heuristics. The sanctioned
// signals today:
//
//   1. CommandExecution.source == "unifiedExecStartup" — a command that
//      the model yielded on while its PTY kept running.
//   2. A collabAgentToolCall spawn_agent whose agentsStates map still
//      reports at least one child thread in a non-terminal state when
//      the parent yields or its turn closes.
//
// The projector sits between the wire signal and the is_background stamp.
// It tracks per-thread inProgress unifiedExec items and spawn_agent
// launch rows, then flips the row on the first MODEL-PRODUCED event
// following the launch once the provider has shown that work is still
// running (text delta, reasoning delta, or turn-completed as the
// catchall). A yield moment is the unambiguous "the agent has moved on
// while this work is still running" signal — i.e. this is no longer a
// synchronous tool call.
//
// The correlation is bounded: completed unifiedExec entries are dropped
// on item/completed, completed spawn_agent entries are dropped on
// wait/subagent notification, and all thread state clears on
// CleanupThread. Nothing persists across sessions.
//
// Claude's handleBackgroundTaskTerminal is the shape-of-truth for the
// completion sibling row; keep the two paths aligned so the tray and
// timeline render backgrounded tasks consistently across providers.

// unifiedExecTracker is the per-thread per-launchID state tracked by
// the projector between item/started and item/completed. backgrounded
// is set once a yield has been observed and the row has been stamped
// is_background=true — prevents double-emission. The sibling-synthesis
// path pulls the current turn index from the store at completion time
// so the sibling row lands at the timeline tail, not the launch turn;
// no launch-time turn index is retained here.
type unifiedExecTracker struct {
	backgrounded bool
	// processID is kept purely for logging / future tray enrichment.
	processID string
}

// spawnAgentTracker mirrors unifiedExecTracker for collabAgentToolCall
// items. hasRunningChildren is refreshed on each item/completed envelope
// carrying agentsStates; the spawn_agent tool_call closes immediately on
// the wire but its child thread may outlive the parent turn. The
// projector stamps is_background=true on the first model yield after
// that running child is observed, with parent turn/completed as a
// fallback.
type spawnAgentTracker struct {
	hasRunningChildren bool
	backgrounded       bool
	receiverThreadIDs  []string
}

// codexBackgroundState holds the per-thread correlation state for
// Codex's background projection. All access is synchronized via
// Router.mu — the per-observer functions each take the lock around
// their map reads/writes. Handle itself does not hold r.mu, so the
// projector is responsible for its own synchronization.
type codexBackgroundState struct {
	// unifiedExec maps launchID → tracker for inProgress unifiedExec
	// command_execution items.
	unifiedExec map[string]*unifiedExecTracker
	// pendingUnifiedExec is the number of unifiedExec trackers that have
	// not yet been stamped backgrounded. Text/thinking deltas are hot-path
	// events; this lets observeCodexModelYield skip the map scan once all
	// tracked commands have already yielded.
	pendingUnifiedExec int
	// spawnAgent maps launchID → tracker for collabAgentToolCall
	// spawn_agent items that may outlive their parent turn.
	spawnAgent map[string]*spawnAgentTracker
	// pendingSpawnAgent is the number of spawnAgent trackers that have
	// observed running children but have not yet been stamped backgrounded.
	pendingSpawnAgent int
}

func newCodexBackgroundState() *codexBackgroundState {
	return &codexBackgroundState{
		unifiedExec: make(map[string]*unifiedExecTracker),
		spawnAgent:  make(map[string]*spawnAgentTracker),
	}
}

// codexBackgroundForThread returns (or lazily creates) the per-thread
// projector state. Must be called with r.mu held.
func (r *Router) codexBackgroundForThread(threadID string) *codexBackgroundState {
	state, ok := r.codexBackground[threadID]
	if !ok {
		state = newCodexBackgroundState()
		r.codexBackground[threadID] = state
	}
	return state
}

// observeCodexToolStart records inProgress Codex items that may become
// backgrounded. Called from handleToolStart AFTER the lifecycle row has
// persisted so the projector can find the row on subsequent events.
//
// Branches:
//   - unifiedExec startup: wire-typed `source == "unifiedExecStartup"`.
//     Tracked; stamped is_background on the first yield (text/reasoning/
//     turn-complete).
//   - collabAgentToolCall spawn_agent: tracked; stamped on the first model
//     yield after agentsStates reports a running child, or on turn/completed
//     as a fallback. The spawn row itself closes on the wire immediately —
//     observeCodexToolComplete refreshes the running-children flag from the
//     end envelope.
//
// No-op for any other item type / provider — Claude runs a different
// background projection entirely (EventBackgroundTaskTerminal).
func (r *Router) observeCodexToolStart(evt provider.ProviderEvent) {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return
	}
	isUnifiedExecCandidate := evt.ItemType == "commandExecution" || evt.ItemType == "command_execution"
	isSpawnAgentCandidate := evt.ItemType == "collab_agent"
	if !isUnifiedExecCandidate && !isSpawnAgentCandidate {
		return
	}
	meta := decodeCodexItemMeta(evt.Meta)
	if meta.Source != "unifiedExecStartup" && !(isSpawnAgentCandidate && meta.Tool == "spawn_agent") {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackgroundForThread(evt.ThreadID)

	switch {
	case meta.Source == "unifiedExecStartup":
		// Idempotent replay guard: on session resume Codex re-emits
		// item/started for any still-inProgress command. If we already
		// tracked this launch and have already stamped it backgrounded,
		// leave the existing entry alone so the replay doesn't reset
		// the flag or clobber the processID.
		if existing, ok := state.unifiedExec[itemID]; ok {
			existing.processID = meta.ProcessID
			return
		}
		state.unifiedExec[itemID] = &unifiedExecTracker{
			processID: meta.ProcessID,
		}
		state.pendingUnifiedExec++
	case isSpawnAgentCandidate && meta.Tool == "spawn_agent":
		// Spawn_agent rows rarely stay inProgress — the item/started event
		// is a very short-lived marker before the immediate completed
		// envelope. We stamp on the completed envelope instead (see
		// observeCodexToolComplete); tracking here just establishes the
		// tracker so the later refresh can attach the running-children
		// flag and receiver ids.
		if _, ok := state.spawnAgent[itemID]; ok {
			return
		}
		state.spawnAgent[itemID] = &spawnAgentTracker{}
	}
}

// observeCodexToolComplete handles three distinct cases on item/completed:
//
//  1. A tracked unifiedExec command closed — the row flips to a terminal
//     status via tool_lifecycle. If the row was backgrounded, synthesize
//     the tool_completion sibling at the LATEST turn's tail (not the
//     launching turn — long commands can complete multiple turns later).
//
//  2. A spawn_agent item closed — the spawn row itself is `completed`
//     on the wire immediately, but the child thread may still be
//     running. Refresh hasRunningChildren from the end envelope's
//     agentsStates so turn/completed can see the final state.
//
//  3. A wait_agent item closed — the agent used `wait` to block on
//     children; the wait's agentsStates reports each awaited child's
//     terminal status. Any backgrounded spawn_agent tracker whose
//     children are now terminal gets a sibling completion row, and is
//     cleared from the projector state.
//
// Called from handleToolComplete AFTER tool_lifecycle has written the
// terminal status so the sibling row lands at the right (launch,
// completion) boundary.
func (r *Router) observeCodexToolComplete(evt provider.ProviderEvent) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return nil
	}

	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	var unifiedTracker *unifiedExecTracker
	var spawnTracker *spawnAgentTracker
	if state != nil {
		unifiedTracker = state.unifiedExec[itemID]
		spawnTracker = state.spawnAgent[itemID]
	}
	r.mu.Unlock()

	if unifiedTracker != nil {
		// Clear the tracker first so a duplicate item/completed (rare but
		// possible on reconnect / replay) can't double-synthesize. The
		// sibling upsert itself is idempotent by stable id, but clearing
		// here keeps the inProgress map bounded. Re-fetch the state
		// under the lock because CleanupThread may have dropped it
		// between the earlier unlock and here.
		r.mu.Lock()
		if state := r.codexBackground[evt.ThreadID]; state != nil {
			if !unifiedTracker.backgrounded && state.pendingUnifiedExec > 0 {
				state.pendingUnifiedExec--
			}
			delete(state.unifiedExec, itemID)
		}
		r.mu.Unlock()
		if unifiedTracker.backgrounded {
			return r.synthesizeCodexBackgroundCompletion(evt, itemID)
		}
		return nil
	}

	if spawnTracker != nil {
		meta := decodeCodexItemMeta(evt.Meta)
		running := hasRunningChild(meta.AgentsStates)
		r.mu.Lock()
		wasPending := spawnTracker.hasRunningChildren && !spawnTracker.backgrounded
		spawnTracker.hasRunningChildren = running
		spawnTracker.receiverThreadIDs = meta.ReceiverThreadIDs
		isPending := spawnTracker.hasRunningChildren && !spawnTracker.backgrounded
		switch {
		case !wasPending && isPending:
			if state := r.codexBackground[evt.ThreadID]; state != nil {
				state.pendingSpawnAgent++
			}
		case wasPending && !isPending:
			if state := r.codexBackground[evt.ThreadID]; state != nil && state.pendingSpawnAgent > 0 {
				state.pendingSpawnAgent--
			}
		}
		// Only clear the tracker when no children remain running. When
		// at least one child is still active we keep it so turn/completed
		// can decide whether to stamp is_background. Re-fetch the state
		// under the lock so a concurrent CleanupThread doesn't cause a
		// nil-map access.
		if !spawnTracker.hasRunningChildren {
			if state := r.codexBackground[evt.ThreadID]; state != nil {
				delete(state.spawnAgent, itemID)
			}
		}
		r.mu.Unlock()
	}

	// wait_agent completion: the parent just resumed after blocking on
	// children. Each awaited child is terminal per the wait's
	// agentsStates map. Find any backgrounded spawn_agent trackers
	// whose receiver thread ids intersect with the just-closed children
	// and emit their sibling completion rows.
	if evt.ItemType == "wait_agent" {
		return r.resolveSubagentsForWait(evt)
	}
	return nil
}

// resolveSubagentsForWait handles a wait_agent item/completed. The
// wait's Meta carries agentsStates (child thread id → terminal status)
// and receiverThreadIds (the children it was waiting on). For each
// backgrounded spawn_agent tracker whose children are now terminal,
// synthesize a sibling completion row and drop the tracker.
func (r *Router) resolveSubagentsForWait(evt provider.ProviderEvent) error {
	meta := decodeCodexItemMeta(evt.Meta)
	if len(meta.AgentsStates) == 0 && len(meta.ReceiverThreadIDs) == 0 {
		return nil
	}

	// Build the set of children the wait reported terminal. A child is
	// terminal when its status is NOT running or pendingInit.
	terminalChildren := make(map[string]string)
	for id, raw := range meta.AgentsStates {
		status := extractAgentStatus(raw)
		switch status {
		case "running", "pendingInit", "":
			continue
		default:
			terminalChildren[id] = status
		}
	}

	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	if state == nil {
		r.mu.Unlock()
		return nil
	}
	type pendingEmit struct {
		launchID string
		status   string
	}
	toEmit := make([]pendingEmit, 0)
	for id, tracker := range state.spawnAgent {
		if !tracker.backgrounded {
			continue
		}
		// A backgrounded spawn_agent still holds receiverThreadIDs
		// pointing at its children. If any of those children just
		// reached a terminal state in this wait, the spawn is done.
		terminalStatus := ""
		allDone := len(tracker.receiverThreadIDs) > 0
		for _, childID := range tracker.receiverThreadIDs {
			if status, ok := terminalChildren[childID]; ok {
				if terminalStatus == "" {
					terminalStatus = status
				}
			} else {
				allDone = false
			}
		}
		if !allDone || terminalStatus == "" {
			continue
		}
		toEmit = append(toEmit, pendingEmit{launchID: id, status: terminalStatus})
		delete(state.spawnAgent, id)
	}
	r.mu.Unlock()

	for _, p := range toEmit {
		synthMeta := subagentStatusToItemStatusMeta(p.status)
		synthEvt := provider.ProviderEvent{
			ThreadID:  evt.ThreadID,
			ItemID:    p.launchID,
			Meta:      synthMeta,
			Timestamp: evt.Timestamp,
		}
		if err := r.synthesizeCodexBackgroundCompletion(synthEvt, p.launchID); err != nil {
			return err
		}
	}
	return nil
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

// observeCodexModelYield is called on every EventTextDelta and
// EventThinking. Any tracked unifiedExec items, plus spawn_agent items
// with running children, that haven't already been stamped get flipped
// to is_background=true and re-emitted so the tray and timeline reflect
// the new state.
//
// The distinction between "tool-call batch" (multiple tools dispatched
// in one response) and "tool → yield" is what keeps this precise: a
// text/reasoning delta is unambiguous evidence that the model moved on
// while the command / child agent is still running. EventToolStart
// explicitly does NOT participate in yield detection — sibling tool starts
// in a parallel batch fire before any model text.
func (r *Router) observeCodexModelYield(threadID string) {
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil || (state.pendingUnifiedExec == 0 && state.pendingSpawnAgent == 0) {
		r.mu.Unlock()
		return
	}
	toFlip := make([]string, 0, state.pendingUnifiedExec)
	for id, tracker := range state.unifiedExec {
		if tracker.backgrounded {
			continue
		}
		tracker.backgrounded = true
		if state.pendingUnifiedExec > 0 {
			state.pendingUnifiedExec--
		}
		toFlip = append(toFlip, id)
	}
	spawnToFlip := make([]string, 0, state.pendingSpawnAgent)
	for id, tracker := range state.spawnAgent {
		if tracker.backgrounded || !tracker.hasRunningChildren {
			continue
		}
		tracker.backgrounded = true
		if state.pendingSpawnAgent > 0 {
			state.pendingSpawnAgent--
		}
		spawnToFlip = append(spawnToFlip, id)
	}
	r.mu.Unlock()

	for _, id := range toFlip {
		if err := r.stampCodexItemBackgrounded(threadID, id); err != nil {
			log.Printf("triage: codex-background stamp %s: %v", id, err)
		}
	}
	for _, id := range spawnToFlip {
		if err := r.stampCodexItemBackgrounded(threadID, id); err != nil {
			log.Printf("triage: codex-background stamp spawn %s: %v", id, err)
		}
	}
}

// observeCodexTurnComplete is the catchall. Runs before the tool
// lifecycle's force-close pass so any unifiedExec or spawn_agent item
// that remained inProgress / has running children through the turn
// boundary is stamped backgrounded in time for the invariant-24
// force-close exemption.
func (r *Router) observeCodexTurnComplete(threadID string) {
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	unifiedToFlip := make([]string, 0, len(state.unifiedExec))
	for id, tracker := range state.unifiedExec {
		if tracker.backgrounded {
			continue
		}
		tracker.backgrounded = true
		if state.pendingUnifiedExec > 0 {
			state.pendingUnifiedExec--
		}
		unifiedToFlip = append(unifiedToFlip, id)
	}
	spawnToFlip := make([]string, 0, len(state.spawnAgent))
	for id, tracker := range state.spawnAgent {
		if tracker.backgrounded || !tracker.hasRunningChildren {
			continue
		}
		tracker.backgrounded = true
		if state.pendingSpawnAgent > 0 {
			state.pendingSpawnAgent--
		}
		spawnToFlip = append(spawnToFlip, id)
	}
	r.mu.Unlock()

	for _, id := range unifiedToFlip {
		if err := r.stampCodexItemBackgrounded(threadID, id); err != nil {
			log.Printf("triage: codex-background turn-close stamp unified %s: %v", id, err)
		}
	}
	for _, id := range spawnToFlip {
		if err := r.stampCodexItemBackgrounded(threadID, id); err != nil {
			log.Printf("triage: codex-background turn-close stamp spawn %s: %v", id, err)
		}
	}
}

// observeCodexSubagentNotification handles the detached-child closure
// signal: Codex core injects a <subagent_notification> tag into the
// parent's next user message when a backgrounded child finished with no
// wait outstanding. The projector matches on agent_path (the child
// path) and synthesizes a sibling completion row for any is_background=true
// spawn_agent row that owns the now-closed child. When the provider
// resolved the path to a parent card, evt.ItemID is the authoritative
// launch id; otherwise we fall back to receiverThreadIDs for older
// unnamed-agent builds where agent_path was the receiver thread id.
// Persistence failures on any single sibling return up so the calling
// handleSubagentNotification surfaces them — we log per failure but the
// first error still escapes rather than silently leaving the tray stale.
func (r *Router) observeCodexSubagentNotification(evt provider.ProviderEvent) error {
	parsed := decodeSubagentNotificationMeta(evt.Meta)
	if parsed.AgentPath == "" {
		return nil
	}
	threadID := evt.ThreadID

	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return nil
	}
	toEmit := make([]string, 0)
	if launchID := strings.TrimSpace(evt.ItemID); launchID != "" {
		if tracker := state.spawnAgent[launchID]; tracker != nil && tracker.backgrounded {
			toEmit = append(toEmit, launchID)
			delete(state.spawnAgent, launchID)
		}
	}
	if len(toEmit) == 0 {
		for id, tracker := range state.spawnAgent {
			if !tracker.backgrounded {
				continue
			}
			if !containsString(tracker.receiverThreadIDs, parsed.AgentPath) {
				continue
			}
			toEmit = append(toEmit, id)
			delete(state.spawnAgent, id)
		}
	}
	r.mu.Unlock()

	// Translate CollabAgentStatus → item_status so the sibling row
	// carries a consistent outcome marker.
	syntheticMeta := subagentStatusToItemStatusMeta(parsed.Status)

	var firstErr error
	for _, id := range toEmit {
		evt := provider.ProviderEvent{
			ThreadID:  threadID,
			ItemID:    id,
			Meta:      syntheticMeta,
			Timestamp: time.Now(),
		}
		if err := r.synthesizeCodexBackgroundCompletion(evt, id); err != nil {
			log.Printf("triage: codex-background subagent completion %s: %v", id, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// stampCodexItemBackgrounded flips is_background=true on a persisted
// row and re-emits the upsert. The row MUST already exist — the
// projector only tracks ids that went through persistToolCallLaunch,
// so a missing row is a sign of a race we can't silently heal.
//
// For spawn_agent rows (collab_agent toolName), the row's wire-level
// status is `completed` as of item/completed but the child thread is
// still doing work — we also revert the row to `status=running` so
// the tray's ListLiveBackgroundTasks query surfaces it as a live
// background launch. See invariant 24: backgrounded launches stay
// `status=running` and the sibling completion row carries the terminal
// state. The status flip mirrors what unifiedExec rows already show
// (they never flipped to completed in the first place — the
// is_background branch in persistToolCallCompletion short-circuits the
// status flip when the projector stamped the launch before
// item/completed fired).
func (r *Router) stampCodexItemBackgrounded(threadID, itemID string) error {
	launch, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return fmt.Errorf("codex-background lookup %s: %w", itemID, err)
	}
	if !found {
		log.Printf("triage: codex-background stamp target %s missing on thread %s", itemID, threadID)
		return nil
	}
	needsStatusRevert := launch.ToolName == "collab_agent" && launch.Status != statusRunning
	if launch.IsBackground && !needsStatusRevert {
		return nil
	}
	launch.IsBackground = true
	if needsStatusRevert {
		launch.Status = statusRunning
	}
	launch.UpdatedAt = time.Now().UnixMilli()
	return r.persistItem(launch, nil)
}

// synthesizeCodexBackgroundCompletion writes the tool_completion
// sibling row for a backgrounded Codex item. Deferred through the
// interrupt queue so a mid-stream completion queues behind the active
// text/reasoning block. The sibling lands at the LATEST turn's tail
// (not the launching turn) — long unifiedExec commands can complete
// hours after their launch, across many turns, and the row must appear
// where the timeline's write-head is at completion time.
//
// Idempotent by stable id (`complete:<launchID>`): a duplicate
// item/completed upserts in place rather than creating a second row.
func (r *Router) synthesizeCodexBackgroundCompletion(evt provider.ProviderEvent, launchID string) error {
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

	completionID := nextToolCompletionID(launch.ID)
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
		if existing.PayloadID != "" {
			completion.PayloadID = existing.PayloadID
		}
	} else if err != nil {
		return fmt.Errorf("codex-background sibling existing lookup %s: %w", completionID, err)
	}

	return r.maybeDeferOrPersist(evt.ThreadID, completion, nil)
}

// codexBackgroundCompletionStatus maps the completing item's item_status
// meta key onto the canonical item status enum. A unifiedExec command
// that yielded and later closed either completed cleanly or failed; the
// wire uses CommandExecutionStatus (inProgress | completed | failed).
// An absent item_status (e.g. synthesized for spawn_agent after a
// subagent_notification) defaults to completed.
func codexBackgroundCompletionStatus(meta json.RawMessage) string {
	decoded := decodeCodexItemMeta(meta)
	switch decoded.ItemStatus {
	case "failed", "errored":
		return statusErrored
	case "killed":
		return statusKilled
	default:
		return statusCompleted
	}
}

// buildCodexBackgroundCompletionSummary produces the sibling row's
// summary. Prefers the launch summary followed by a short outcome
// marker (`-> done`, `-> failed`) so the tray stays readable. Falls
// back to a bare "done" when no launch summary is available.
func buildCodexBackgroundCompletionSummary(launchSummary string, meta json.RawMessage) string {
	outcome := codexBackgroundOutcome(meta)
	summary := strings.TrimSpace(launchSummary)
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

func codexBackgroundOutcome(meta json.RawMessage) string {
	decoded := decodeCodexItemMeta(meta)
	switch decoded.ItemStatus {
	case "failed":
		return "failed"
	case "errored":
		return "errored"
	case "killed":
		return "killed"
	case "completed":
		return "done"
	default:
		return ""
	}
}

// codexItemMeta is the subset of a Codex EventToolStart /
// EventToolComplete Meta blob that the projector needs. See
// protocol.enrichItemMeta for the source of each field.
type codexItemMeta struct {
	Source            string
	ItemStatus        string
	ProcessID         string
	Tool              string
	AgentsStates      map[string]json.RawMessage
	ReceiverThreadIDs []string
}

// decodeCodexItemMeta pulls the projector-relevant fields out of a
// wire-enriched Meta blob. Malformed JSON returns the zero value rather
// than bubbling the error — the projector's behaviour on an empty meta
// is "no wire-typed signal, skip", which is the correct fallback for a
// garbled envelope. Upstream validation (`enrichItemMeta` in the Codex
// parser) already produces well-formed JSON; a decode failure here
// would indicate a corrupt event that couldn't have been routed
// correctly anyway.
func decodeCodexItemMeta(raw json.RawMessage) codexItemMeta {
	if len(raw) == 0 {
		return codexItemMeta{}
	}
	var shell struct {
		Source     string          `json:"source"`
		ItemStatus string          `json:"item_status"`
		ProcessID  string          `json:"process_id"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return codexItemMeta{}
	}
	out := codexItemMeta{
		Source:     shell.Source,
		ItemStatus: shell.ItemStatus,
		ProcessID:  shell.ProcessID,
	}
	if len(shell.Input) == 0 {
		return out
	}
	var input struct {
		Tool              string                     `json:"tool"`
		AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
		ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
	}
	if err := json.Unmarshal(shell.Input, &input); err != nil {
		return out
	}
	out.Tool = input.Tool
	out.AgentsStates = input.AgentsStates
	out.ReceiverThreadIDs = input.ReceiverThreadIDs
	return out
}

// hasRunningChild reports whether agentsStates contains at least one
// child in a non-terminal state. The agentsStates map keys are child
// thread ids; values are CollabAgentStatus variants ("pendingInit" |
// "running" | "interrupted" | "completed" | "errored" | "shutdown" |
// "notFound"). Running or pendingInit count as non-terminal; anything
// else is terminal. The value may be either a bare string ("running") or
// an object ({status: "running"}) — Codex ships both shapes depending on
// wire version.
func hasRunningChild(states map[string]json.RawMessage) bool {
	for _, raw := range states {
		status := extractAgentStatus(raw)
		switch status {
		case "running", "pendingInit":
			return true
		}
	}
	return false
}

// extractAgentStatus pulls the status string from an agentsStates entry.
// Accepts both bare-string ("running") and object ({status: "running"})
// shapes — older wire sends strings, v2 sends nested objects.
func extractAgentStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err == nil {
		return bare
	}
	var obj struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Status
	}
	return ""
}

// subagentNotificationMeta mirrors the frontend-facing shape emitted by
// session.go's <subagent_notification> parser.
type subagentNotificationMeta struct {
	AgentPath string `json:"agent_path"`
	Status    string `json:"status"`
}

func decodeSubagentNotificationMeta(raw json.RawMessage) subagentNotificationMeta {
	if len(raw) == 0 {
		return subagentNotificationMeta{}
	}
	var m subagentNotificationMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return subagentNotificationMeta{}
	}
	return m
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
