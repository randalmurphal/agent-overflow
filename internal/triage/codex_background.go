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
// signals today:
//
//   1. CommandExecution.source == "unifiedExecStartup" — a command that
//      the model yielded on while its PTY kept running.
//   2. A collabAgentToolCall spawn_agent whose agentsStates map still
//      reports at least one child thread in a non-terminal state when
//      the parent yields or its turn closes.
//
// The projector sits between the wire signal and the is_background stamp.
// It tracks per-thread inProgress unifiedExec items in transient state
// and spawn_agent launch rows in SQLite. A unifiedExec is shown in the
// running-task tray immediately, but it only becomes a background task
// when a yield signal proves Codex moved on while the PTY kept running.
// Completed background output is attached to the explicit terminal
// wait/poll row. A spawn_agent with running children still stamps its
// persisted launch row is_background=true because that tool call is
// real transcript history.
//
// The correlation is bounded: quick unifiedExec entries are dropped on
// item/completed, completed backgrounded unifiedExec entries are
// retained only long enough for tray visibility / wait-row enrichment,
// completed spawn_agent entries are dropped on wait/subagent
// notification, and all thread state clears on CleanupThread. Nothing
// persists across sessions.
//
// Claude's handleBackgroundTaskTerminal remains the shape-of-truth for
// persisted completion sibling rows. Codex unifiedExec intentionally
// diverges: live/completed state feeds the tray, and command output only
// becomes transcript history on an explicit terminal wait row.

const (
	codexLiveCommandOutputMaxBytes       = 1024 * 1024
	codexCompletedOutputRetentionMillis  = 30 * 60 * 1000
	codexBackgroundTasksChangedEventName = "provider:background_tasks_changed"
)

