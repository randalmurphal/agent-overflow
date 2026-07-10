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

const (
	codexLiveCommandOutputMaxBytes       = 1024 * 1024
	codexBackgroundTasksChangedEventName = "provider:background_tasks_changed"
)

// unifiedExecTracker is the per-thread per-launchID state tracked for
// Codex unified exec command executions. Unlike Claude background
// tasks, these rows are not timeline history while they are live:
// they first appear only in the running-task tray. Typed item/completed removes
// the tracker; it persists the normal command row only while a Codex wire round
// is still active. Empty write_stdin polls create separate terminal_interaction
// marker rows while the tracker is still live.
type unifiedExecTracker struct {
	backgrounded bool
	launchID     string
	processID    string
	command      string
	summary      string
	parentID     string
	status       string
	exitCode     int
	meta         json.RawMessage
	output       cappedCommandOutput
	createdAt    int64
	updatedAt    int64
}

type cappedCommandOutput struct {
	data  []byte
	ring  bool
	start int
	size  int
}

type pendingTerminalWait struct {
	itemID    string
	turnIndex int
	createdAt int64
}

type BackgroundTasksChangedEvent struct {
	ThreadID string `json:"threadId"`
}

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
}

const (
	codexExecResultRunning = "running"
	codexExecResultExited  = "exited"
)

