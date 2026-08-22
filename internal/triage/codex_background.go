package triage

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// codex_background.go — Codex-specific background-terminal projection.
//
// Invariant 25: is_background=true on a Codex item is set ONLY from a
// wire-typed signal, never from event-ordering heuristics. The sanctioned
// authorization signals today:
//
//   1. A typed empty write_stdin poll targeted the process. This is the
//      model-visible wait signal for unified exec.
//      Raw exec_command output may enrich live state, but it is not a chat
//      history source.
//   2. A collabAgentToolCall spawn_agent whose agentsStates map still
//      reports at least one child thread in a non-terminal state when
//      the parent yields or its turn closes.
//
// The projector sits between the wire signal and the is_background stamp.
// It tracks per-thread inProgress unifiedExec items in transient state
// and spawn_agent starts in transient state. A unifiedExec is shown in
// the running-task tray immediately, but it only becomes a background
// task when a typed trigger proves the model explicitly waited on it.
// Command completion transcript history comes from typed item/completed only
// while a Codex wire round is active, matching Codex TUI timing; terminal
// interactions own only waited/interacted marker rows. A spawn_agent start
// becomes transcript history only when Codex emits the terminal spawn
// completion.
//
// The correlation is bounded: unifiedExec entries are dropped after typed
// item/completed clears the transient tracker,
// pending spawn_agent starts are dropped at the turn boundary if no
// terminal spawn completion arrived, completed spawn_agent entries are
// dropped on wait/subagent notification, and all thread state clears on
// CleanupThread. Nothing persists across sessions.
//
// Claude's handleBackgroundTaskTerminal remains the shape-of-truth for
// persisted completion sibling rows. Codex unifiedExec intentionally
// diverges: typed command item/completed owns the command row itself while a
// Codex wire round is active, while TerminalInteraction persists only the
// separate wait/interact marker rows.
//
// This file holds what the two halves SHARE: the per-thread correlation state,
// its constructor and accessor, the tray-changed emit, and the two
// tool-lifecycle entry points (observeCodexToolStart / observeCodexToolComplete)
// that dispatch into either half. The halves themselves are:
//
//   - codex_background_exec.go — unifiedExec trackers, the backgrounding
//     stamp, terminal-wait carriers, command output and command history.
//   - codex_background_subagents.go — spawn_agent launches, wait_agent
//     resolution, the child terminal-status ledger and the synthesized
//     completion row. Two narrower concerns sit beside it:
//     codex_background_mailbox.go (Codex's injected <subagent_notification>
//     deliveries) and codex_background_interactions.go (the spawn card's
//     bounded collab-interaction list).

const (
	codexLiveCommandOutputMaxBytes       = 1024 * 1024
	codexBackgroundTasksChangedEventName = "provider:background_tasks_changed"
)

// BackgroundTasksChangedEvent is the `provider:background_tasks_changed`
// payload: a refresh nudge for the tray listing, optionally carrying
// Claude's level set of live background tasks (subagent_progress.go).
// Tasks is nil for the Codex / app-side nudges, which know no set.
type BackgroundTasksChangedEvent struct {
	ThreadID string                       `json:"threadId"`
	Tasks    []provider.BackgroundTaskRef `json:"tasks,omitempty"`
}

// codexBackgroundState holds the per-thread correlation state for
// Codex's background projection. All access is synchronized via
// Router.mu — the per-observer functions each take the lock around
// their map reads/writes. Handle itself does not hold r.mu, so the
// projector is responsible for its own synchronization.
type codexBackgroundState struct {
	// unifiedExec maps launchID → tracker for inProgress unifiedExec
	// command_execution items. Typed item/completed removes the tracker and
	// persists the command row only when the frontend-visible Codex task is
	// still running.
	unifiedExec map[string]*unifiedExecTracker
	// unifiedExecByProcess maps process_id → launchID so
	// TerminalInteraction events can correlate wait/interact marker rows without
	// scanning every tracker in the hot path.
	unifiedExecByProcess map[string]string
	// pendingWaitByProcess maps process_id → latest empty-stdin
	// terminal_interaction row waiting on a still-running backgrounded
	// unifiedExec. If the command completes before the next model yield,
	// the wait carrier is flushed before the typed command completion row.
	// Later assistant text/plan content, turn completion, a
	// different-process wait, or non-empty stdin settles the wait as a
	// neutral completed carrier and detaches it so old wait rows do not
	// receive ghost completions. Reasoning deltas do not flush a wait streak.
	pendingWaitByProcess map[string]pendingTerminalWait
	// waitCarrierByProcess maps process_id → latest empty-stdin wait
	// carrier in the current turn. It outlives pendingWaitByProcess so
	// repeated canonical TerminalInteraction signals can update one visible
	// carrier even after the PTY wait has already completed.
	waitCarrierByProcess map[string]pendingTerminalWait
	// spawnAgent maps launchID → tracker for collabAgentToolCall
	// spawn_agent items that may outlive their parent turn.
	spawnAgent map[string]*spawnAgentTracker
}