// unifiedExecTracker is the per-thread per-launchID state tracked for
// Codex unified exec command executions. Unlike Claude background
// tasks, these rows are not timeline history while they are live:
// they first appear only in the running-task tray. If the command
// completes before a yield, it becomes a normal command row. If Codex
// yields while it is still running, the tracker flips to backgrounded
// tray state and later output only renders in chat when the model
// explicitly polls with write_stdin.
type unifiedExecTracker struct {
	backgrounded bool
	completed    bool
	launchID     string
	processID    string
	command      string
	summary      string
	parentID     string
	status       string
	exitCode     int
	output       cappedCommandOutput
	createdAt    int64
	updatedAt    int64
	completedAt  int64
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

// codexBackgroundState holds the per-thread correlation state for
// Codex's background projection. All access is synchronized via
// Router.mu — the per-observer functions each take the lock around
// their map reads/writes. Handle itself does not hold r.mu, so the
// projector is responsible for its own synchronization.
type codexBackgroundState struct {
	// unifiedExec maps launchID → tracker for inProgress unifiedExec
	// command_execution items and recently-completed backgrounded PTYs
	// retained for tray badge / explicit wait-row enrichment.
	unifiedExec map[string]*unifiedExecTracker
	// unifiedExecByProcess maps process_id → launchID so
	// TerminalInteraction events can attach the completed output to the
	// "Waited for background terminal" row without scanning every
	// tracker in the hot path.
	unifiedExecByProcess map[string]string
	// pendingUnifiedExec counts trackers that have not yet been marked
	// backgrounded. Text/thinking deltas are hot-path events, so this
	// prevents repeated scans once every live command has already yielded.
	pendingUnifiedExec int
	// pendingWaitByProcess maps process_id → latest empty-stdin
	// terminal_interaction row waiting on a still-running backgrounded
	// unifiedExec. If the command completes before the next model yield,
	// output attaches to that row. Any later text/thinking/tool-start
	// clears this so old wait rows do not receive ghost completions.
	pendingWaitByProcess map[string]pendingTerminalWait
	// spawnAgent maps launchID → tracker for collabAgentToolCall
	// spawn_agent items that may outlive their parent turn.
	spawnAgent map[string]*spawnAgentTracker
}

func newCodexBackgroundState() *codexBackgroundState {
	return &codexBackgroundState{
		unifiedExec:          make(map[string]*unifiedExecTracker),
		unifiedExecByProcess: make(map[string]string),
		pendingWaitByProcess: make(map[string]pendingTerminalWait),
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

func (r *Router) pruneExpiredCodexCompletedTrackersLocked(state *codexBackgroundState, now int64) bool {
	if state == nil {
		return false
	}
	cutoff := now - codexCompletedOutputRetentionMillis
	changed := false
	for id, tracker := range state.unifiedExec {
		if tracker == nil || !tracker.completed || tracker.completedAt >= cutoff {
			continue
		}
		if tracker.processID != "" {
			delete(state.unifiedExecByProcess, tracker.processID)
			delete(state.pendingWaitByProcess, tracker.processID)
		}
		delete(state.unifiedExec, id)
		changed = true
	}
	return changed
}

func (r *Router) scheduleCodexCompletedTrackerPrune(threadID, launchID string) {
	time.AfterFunc(time.Duration(codexCompletedOutputRetentionMillis)*time.Millisecond, func() {
		pruned := false
		now := time.Now().UnixMilli()
		r.mu.Lock()
		state := r.codexBackground[threadID]
		if state != nil {
			tracker := state.unifiedExec[launchID]
			if tracker != nil && tracker.completed && tracker.completedAt <= now-codexCompletedOutputRetentionMillis {
				if tracker.processID != "" {
					delete(state.unifiedExecByProcess, tracker.processID)
					delete(state.pendingWaitByProcess, tracker.processID)
				}
				delete(state.unifiedExec, launchID)
				pruned = true
			}
		}
		r.mu.Unlock()
		if pruned {
			r.emitCodexBackgroundTasksChanged(threadID)
		}
	})
}

func (r *Router) markCodexUnifiedExecBackgrounded(threadID, excludeItemID string) {
	changed := false
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		if state.pendingUnifiedExec == 0 {
			r.mu.Unlock()
			return
		}
		for id, tracker := range state.unifiedExec {
			if id == excludeItemID || tracker.backgrounded || tracker.completed {
				continue
			}
			tracker.backgrounded = true
			tracker.updatedAt = time.Now().UnixMilli()
			if state.pendingUnifiedExec > 0 {
				state.pendingUnifiedExec--
			}
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.emitCodexBackgroundTasksChanged(threadID)
	}
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
//   - collabAgentToolCall spawn_agent: tracked so item/completed can stamp
//     the persisted row backgrounded if agentsStates reports a running
//     child. The spawn row itself closes on the wire immediately.
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

	// A subsequent tool call means the model has moved past earlier
	// unified exec calls. Mark existing live commands as backgrounded
	// before registering the new one; this avoids treating "sleep 15
	// yielded, then run another command" as a quick synchronous command
	// just because no assistant text streamed between the calls.
	r.clearCodexPendingTerminalWaits(evt.ThreadID)
	r.markCodexUnifiedExecBackgrounded(evt.ThreadID, itemID)

	emitChanged := false
	r.mu.Lock()
	state := r.codexBackgroundForThread(evt.ThreadID)
	now := eventTimestampMillis(evt)
	if r.pruneExpiredCodexCompletedTrackersLocked(state, now) {
		emitChanged = true
	}

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
			createdAt: now,
			updatedAt: now,
		}
		if meta.ProcessID != "" {
			state.unifiedExecByProcess[meta.ProcessID] = itemID
		}
		state.pendingUnifiedExec++
		emitChanged = true
		r.mu.Unlock()
		if emitChanged {
			r.emitCodexBackgroundTasksChanged(evt.ThreadID)
		}
		return true
	case isSpawnAgentCandidate && meta.Tool == "spawn_agent":
		// Spawn_agent rows rarely stay inProgress — the item/started event
		// is a very short-lived marker before the immediate completed
		// envelope. We stamp on the completed envelope instead (see
		// observeCodexToolComplete); tracking here just establishes the
		// tracker so the later refresh can attach the running-children
		// flag and receiver ids.
		if _, ok := state.spawnAgent[itemID]; ok {
			r.mu.Unlock()
			if emitChanged {
				r.emitCodexBackgroundTasksChanged(evt.ThreadID)
			}
			return false
		}
		state.spawnAgent[itemID] = &spawnAgentTracker{}
	}
	r.mu.Unlock()
	if emitChanged {
		r.emitCodexBackgroundTasksChanged(evt.ThreadID)
	}
	return false
}

func (r *Router) clearCodexPendingTerminalWaits(threadID string) {
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil && len(state.pendingWaitByProcess) > 0 {
		state.pendingWaitByProcess = make(map[string]pendingTerminalWait)
	}
	r.mu.Unlock()
}

func (r *Router) trackCodexPendingTerminalWait(threadID, processID, itemID string, turnIndex int, createdAt int64) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return
	}
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	launchID := state.unifiedExecByProcess[processID]
	tracker := state.unifiedExec[launchID]
	if tracker != nil && tracker.backgrounded && !tracker.completed {
		state.pendingWaitByProcess[processID] = pendingTerminalWait{
			itemID:    itemID,
			turnIndex: turnIndex,
			createdAt: createdAt,
		}
	} else {
		for _, candidate := range state.unifiedExec {
			if candidate != nil && candidate.processID == "" && candidate.backgrounded && !candidate.completed {
				state.pendingWaitByProcess[processID] = pendingTerminalWait{
					itemID:    itemID,
					turnIndex: turnIndex,
					createdAt: createdAt,
				}
				break
			}
		}
	}
	r.mu.Unlock()
}

