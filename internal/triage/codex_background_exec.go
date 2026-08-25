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

// codex_background_exec.go — the unified-exec half of the Codex background
// projection: the per-process trackers behind the running-task tray, the
// wire-typed backgrounding stamp, the terminal-wait carrier rows, and the
// command history a typed `item/completed` persists.
//
// The authorization rule it implements is invariant 25's first signal: a
// unifiedExec item is tray-visible from its start, but only a typed empty
// `write_stdin` poll against its process makes it a BACKGROUND task. Nothing
// here may infer that from event ordering.
//
// The spawn/subagent half lives in codex_background_subagents.go and its
// codex_background_mailbox.go / codex_background_interactions.go siblings; the
// shared per-thread state, the two tool-lifecycle entry points that dispatch
// into both, and the file-level doc are in codex_background.go.

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

const (
	codexExecResultRunning = "running"
	codexExecResultExited  = "exited"
)

type codexExecResultMeta struct {
	ProcessID string `json:"process_id"`
	Result    string `json:"result"`
	Command   string `json:"command"`
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

// codexLiveUnifiedExecMeta builds the transient tray row's meta. It is an
// ALLOWLIST, not a passthrough of the provider blob: raw Codex item meta
// stays off the wire, and a field earns a place here only when a renderer
// or a user affordance needs it.
//
//   - `source` is the wire-typed background-terminal marker
//     (invariant 25) every consumer branches on.
//   - `command` backs the command row's full-command hover text.
//   - `process_id` is the handle `thread/backgroundTerminals/terminate`
//     joins on, so it is what the tray's per-row Stop button targets.
//     It is empty until the wire names it; the row simply has no stop
//     affordance until then.
func codexLiveUnifiedExecMeta(command, processID string) string {
	meta := map[string]any{
		"source": "unifiedExecStartup",
	}
	if trimmed := strings.TrimSpace(command); trimmed != "" {
		meta["command"] = trimmed
	}
	if trimmed := strings.TrimSpace(processID); trimmed != "" {
		meta["process_id"] = trimmed
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
	if state != nil {
		if wait := state.waitCarrierByProcess[processID]; strings.TrimSpace(wait.itemID) == itemID {
			delete(state.waitCarrierByProcess, processID)
		}
	}
	r.mu.Unlock()
}

func (r *Router) clearCodexPendingTerminalWaitState(threadID, processID, itemID string) {
	processID = strings.TrimSpace(processID)
	itemID = strings.TrimSpace(itemID)
	r.mu.Lock()
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(threadID)
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
	state := r.codexBackgroundIfPresent(evt.ThreadID)
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
	state := r.codexBackgroundIfPresent(evt.ThreadID)
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
	state := r.codexBackgroundIfPresent(evt.ThreadID)
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
	meta := DecodeToolStartMeta(evt.Meta)
	summary := BuildToolCallSummary(meta, evt.ItemType)
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
		Summary:   BuildCompletionSummary(summary, DecodeToolCompleteMeta(evt.Meta)),
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
	state := r.codexBackgroundIfPresent(threadID)
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
			Meta:         codexLiveUnifiedExecMeta(tracker.command, tracker.processID),
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
	state := r.codexBackgroundIfPresent(threadID)
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
	for threadID, st := range r.threads {
		state := st.codexBackground
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