func newCodexBackgroundState() *codexBackgroundState {
	return &codexBackgroundState{
		unifiedExec:          make(map[string]*unifiedExecTracker),
		unifiedExecByProcess: make(map[string]string),
		pendingWaitByProcess: make(map[string]pendingTerminalWait),
		waitCarrierByProcess: make(map[string]pendingTerminalWait),
		spawnAgent:           make(map[string]*spawnAgentTracker),
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

func (r *Router) emitCodexBackgroundTasksChanged(threadID string) {
	if strings.TrimSpace(threadID) == "" {
		return
	}
	r.emit(codexBackgroundTasksChangedEventName, BackgroundTasksChangedEvent{ThreadID: threadID})
}

func mergeRawJSONObject(left, right json.RawMessage) json.RawMessage {
	out, ok := mergeJSONObjectBytes(left, right)
	if !ok {
		return right
	}
	return out
}

// observeCodexToolStart records Codex items that may outlive the parent
// turn. It returns true when the event is fully handled by the Codex
// live projector and should not continue through the normal tool_call
// persistence path.
//
// Branches:
//   - unifiedExec startup: wire-typed `source == "unifiedExecStartup"`.
//     Tracked as transient live state for the running tray; not
//     persisted as a launch row.
//   - collabAgentToolCall spawn_agent: start is pending-only, matching
//     Codex TUI. Track it so item/completed can create the visible row
//     and stamp it backgrounded if agentsStates reports a running child.
//
// No-op for any other item type / provider — Claude runs a different
// background projection entirely (EventBackgroundTaskTerminal).
func (r *Router) observeCodexToolStart(evt provider.ProviderEvent) bool {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return false
	}
	isUnifiedExecCandidate := evt.ItemType == "commandExecution" || evt.ItemType == "command_execution"
	isSpawnAgentCandidate := evt.ItemType == "collab_agent"
	if !isUnifiedExecCandidate && !isSpawnAgentCandidate {
		return false
	}
	meta := decodeCodexItemMeta(evt.Meta)
	if meta.Source != "unifiedExecStartup" && !(isSpawnAgentCandidate && meta.Tool == "spawn_agent") {
		return false
	}

	emitChanged := false
	r.mu.Lock()
	state := r.codexBackgroundForThread(evt.ThreadID)
	now := eventTimestampMillis(evt)

	switch {
	case meta.Source == "unifiedExecStartup":
		if existing, ok := state.unifiedExec[itemID]; ok {
			rebindCodexUnifiedExecProcessLocked(state, existing, meta.ProcessID)
			r.mu.Unlock()
			if emitChanged {
				r.emitCodexBackgroundTasksChanged(evt.ThreadID)
			}
			return true
		}
		startMeta := DecodeToolStartMeta(evt.Meta)
		summary := BuildToolCallSummary(startMeta, evt.ItemType)
		if strings.TrimSpace(summary) == "" {
			summary = "Bash"
		}
		state.unifiedExec[itemID] = &unifiedExecTracker{
			launchID:  itemID,
			processID: meta.ProcessID,
			command:   codexCommandFromMeta(evt.Meta),
			summary:   summary,
			parentID:  eventParentID(evt),
			status:    statusRunning,
			meta:      evt.Meta,
			createdAt: now,
			updatedAt: now,
		}
		if meta.ProcessID != "" {
			state.unifiedExecByProcess[meta.ProcessID] = itemID
		}
		emitChanged = true
		r.mu.Unlock()
		if emitChanged {
			r.emitCodexBackgroundTasksChanged(evt.ThreadID)
		}
		return true
	case isSpawnAgentCandidate && meta.Tool == "spawn_agent":
		// Spawn_agent starts are pending-only. Codex TUI records the begin
		// event internally and renders only CollabAgentSpawnEnd; doing the
		// same here avoids persisted ghost rows when core rejects before it
		// can emit a terminal spawn event.
		if _, ok := state.spawnAgent[itemID]; ok {
			r.mu.Unlock()
			if emitChanged {
				r.emitCodexBackgroundTasksChanged(evt.ThreadID)
			}
			return true
		}
		state.spawnAgent[itemID] = &spawnAgentTracker{}
	}
	r.mu.Unlock()
	if emitChanged {
		r.emitCodexBackgroundTasksChanged(evt.ThreadID)
	}
	return isSpawnAgentCandidate && meta.Tool == "spawn_agent"
}

func (r *Router) ClearLiveCodexBackgroundTasks(threadID string) {
	r.mu.Lock()
	delete(r.codexBackground, threadID)
	r.mu.Unlock()
}

// observeCodexToolComplete handles spawn_agent and wait_agent completion:
//
//  1. A spawn_agent item closed — the spawn row itself is `completed`
//     on the wire immediately, but the child thread may still be
//     running. Refresh hasRunningChildren from the end envelope's
//     agentsStates and stamp the persisted spawn row as backgrounded
//     immediately when any child is still live.
//
//  2. A wait_agent item closed — the agent used `wait` to block on
//     children; the wait's agentsStates reports each awaited child's
//     terminal status. Any incomplete spawn_agent launch whose children
//     are now terminal gets a sibling completion row, and is cleared from
//     the projector state.
//
// Called from handleToolComplete AFTER tool_lifecycle has written the
// terminal status so the sibling row lands at the right (launch,
// completion) boundary.
func (r *Router) observeCodexToolComplete(evt provider.ProviderEvent) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return nil
	}

	// A collab interaction (send_message / followup_task, and V1 sendInput)
	// belongs on the spawn card, not on a top-level row of its own.
	if claimed, err := r.observeCodexCollabInteractionComplete(evt); err != nil || claimed {
		return err
	}

	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	var spawnTracker *spawnAgentTracker
	if state != nil {
		spawnTracker = state.spawnAgent[itemID]
	}
	r.mu.Unlock()

	if spawnTracker != nil {
		meta := decodeCodexItemMeta(evt.Meta)
		running := hasRunningChild(meta.AgentsStates)
		shouldStamp := false
		r.mu.Lock()
		spawnTracker.hasRunningChildren = running
		spawnTracker.receiverThreadIDs = meta.ReceiverThreadIDs
		if running && !spawnTracker.backgrounded {
			spawnTracker.backgrounded = true
			shouldStamp = true
		}
		if !spawnTracker.hasRunningChildren {
			if state := r.codexBackground[evt.ThreadID]; state != nil {
				delete(state.spawnAgent, itemID)
			}
		}
		r.mu.Unlock()
		if shouldStamp {
			return r.stampCodexItemBackgrounded(evt.ThreadID, itemID)
		}
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

// observeCodexModelContent is called before assistant-visible content that
// ends a terminal wait streak in the Codex TUI. It settles active wait carriers
// only; unified exec command history is owned by typed item/completed.
func (r *Router) observeCodexModelContent(threadID string) {
	r.settleCodexTerminalWaits(threadID)
}

func (r *Router) observeCodexModelReasoning(threadID string) {
	// Reasoning does not flush terminal wait streaks in Codex TUI and is not a
	// backgrounding signal for unified exec.
}

// observeCodexTurnComplete closes active wait carriers and clears pending
// spawn-agent starts. Unified exec command rows still come from typed
// item/completed, so turn completion never fabricates a command history row.
func (r *Router) observeCodexTurnComplete(threadID string) {
	r.settleCodexTerminalWaits(threadID)
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	spawnChanged := clearPendingCodexSpawnTrackersLocked(state)
	r.mu.Unlock()
	if spawnChanged {
		r.emitCodexBackgroundTasksChanged(threadID)
	}
}