func rebindCodexUnifiedExecProcessLocked(state *codexBackgroundState, tracker *unifiedExecTracker, processID string) {
	processID = strings.TrimSpace(processID)
	if state == nil || tracker == nil || processID == "" || tracker.processID == processID {
		return
	}
	if tracker.processID != "" {
		delete(state.unifiedExecByProcess, tracker.processID)
		delete(state.pendingWaitByProcess, tracker.processID)
	}
	tracker.processID = processID
	state.unifiedExecByProcess[processID] = tracker.launchID
}

// observeCodexCommandOutput buffers output for live Codex unified exec
// commands and prevents the generic command-output path from creating
// ghost timeline rows for background PTYs.
func (r *Router) observeCodexCommandOutput(evt provider.ProviderEvent) bool {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false
	}
	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	if state == nil {
		r.mu.Unlock()
		return false
	}
	now := eventTimestampMillis(evt)
	pruned := r.pruneExpiredCodexCompletedTrackersLocked(state, now)
	tracker := state.unifiedExec[itemID]
	if tracker == nil {
		r.mu.Unlock()
		if pruned {
			r.emitCodexBackgroundTasksChanged(evt.ThreadID)
		}
		return false
	}
	meta := decodeCodexItemMeta(evt.Meta)
	rebindCodexUnifiedExecProcessLocked(state, tracker, meta.ProcessID)
	if evt.Replace {
		tracker.output.Replace(evt.Content)
	} else {
		tracker.output.Append(evt.Content)
	}
	if command := codexCommandFromMeta(evt.Meta); strings.TrimSpace(command) != "" {
		tracker.command = command
	}
	tracker.updatedAt = now
	r.mu.Unlock()
	if pruned {
		r.emitCodexBackgroundTasksChanged(evt.ThreadID)
	}
	return true
}

func (r *Router) observeCodexTopLevelToolBoundary(evt provider.ProviderEvent) {
	if strings.TrimSpace(evt.ParentToolUseID) != "" {
		return
	}
	// A top-level tool after a terminal poll means the model moved on from
	// that poll. If the background PTY completes later, keep it in the tray
	// until the model explicitly polls again instead of mutating the stale
	// "waited" row.
	r.clearCodexPendingTerminalWaits(evt.ThreadID)
}