type codexExecResultMeta struct {
	ProcessID string `json:"process_id"`
	Result    string `json:"result"`
	Command   string `json:"command"`
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

func codexCommandFromMeta(raw json.RawMessage) string {
	var parsed struct {
		Command string `json:"command"`
		Input   struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	if strings.TrimSpace(parsed.Command) != "" {
		return parsed.Command
	}
	return parsed.Input.Command
}

func codexLiveUnifiedExecMeta(command string) string {
	meta := map[string]any{
		"source": "unifiedExecStartup",
	}
	if trimmed := strings.TrimSpace(command); trimmed != "" {
		meta["command"] = trimmed
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return `{"source":"unifiedExecStartup"}`
	}
	return string(encoded)
}

func codexExitCodeFromMeta(raw json.RawMessage) int {
	var parsed struct {
		ExitCode      int `json:"exitCode"`
		ExitCodeSnake int `json:"exit_code"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &parsed) != nil {
		return 0
	}
	if parsed.ExitCode != 0 {
		return parsed.ExitCode
	}
	return parsed.ExitCodeSnake
}

func mergeRawJSONObject(left, right json.RawMessage) json.RawMessage {
	out, ok := mergeJSONObjectBytes(left, right)
	if !ok {
		return right
	}
	return out
}

func addCodexWaitCarrierMeta(raw json.RawMessage, waitCarrierID string) json.RawMessage {
	waitCarrierID = strings.TrimSpace(waitCarrierID)
	if waitCarrierID == "" {
		return raw
	}
	extra, err := json.Marshal(map[string]any{"wait_carrier_id": waitCarrierID})
	if err != nil {
		return raw
	}
	return mergeRawJSONObject(raw, extra)
}

func codexSubagentTerminalMeta(childID, status string, allTerminal bool, aggregateStatus string) json.RawMessage {
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
		meta["codex_child_status"] = aggregateStatus
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return encoded
}

func (o *cappedCommandOutput) Append(delta string) {
	if delta == "" {
		return
	}
	if len(delta) >= codexLiveCommandOutputMaxBytes {
		o.data = []byte(delta[len(delta)-codexLiveCommandOutputMaxBytes:])
		o.ring = false
		o.start = 0
		o.size = len(o.data)
		return
	}
	if !o.ring && o.size+len(delta) <= codexLiveCommandOutputMaxBytes {
		o.data = append(o.data, delta...)
		o.size = len(o.data)
		return
	}
	if !o.ring {
		next := make([]byte, codexLiveCommandOutputMaxBytes)
		copy(next, o.data)
		o.data = next
		o.ring = true
		o.start = 0
	}
	for i := 0; i < len(delta); i++ {
		if o.size < codexLiveCommandOutputMaxBytes {
			pos := (o.start + o.size) % codexLiveCommandOutputMaxBytes
			o.data[pos] = delta[i]
			o.size++
			continue
		}
		o.data[o.start] = delta[i]
		o.start = (o.start + 1) % codexLiveCommandOutputMaxBytes
	}
}

func (o *cappedCommandOutput) Replace(output string) {
	*o = cappedCommandOutput{}
	o.Append(output)
}

func (o cappedCommandOutput) String() string {
	if o.size == 0 {
		return ""
	}
	if !o.ring {
		return string(o.data[:o.size])
	}
	out := make([]byte, o.size)
	first := copy(out, o.data[o.start:])
	copy(out[first:], o.data[:o.size-first])
	return string(out)
}

func (o cappedCommandOutput) Bytes() []byte {
	if o.size == 0 {
		return nil
	}
	if !o.ring {
		out := make([]byte, o.size)
		copy(out, o.data[:o.size])
		return out
	}
	out := make([]byte, o.size)
	first := copy(out, o.data[o.start:])
	copy(out[first:], o.data[:o.size-first])
	return out
}

func (o cappedCommandOutput) Empty() bool {
	return o.size == 0
}

func markCodexUnifiedExecTrackerYieldedLocked(state *codexBackgroundState, tracker *unifiedExecTracker, now int64) bool {
	if state == nil || tracker == nil || tracker.backgrounded {
		return false
	}
	tracker.backgrounded = true
	tracker.updatedAt = now
	return true
}

func (r *Router) markCodexUnifiedExecProcessBackgrounded(threadID, processID string) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return
	}
	changed := false
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		tracker := codexUnifiedExecTrackerByProcessLocked(state, processID)
		if tracker == nil {
			tracker = codexFirstUnboundUnifiedExecTrackerLocked(state)
			if tracker != nil {
				rebindCodexUnifiedExecProcessLocked(state, tracker, processID)
			}
		}
		if markCodexUnifiedExecTrackerYieldedLocked(state, tracker, time.Now().UnixMilli()) {
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.emitCodexBackgroundTasksChanged(threadID)
	}
}

func codexUnifiedExecTrackerByProcessLocked(state *codexBackgroundState, processID string) *unifiedExecTracker {
	processID = strings.TrimSpace(processID)
	if state == nil || processID == "" {
		return nil
	}
	if launchID := state.unifiedExecByProcess[processID]; launchID != "" {
		if tracker := state.unifiedExec[launchID]; tracker != nil {
			if tracker.processID == "" {
				tracker.processID = processID
			}
			if tracker.processID == processID {
				return tracker
			}
		}
		delete(state.unifiedExecByProcess, processID)
	}
	for _, tracker := range state.unifiedExec {
		if tracker != nil && tracker.processID == processID {
			state.unifiedExecByProcess[processID] = tracker.launchID
			return tracker
		}
	}
	return nil
}

func codexFirstUnboundUnifiedExecTrackerLocked(state *codexBackgroundState) *unifiedExecTracker {
	if state == nil {
		return nil
	}
	for _, tracker := range state.unifiedExec {
		if tracker != nil && tracker.processID == "" {
			return tracker
		}
	}
	return nil
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
		startMeta := decodeToolStartMeta(evt.Meta)
		summary := buildToolCallSummary(startMeta, evt.ItemType)
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

func (r *Router) settleCodexTerminalWaits(threadID string) {
	r.settleCodexTerminalWaitsExcept(threadID, "")
}

func (r *Router) settleCodexTerminalWaitsExcept(threadID, keepProcessID string) {
	keepProcessID = strings.TrimSpace(keepProcessID)
	type pendingProcessWait struct {
		processID string
		wait      pendingTerminalWait
	}
	var waits []pendingProcessWait
	var seenItemIDs map[string]struct{}
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil || (len(state.pendingWaitByProcess) == 0 && len(state.waitCarrierByProcess) == 0) {
		r.mu.Unlock()
		return
	}
	waits = make([]pendingProcessWait, 0, len(state.pendingWaitByProcess)+len(state.waitCarrierByProcess))
	for processID, wait := range state.pendingWaitByProcess {
		if keepProcessID != "" && processID == keepProcessID {
			continue
		}
		itemID := strings.TrimSpace(wait.itemID)
		if itemID == "" {
			continue
		}
		if seenItemIDs == nil {
			seenItemIDs = make(map[string]struct{})
		}
		seenItemIDs[itemID] = struct{}{}
		waits = append(waits, pendingProcessWait{
			processID: processID,
			wait:      wait,
		})
	}
	for processID, wait := range state.waitCarrierByProcess {
		if keepProcessID != "" && processID == keepProcessID {
			continue
		}
		itemID := strings.TrimSpace(wait.itemID)
		if itemID == "" {
			continue
		}
		if _, seen := seenItemIDs[itemID]; seen {
			continue
		}
		if seenItemIDs == nil {
			seenItemIDs = make(map[string]struct{})
		}
		seenItemIDs[itemID] = struct{}{}
		waits = append(waits, pendingProcessWait{
			processID: processID,
			wait:      wait,
		})
	}
	r.mu.Unlock()
	for _, wait := range waits {
		if err := r.settleCodexTerminalWaitCarrier(threadID, wait.wait.itemID); err != nil {
			log.Printf("triage: settle terminal wait %s: %v", wait.wait.itemID, err)
			continue
		}
		r.clearCodexPendingTerminalWaitState(threadID, wait.processID, wait.wait.itemID)
		r.clearCodexTerminalWaitCarrierIfMatches(threadID, wait.processID, wait.wait.itemID)
	}
}

func (r *Router) settleCodexTerminalWaitForProcess(threadID, processID string) error {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return nil
	}
	var wait pendingTerminalWait
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		if pending := state.pendingWaitByProcess[processID]; strings.TrimSpace(pending.itemID) != "" {
			wait = pending
		} else if carrier := state.waitCarrierByProcess[processID]; strings.TrimSpace(carrier.itemID) != "" {
			wait = carrier
		}
	}
	r.mu.Unlock()
	if strings.TrimSpace(wait.itemID) == "" {
		return nil
	}
	if err := r.settleCodexTerminalWaitCarrier(threadID, wait.itemID); err != nil {
		return err
	}
	r.clearCodexPendingTerminalWaitState(threadID, processID, wait.itemID)
	r.clearCodexTerminalWaitCarrierIfMatches(threadID, processID, wait.itemID)
	return nil
}

func (r *Router) trackCodexPendingTerminalWait(threadID, processID, itemID string, turnIndex int, createdAt int64) bool {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return false
	}
	tracker := codexUnifiedExecTrackerByProcessLocked(state, processID)
	if tracker != nil {
		state.pendingWaitByProcess[processID] = pendingTerminalWait{
			itemID:    itemID,
			turnIndex: turnIndex,
			createdAt: createdAt,
		}
		return true
	} else {
		candidate := codexFirstUnboundUnifiedExecTrackerLocked(state)
		if candidate != nil {
			rebindCodexUnifiedExecProcessLocked(state, candidate, processID)
			state.pendingWaitByProcess[processID] = pendingTerminalWait{
				itemID:    itemID,
				turnIndex: turnIndex,
				createdAt: createdAt,
			}
			return true
		}
	}
	return false
}

func (r *Router) rememberCodexTerminalWaitCarrier(threadID, processID, itemID string, turnIndex int, createdAt int64) {
	processID = strings.TrimSpace(processID)
	itemID = strings.TrimSpace(itemID)
	if processID == "" || itemID == "" {
		return
	}
	r.mu.Lock()
	state := r.codexBackgroundForThread(threadID)
	state.waitCarrierByProcess[processID] = pendingTerminalWait{
		itemID:    itemID,
		turnIndex: turnIndex,
		createdAt: createdAt,
	}
	r.mu.Unlock()
}

func (r *Router) clearCodexTerminalWaitCarrierIfMatches(threadID, processID, itemID string) {
	processID = strings.TrimSpace(processID)
	itemID = strings.TrimSpace(itemID)
	if processID == "" || itemID == "" {
		return
	}
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		if wait := state.waitCarrierByProcess[processID]; strings.TrimSpace(wait.itemID) == itemID {
			delete(state.waitCarrierByProcess, processID)
		}
	}
	r.mu.Unlock()
}

func (r *Router) clearCodexPendingTerminalWait(threadID, processID string) error {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return nil
	}
	var itemID string
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		wait := state.pendingWaitByProcess[processID]
		itemID = wait.itemID
	}
	r.mu.Unlock()
	if err := r.settleCodexTerminalWaitCarrier(threadID, itemID); err != nil {
		return err
	}
	r.clearCodexPendingTerminalWaitState(threadID, processID, itemID)
	return nil
}

func (r *Router) clearCodexPendingTerminalWaitState(threadID, processID, itemID string) {
	processID = strings.TrimSpace(processID)
	itemID = strings.TrimSpace(itemID)
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		if processID != "" {
			if wait := state.pendingWaitByProcess[processID]; strings.TrimSpace(wait.itemID) == itemID {
				delete(state.pendingWaitByProcess, processID)
			}
		}
	}
	r.mu.Unlock()
}

func (r *Router) codexTerminalWaitItemIDForProcess(threadID, processID string, turnIndex int) string {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return ""
	}
	if wait := state.pendingWaitByProcess[processID]; wait.itemID != "" && wait.turnIndex == turnIndex {
		return wait.itemID
	}
	if wait := state.waitCarrierByProcess[processID]; wait.itemID != "" && wait.turnIndex == turnIndex {
		return wait.itemID
	}
	return ""
}

func (r *Router) hasCodexBackgroundTerminalForProcess(threadID, processID string) bool {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return false
	}
	tracker := codexUnifiedExecTrackerByProcessLocked(state, processID)
	return tracker != nil && tracker.backgrounded
}

func (r *Router) settleCodexTerminalWaitCarrier(threadID, itemID string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	item, found, err := r.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return fmt.Errorf("terminal wait carrier lookup %s: %w", itemID, err)
	}
	if !found || item.Kind != string(provider.ItemTerminalInteraction) {
		return nil
	}
	if item.Status != statusRunning && item.Status != statusStreaming {
		return nil
	}
	item.Status = statusCompleted
	item.UpdatedAt = time.Now().UnixMilli()
	return r.persistItem(item, nil)
}

func (r *Router) codexTerminalSummaryForProcess(threadID, processID string) string {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return ""
	}
	launchID := state.unifiedExecByProcess[processID]
	tracker := state.unifiedExec[launchID]
	if tracker == nil {
		return ""
	}
	return tracker.summary
}

func (r *Router) codexTerminalCommandForProcess(threadID, processID string) string {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return ""
	}
	launchID := state.unifiedExecByProcess[processID]
	tracker := state.unifiedExec[launchID]
	if tracker == nil {
		return ""
	}
	return strings.TrimSpace(tracker.command)
}

func rebindCodexUnifiedExecProcessLocked(state *codexBackgroundState, tracker *unifiedExecTracker, processID string) {
	processID = strings.TrimSpace(processID)
	if state == nil || tracker == nil || processID == "" || tracker.processID == processID {
		return
	}
	if tracker.processID != "" {
		delete(state.unifiedExecByProcess, tracker.processID)
		delete(state.pendingWaitByProcess, tracker.processID)
		delete(state.waitCarrierByProcess, tracker.processID)
	}
	tracker.processID = processID
	state.unifiedExecByProcess[processID] = tracker.launchID
}

func decodeCodexExecResultMeta(raw json.RawMessage) codexExecResultMeta {
	var decoded codexExecResultMeta
	if len(raw) == 0 {
		return decoded
	}
	_ = json.Unmarshal(raw, &decoded)
	decoded.ProcessID = strings.TrimSpace(decoded.ProcessID)
	decoded.Result = strings.TrimSpace(decoded.Result)
	decoded.Command = strings.TrimSpace(decoded.Command)
	return decoded
}

// handleCodexExecResult records the model-visible result of the original
// exec_command call. Codex TUI does not use this raw model transcript to decide
// whether command history is visible; typed item/completed owns that. We keep
// the signal only to enrich the live process tracker when it arrives.
func (r *Router) handleCodexExecResult(evt provider.ProviderEvent) error {
	meta := decodeCodexExecResultMeta(evt.Meta)
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" || meta.Result == "" {
		return nil
	}

	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	if state != nil {
		tracker := state.unifiedExec[itemID]
		if tracker == nil && meta.ProcessID != "" {
			tracker = codexUnifiedExecTrackerByProcessLocked(state, meta.ProcessID)
		}
		if tracker != nil {
			rebindCodexUnifiedExecProcessLocked(state, tracker, meta.ProcessID)
			if meta.Command != "" {
				tracker.command = meta.Command
			}
			tracker.meta = mergeRawJSONObject(tracker.meta, evt.Meta)
			tracker.updatedAt = eventTimestampMillis(evt)
		}
	}
	r.mu.Unlock()
	return nil
}

func removeCodexUnifiedExecTrackerLocked(state *codexBackgroundState, tracker *unifiedExecTracker) {
	if state == nil || tracker == nil {
		return
	}
	if tracker.processID != "" {
		delete(state.unifiedExecByProcess, tracker.processID)
		delete(state.pendingWaitByProcess, tracker.processID)
		delete(state.waitCarrierByProcess, tracker.processID)
	}
	delete(state.unifiedExec, tracker.launchID)
}

// observeCodexCommandOutput buffers output for live Codex unified exec
// commands and prevents the generic command-output path from creating
// ghost timeline rows for background PTYs.
func (r *Router) observeCodexCommandOutput(evt provider.ProviderEvent) bool {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false
	}
	meta := decodeCodexItemMeta(evt.Meta)
	command := codexCommandFromMeta(evt.Meta)
	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	if state == nil {
		r.mu.Unlock()
		return false
	}
	now := eventTimestampMillis(evt)
	tracker := state.unifiedExec[itemID]
	if tracker == nil {
		r.mu.Unlock()
		return false
	}
	rebindCodexUnifiedExecProcessLocked(state, tracker, meta.ProcessID)
	if evt.Replace {
		tracker.output.Replace(evt.Content)
	} else {
		tracker.output.Append(evt.Content)
	}
	if strings.TrimSpace(command) != "" {
		tracker.command = command
	}
	tracker.meta = mergeRawJSONObject(tracker.meta, evt.Meta)
	tracker.updatedAt = now
	r.mu.Unlock()
	return true
}

// observeCodexUnifiedExecComplete owns item/completed for tracked unified exec
// startups. This mirrors Codex TUI: late completions still clear background
// process state, but chat history only changes while the task indicator is
// running. TerminalInteraction owns only the separate waited/interacted marker
// rows.
func (r *Router) observeCodexUnifiedExecComplete(evt provider.ProviderEvent) (bool, error) {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false, nil
	}

	meta := decodeCodexItemMeta(evt.Meta)
	command := codexCommandFromMeta(evt.Meta)
	status := codexBackgroundCompletionStatus(evt.Meta)
	exitCode := codexExitCodeFromMeta(evt.Meta)

	var tracker unifiedExecTracker
	var pendingWait pendingTerminalWait
	handled := false
	hasPendingWait := false
	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	now := eventTimestampMillis(evt)
	if state != nil && state.unifiedExec[itemID] != nil {
		live := state.unifiedExec[itemID]
		handled = true
		live.status = status
		live.exitCode = exitCode
		live.updatedAt = now
		if strings.TrimSpace(command) != "" {
			live.command = command
		}
		live.meta = mergeRawJSONObject(live.meta, evt.Meta)
		rebindCodexUnifiedExecProcessLocked(state, live, meta.ProcessID)
		if evt.Content != "" {
			live.output.Replace(evt.Content)
		}
		tracker = *live
		if live.processID != "" {
			if wait, ok := state.pendingWaitByProcess[live.processID]; ok {
				pendingWait = wait
				hasPendingWait = true
			}
		}
		removeCodexUnifiedExecTrackerLocked(state, live)
	}
	r.mu.Unlock()
	if !handled {
		return false, nil
	}
	if hasPendingWait {
		if err := r.settleCodexTerminalWaitCarrier(evt.ThreadID, pendingWait.itemID); err != nil {
			return true, err
		}
		r.clearCodexPendingTerminalWaitState(evt.ThreadID, tracker.processID, pendingWait.itemID)
		r.clearCodexTerminalWaitCarrierIfMatches(evt.ThreadID, tracker.processID, pendingWait.itemID)
	}
	r.emitCodexBackgroundTasksChanged(evt.ThreadID)
	turnIndex, ok := r.activeRoundTurnIndex(evt.ThreadID)
	if !ok {
		return true, nil
	}
	return true, r.persistCodexUnifiedExecCommand(evt, tracker, turnIndex)
}

func (r *Router) persistCodexUnifiedExecCommand(evt provider.ProviderEvent, tracker unifiedExecTracker, turnIndex int) error {
	meta := decodeToolStartMeta(evt.Meta)
	summary := buildToolCallSummary(meta, evt.ItemType)
	if strings.TrimSpace(summary) == "" {
		summary = tracker.summary
	}
	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        tracker.launchID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindToolCall,
		Role:      "assistant",
		Status:    tracker.status,
		Summary:   buildCompletionSummary(summary, decodeToolCompleteMeta(evt.Meta)),
		ParentID:  tracker.parentID,
		ToolName:  "command_execution",
		Meta:      validJSONObjectString(mergeRawJSONObject(tracker.meta, evt.Meta)),
		CreatedAt: tracker.createdAt,
		UpdatedAt: now,
	}
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	output := tracker.output.String()
	if output == "" {
		return r.persistItem(item, nil)
	}
	outputEvt := evt
	outputEvt.Content = output
	outputEvt.Replace = true
	return r.attachPayloadToItem(item, outputEvt, "command_output", item.Summary, true)
}

// ListLiveCodexBackgroundTasks returns transient Codex unified exec tray
// rows. Pending foreground commands are included with IsBackground=false;
// yielded PTYs are included with IsBackground=true. Completed commands leave
// the live tray when typed item/completed removes the transient tracker.
func (r *Router) ListLiveCodexBackgroundTasks(threadID string, _ int64, _ int64) []store.Item {
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return nil
	}
	trackers := make([]unifiedExecTracker, 0, len(state.unifiedExec))
	for _, tracker := range state.unifiedExec {
		if tracker == nil {
			continue
		}
		trackers = append(trackers, *tracker)
	}
	r.mu.Unlock()

	sort.SliceStable(trackers, func(i, j int) bool {
		if trackers[i].createdAt != trackers[j].createdAt {
			return trackers[i].createdAt < trackers[j].createdAt
		}
		return trackers[i].launchID < trackers[j].launchID
	})

	items := make([]store.Item, 0, len(trackers))
	for _, tracker := range trackers {
		if strings.TrimSpace(tracker.parentID) != "" {
			continue
		}
		launch := store.Item{
			ID:           tracker.launchID,
			ThreadID:     threadID,
			TurnIndex:    0,
			ItemIndex:    0,
			Kind:         itemKindToolCall,
			Role:         "assistant",
			Status:       statusRunning,
			Summary:      tracker.summary,
			ParentID:     tracker.parentID,
			IsBackground: tracker.backgrounded,
			ToolName:     "command_execution",
			Meta:         codexLiveUnifiedExecMeta(tracker.command),
			CreatedAt:    tracker.createdAt,
			UpdatedAt:    tracker.updatedAt,
		}
		items = append(items, launch)
	}
	return items
}

func (r *Router) CountLiveCodexBackgroundTasks(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return 0
	}
	count := 0
	for _, tracker := range state.unifiedExec {
		if tracker != nil && strings.TrimSpace(tracker.parentID) == "" {
			count++
		}
	}
	return count
}

// ThreadIDsWithLiveCodexBackgroundTasks snapshots the threads that currently
// own top-level transient unified-exec tasks. Callers that need project-wide
// availability can take one router lock instead of probing every historical
// thread independently.
func (r *Router) ThreadIDsWithLiveCodexBackgroundTasks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for threadID, state := range r.codexBackground {
		if state == nil {
			continue
		}
		for _, tracker := range state.unifiedExec {
			if tracker != nil && strings.TrimSpace(tracker.parentID) == "" {
				ids = append(ids, threadID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
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
	state := r.codexBackground[evt.ThreadID]
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
	// mergeItemMetaJSON deep-merges maps, so an explicit empty value clears
	// this child logically without requiring a delete sentinel in stored JSON.
	terminalStatuses[childID] = ""
	extra, err := json.Marshal(map[string]any{
		"codex_child_terminal_statuses": terminalStatuses,
		"codex_child_status":            "running",
		"live_background_active":        true,
	})
	if err != nil {
		return err
	}
	launch.item.Meta = mergeItemMetaJSON(launch.item.Meta, extra)
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
	if launch.Kind != itemKindToolCall || launch.ToolName != "collab_agent" || !launch.IsBackground {
		return persistedCodexSpawnLaunch{}, false, nil
	}
	meta := decodeCodexItemMeta(json.RawMessage(launch.Meta))
	if !containsString(meta.ReceiverThreadIDs, childID) {
		return persistedCodexSpawnLaunch{}, false, nil
	}
	return persistedCodexSpawnLaunch{item: launch, meta: meta}, true, nil
}

func (r *Router) observeCodexSpawnChildTerminalInMemory(threadID, launchID string, allTerminal bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.codexBackground[threadID]
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
	terminalStatuses := decodeCodexChildTerminalStatuses(json.RawMessage(launch.Meta))
	terminalStatuses[childID] = status
	allTerminal := allCodexSpawnChildrenTerminal(meta.ReceiverThreadIDs, terminalStatuses)
	aggregateStatus := aggregateCodexSubagentTerminalStatus(meta.ReceiverThreadIDs, terminalStatuses)
	launch.Meta = mergeItemMetaJSON(launch.Meta, codexSubagentTerminalMeta(childID, status, allTerminal, aggregateStatus))
	launch.UpdatedAt = time.Now().UnixMilli()
	return allTerminal, aggregateStatus, r.persistItem(launch, nil)
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

// observeCodexSubagentNotification handles the detached-child closure
// signal: Codex core injects a <subagent_notification> tag into the
// parent's next user message when a backgrounded child finished with no wait
// outstanding. The projector records terminal status on the owning spawn row
// and only synthesizes the transcript sibling once every child in that spawn is
// terminal. When the provider resolved the path to a parent card, evt.ItemID is
// the authoritative launch id; otherwise we fall back to receiverThreadIDs for
// older unnamed-agent builds where agent_path was the receiver thread id.
func (r *Router) observeCodexSubagentNotification(evt provider.ProviderEvent) error {
	parsed := decodeCodexSubagentSignalMeta(evt.Meta)
	if parsed.AgentPath == "" {
		return nil
	}
	threadID := evt.ThreadID
	status := strings.TrimSpace(parsed.Status)
	if status == "" {
		status = "completed"
	}

	launches, err := r.persistedSubagentNotificationLaunches(threadID, strings.TrimSpace(evt.ItemID), parsed.AgentPath)
	if err != nil {
		return err
	}

	var firstErr error
	for _, launch := range launches {
		childID, ok := codexNotificationChildID(launch.meta, parsed.AgentPath)
		if !ok {
			continue
		}
		allTerminal, aggregateStatus, err := r.markCodexSpawnChildTerminal(launch.item, launch.meta, childID, status)
		if err != nil {
			log.Printf("triage: codex-background subagent notification mark %s: %v", launch.item.ID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.observeCodexSpawnChildTerminalInMemory(threadID, launch.item.ID, allTerminal)
		if !allTerminal {
			continue
		}
		r.emitCodexBackgroundTasksChanged(threadID)
		evt := provider.ProviderEvent{
			ThreadID:  threadID,
			ItemID:    launch.item.ID,
			Content:   strings.TrimSpace(parsed.Message),
			Meta:      subagentStatusToItemStatusMeta(aggregateStatus),
			Timestamp: time.Now(),
		}
		if err := r.synthesizeCodexBackgroundCompletion(evt, launch.item.ID, codexBackgroundCompletionOptions{}); err != nil {
			log.Printf("triage: codex-background subagent completion %s: %v", launch.item.ID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func codexNotificationChildID(meta codexItemMeta, agentPath string) (string, bool) {
	agentPath = strings.TrimSpace(agentPath)
	if containsString(meta.ReceiverThreadIDs, agentPath) {
		return agentPath, true
	}
	if len(meta.ReceiverThreadIDs) == 1 {
		childID := strings.TrimSpace(meta.ReceiverThreadIDs[0])
		return childID, childID != ""
	}
	return "", false
}

func (r *Router) persistedSubagentNotificationLaunches(
	threadID string,
	launchID string,
	agentPath string,
) ([]persistedCodexSpawnLaunch, error) {
	if launchID != "" {
		launch, found, err := r.findPersistedCodexSpawnLaunch(threadID, launchID, agentPath, false)
		if err != nil || !found {
			return nil, err
		}
		return []persistedCodexSpawnLaunch{launch}, nil
	}
	launches, err := r.listPersistedCodexSpawnLaunches(threadID)
	if err != nil {
		return nil, err
	}
	out := make([]persistedCodexSpawnLaunch, 0, 1)
	for _, launch := range launches {
		if containsString(launch.meta.ReceiverThreadIDs, agentPath) {
			out = append(out, launch)
		}
	}
	return out, nil
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

	payload := completionPayload(completion.ID, evt, decodeToolCompleteMeta(evt.Meta), now)
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
	waitRow, found, err := r.store.GetThreadItem(evt.ThreadID, nextToolCompletionID(evt.ItemID))
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