// observeCodexUnifiedExecComplete owns item/completed for tracked
// unified exec startups. Quick commands that completed before the model
// moved on become normal command rows. Commands that yielded stay
// tray-only until an explicit terminal wait/poll surfaces their output.
func (r *Router) observeCodexUnifiedExecComplete(evt provider.ProviderEvent) (bool, error) {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false, nil
	}

	var tracker unifiedExecTracker
	var pendingWait pendingTerminalWait
	handled := false
	backgrounded := false
	hasPendingWait := false
	r.mu.Lock()
	state := r.codexBackground[evt.ThreadID]
	now := eventTimestampMillis(evt)
	pruned := false
	if state != nil {
		pruned = r.pruneExpiredCodexCompletedTrackersLocked(state, now)
	}
	if state != nil && state.unifiedExec[itemID] != nil {
		live := state.unifiedExec[itemID]
		handled = true
		backgrounded = live.backgrounded
		live.completed = true
		live.status = codexBackgroundCompletionStatus(evt.Meta)
		live.exitCode = codexExitCodeFromMeta(evt.Meta)
		live.completedAt = now
		live.updatedAt = now
		if command := codexCommandFromMeta(evt.Meta); strings.TrimSpace(command) != "" {
			live.command = command
		}
		meta := decodeCodexItemMeta(evt.Meta)
		rebindCodexUnifiedExecProcessLocked(state, live, meta.ProcessID)
		if evt.Content != "" {
			live.output.Replace(evt.Content)
		}
		tracker = *live
		if !live.backgrounded {
			if live.processID != "" {
				delete(state.unifiedExecByProcess, live.processID)
				delete(state.pendingWaitByProcess, live.processID)
			}
			if state.pendingUnifiedExec > 0 {
				state.pendingUnifiedExec--
			}
			delete(state.unifiedExec, itemID)
		} else {
			if live.processID != "" {
				if wait, ok := state.pendingWaitByProcess[live.processID]; ok {
					pendingWait = wait
					hasPendingWait = true
				}
			}
			r.scheduleCodexCompletedTrackerPrune(evt.ThreadID, itemID)
		}
	}
	r.mu.Unlock()
	if !handled {
		if pruned {
			r.emitCodexBackgroundTasksChanged(evt.ThreadID)
		}
		return false, nil
	}
	if backgrounded {
		if hasPendingWait {
			if err := r.attachCodexCompletionToPendingWait(evt, tracker, pendingWait); err != nil {
				return true, err
			}
			r.clearCodexCompletedOutputTracker(evt.ThreadID, tracker.processID)
			return true, nil
		}
		r.emitCodexBackgroundTasksChanged(evt.ThreadID)
		return true, nil
	}
	r.emitCodexBackgroundTasksChanged(evt.ThreadID)
	return true, r.persistQuickCodexCommand(evt, tracker)
}

func (r *Router) persistQuickCodexCommand(evt provider.ProviderEvent, tracker unifiedExecTracker) error {
	meta := decodeToolStartMeta(evt.Meta)
	summary := buildToolCallSummary(meta, evt.ItemType)
	if strings.TrimSpace(summary) == "" {
		summary = tracker.summary
	}
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("codex quick command turn index %s: %w", tracker.launchID, err)
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

func codexCompletedOutputPayloadFromTracker(snapshot unifiedExecTracker, itemID string, fallbackMeta json.RawMessage, now int64) (*store.Payload, string) {
	output := snapshot.output.String()
	metaEvt := provider.ProviderEvent{
		Content: output,
		Meta:    fallbackMeta,
	}
	if snapshot.command != "" || snapshot.exitCode != 0 {
		metaJSON, err := json.Marshal(map[string]any{
			"command":   snapshot.command,
			"exitCode":  snapshot.exitCode,
			"exit_code": snapshot.exitCode,
		})
		if err == nil {
			metaEvt.Meta = metaJSON
		}
	}
	payload := &store.Payload{
		ID:        "command-output:" + itemID,
		Kind:      "command_output",
		Meta:      buildPayloadMeta("command_output", metaEvt),
		Data:      []byte(output),
		CreatedAt: now,
	}
	summary := "Waited for background terminal"
	if snapshot.summary != "" {
		summary = summary + ": " + snapshot.summary
	}
	return payload, summary
}

func (r *Router) attachCodexCompletionToPendingWait(evt provider.ProviderEvent, tracker unifiedExecTracker, wait pendingTerminalWait) error {
	item, found, err := r.store.GetThreadItem(evt.ThreadID, wait.itemID)
	if err != nil {
		return fmt.Errorf("codex pending wait lookup %s: %w", wait.itemID, err)
	}
	if !found {
		return fmt.Errorf("codex pending wait %s disappeared before completion attach", wait.itemID)
	}
	if item.Kind != string(provider.ItemTerminalInteraction) {
		return fmt.Errorf("codex pending wait %s kind = %q, want terminal_interaction", wait.itemID, item.Kind)
	}
	now := eventTimestampMillis(evt)
	payload, summary := codexCompletedOutputPayloadFromTracker(tracker, item.ID, evt.Meta, now)
	item.Status = statusCompleted
	item.Summary = summary
	item.UpdatedAt = now
	return r.persistItem(item, payload)
}

func (r *Router) codexCompletedOutputPayloadForProcess(threadID, processID, itemID string, fallbackMeta json.RawMessage, now int64) (*store.Payload, string) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return nil, ""
	}
	var tracker *unifiedExecTracker
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		if launchID := state.unifiedExecByProcess[processID]; launchID != "" {
			tracker = state.unifiedExec[launchID]
		}
	}
	var snapshot unifiedExecTracker
	if tracker != nil {
		snapshot = *tracker
	}
	r.mu.Unlock()
	if tracker == nil || !snapshot.completed || !snapshot.backgrounded {
		return nil, ""
	}
	return codexCompletedOutputPayloadFromTracker(snapshot, itemID, fallbackMeta, now)
}

func (r *Router) clearCodexCompletedOutputTracker(threadID, processID string) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return
	}
	cleared := false
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state != nil {
		launchID := state.unifiedExecByProcess[processID]
		tracker := state.unifiedExec[launchID]
		if tracker != nil && tracker.completed && tracker.backgrounded {
			delete(state.unifiedExecByProcess, processID)
			delete(state.pendingWaitByProcess, processID)
			delete(state.unifiedExec, launchID)
			cleared = true
		}
	}
	r.mu.Unlock()
	if cleared {
		r.emitCodexBackgroundTasksChanged(threadID)
	}
}

// ListLiveCodexBackgroundTasks returns transient Codex unified exec tray
// rows. Pending foreground commands are included with IsBackground=false;
// yielded PTYs are included with IsBackground=true. These are
// intentionally not persisted timeline items.
func (r *Router) ListLiveCodexBackgroundTasks(threadID string, nowMillis, retentionCutoffMillis int64) []store.Item {
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
		if tracker.completed && tracker.completedAt < nowMillis-codexCompletedOutputRetentionMillis {
			continue
		}
		if tracker.completed && tracker.completedAt < retentionCutoffMillis {
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

	items := make([]store.Item, 0, len(trackers)*2)
	for _, tracker := range trackers {
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
			Meta:         `{"source":"unifiedExecStartup"}`,
			CreatedAt:    tracker.createdAt,
			UpdatedAt:    tracker.updatedAt,
		}
		items = append(items, launch)
		if tracker.completed {
			completionMeta, _ := json.Marshal(map[string]any{"item_status": tracker.status})
			items = append(items, store.Item{
				ID:           nextToolCompletionID(tracker.launchID),
				ThreadID:     threadID,
				TurnIndex:    0,
				ItemIndex:    1,
				Kind:         itemKindBackgroundDone,
				Role:         "assistant",
				Status:       tracker.status,
				Summary:      buildCodexBackgroundCompletionSummary(tracker.summary, completionMeta),
				IsBackground: true,
				CompletionOf: tracker.launchID,
				ToolName:     "command_execution",
				CreatedAt:    tracker.completedAt,
				UpdatedAt:    tracker.completedAt,
			})
		}
	}
	return items
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
	r.clearCodexPendingTerminalWaits(threadID)
	r.markCodexUnifiedExecBackgrounded(threadID, "")
}

// observeCodexTurnComplete is the unifiedExec catchall. If a tracked
// command stayed inProgress through the turn boundary, Codex has yielded
// while the PTY is still running, so the command becomes live tray state.
func (r *Router) observeCodexTurnComplete(threadID string) {
	r.mu.Lock()
	state := r.codexBackground[threadID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	if state.pendingUnifiedExec == 0 {
		r.mu.Unlock()
		return
	}
	unifiedChanged := false
	for _, tracker := range state.unifiedExec {
		if tracker.backgrounded || tracker.completed {
			continue
		}
		tracker.backgrounded = true
		if state.pendingUnifiedExec > 0 {
			state.pendingUnifiedExec--
		}
		unifiedChanged = true
	}
	r.mu.Unlock()
	if unifiedChanged {
		r.emitCodexBackgroundTasksChanged(threadID)
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
