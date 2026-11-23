package triage

import (
	"encoding/json"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// codex_background_exec.go — the unified-exec half of the Codex background
// projection: the per-process trackers behind the running-task tray, the
// wire-typed backgrounding stamp and the
// command history a typed `item/completed` persists.
//
// The authorization rule it implements is invariant 25's first signal: a
// unifiedExec item is tray-visible from its start, but only a typed empty
// `write_stdin` poll against its process makes it a BACKGROUND task. Nothing
// here may infer that from event ordering.
//
// The spawn/subagent half lives in codex_background_subagents.go and its
// codex_background_mailbox.go / codex_background_activity.go siblings; the
// shared per-thread state, the two tool-lifecycle entry points that dispatch
// into both, and the file-level doc are in codex_background.go.

// unifiedExecTracker is the per-thread per-launchID state tracked for
// Codex unified exec command executions. Unlike Claude background
// tasks, these rows are not timeline history while they are live:
// they first appear only in the running-task tray. Typed item/completed removes
// the tracker; it persists the normal command row only while a Codex wire round
// is still active. Empty write_stdin polls update background process state only.
type unifiedExecTracker struct {
	backgrounded bool
	launchID     string
	processID    string
	command      string
	summary      string
	// parentID is the command's display parent: the spawn launch of the
	// agent that ran it, or the review launch. Where its process runs is
	// scope, which is not the same thing.
	parentID string
	// scope is the process table that holds the command, as the provider
	// named it when the command started (provider.ProviderEvent.ProcessScope).
	scope provider.ProcessScope
	// seq orders the thread's command starts, so a check of a scope applies
	// only to the commands that had started when it was asked
	// (CodexScopeCheck).
	seq       uint64
	status    string
	exitCode  int
	meta      json.RawMessage
	output    cappedCommandOutput
	createdAt int64
	updatedAt int64
}

// endedCodexCommand is what a command that left the live work before its
// own item/completed keeps for that completion, which Codex can still
// write: a process the model polled after it exited leaves Codex's table
// before its end is written (core unified_exec process_manager.rs
// refresh_process_state, async_watcher.rs). It holds what the command's
// history row needs and nothing more; output is only the completion's own
// aggregated output, between its replace frame and the completion.
type endedCodexCommand struct {
	launchID  string
	parentID  string
	summary   string
	createdAt int64
	output    cappedCommandOutput
}

// codexExecTrackers is the unified-exec commands of one provider process:
// the root thread's, every agent thread's and a review run's. It is process
// state (processBound), so a replacement session starts with none. Every
// writer refuses a retired session's frame (frameRetiredLocked).
//
// A command leaves the live work in exactly four ways, all in this file:
//
//  1. its own item/completed (observeCodexUnifiedExecComplete);
//  2. its process table ends: the thread's notLoaded, or the review run's
//     end (endCodexScope);
//  3. the session ends or is replaced (processBound);
//  4. Codex confirms it absent: the table's list, asked in this session
//     after the command started, does not hold it, or says the thread is
//     not loaded (ConfirmCodexScope).
//
// A termination's answer, a close item and a clean's acknowledgement are
// not among them: none of them says the process is gone.
type codexExecTrackers struct {
	// byLaunch maps the command item id to its live tracker.
	byLaunch map[string]*unifiedExecTracker
	// byProcess maps a scope and an app-server process id to the launch
	// id, so TerminalInteraction events correlate without a scan. Each
	// process table numbers its own processes, so a process id names a
	// command only within its scope.
	byProcess map[codexProcessKey]string
	// ended holds the commands that left the live work before their own
	// item/completed, by item id, until it comes or the session ends.
	ended map[string]*endedCodexCommand
	// lastSeq is the seq of the latest command start.
	lastSeq uint64
}

type codexProcessKey struct {
	scope     string
	processID string
}

// codexExecIfPresent returns the thread's commands, or nil when it has
// none, live or ended, without allocating. Caller holds r.mu.
func (r *Router) codexExecIfPresent(threadID string) *codexExecTrackers {
	st := r.threadStateIfPresent(threadID)
	if st == nil || len(st.codexExec.byLaunch) == 0 && len(st.codexExec.ended) == 0 {
		return nil
	}
	return &st.codexExec
}

// codexExecForThread returns the thread's live commands, creating the maps.
// Caller holds r.mu.
func (r *Router) codexExecForThread(threadID string) *codexExecTrackers {
	execs := &r.state(threadID).codexExec
	if execs.byLaunch == nil {
		execs.byLaunch = make(map[string]*unifiedExecTracker)
		execs.byProcess = make(map[codexProcessKey]string)
	}
	return execs
}

// lookup returns the live tracker for a command item id; nil-safe.
func (e *codexExecTrackers) lookup(launchID string) *unifiedExecTracker {
	if e == nil {
		return nil
	}
	return e.byLaunch[launchID]
}

// endedFor returns the ended record for a command item id; nil-safe.
func (e *codexExecTrackers) endedFor(launchID string) *endedCodexCommand {
	if e == nil {
		return nil
	}
	return e.ended[launchID]
}

func (e *codexExecTrackers) add(tracker *unifiedExecTracker) {
	e.lastSeq++
	tracker.seq = e.lastSeq
	e.byLaunch[tracker.launchID] = tracker
	if tracker.processID != "" {
		e.byProcess[codexProcessKey{tracker.scope.ID, tracker.processID}] = tracker.launchID
	}
}

// byProcessID returns scope's tracker for processID, repairing the index
// from a scan when it is stale.
func (e *codexExecTrackers) byProcessID(scope, processID string) *unifiedExecTracker {
	processID = strings.TrimSpace(processID)
	if e == nil || processID == "" {
		return nil
	}
	key := codexProcessKey{scope, processID}
	if launchID := e.byProcess[key]; launchID != "" {
		if tracker := e.byLaunch[launchID]; tracker != nil && tracker.scope.ID == scope {
			if tracker.processID == "" {
				tracker.processID = processID
			}
			if tracker.processID == processID {
				return tracker
			}
		}
		delete(e.byProcess, key)
	}
	for _, tracker := range e.byLaunch {
		if tracker.scope.ID == scope && tracker.processID == processID {
			e.byProcess[key] = tracker.launchID
			return tracker
		}
	}
	return nil
}

func (e *codexExecTrackers) firstUnbound(scope string) *unifiedExecTracker {
	for _, tracker := range e.byLaunch {
		if tracker.scope.ID == scope && tracker.processID == "" {
			return tracker
		}
	}
	return nil
}

func (e *codexExecTrackers) rebind(tracker *unifiedExecTracker, processID string) {
	processID = strings.TrimSpace(processID)
	if tracker == nil || processID == "" || tracker.processID == processID {
		return
	}
	if tracker.processID != "" {
		delete(e.byProcess, codexProcessKey{tracker.scope.ID, tracker.processID})
	}
	tracker.processID = processID
	e.byProcess[codexProcessKey{tracker.scope.ID, processID}] = tracker.launchID
}

// remove forgets a live tracker.
func (e *codexExecTrackers) remove(tracker *unifiedExecTracker) {
	key := codexProcessKey{tracker.scope.ID, tracker.processID}
	if tracker.processID != "" && e.byProcess[key] == tracker.launchID {
		delete(e.byProcess, key)
	}
	delete(e.byLaunch, tracker.launchID)
}

// end takes a live tracker out of the live work before its own
// item/completed and keeps the record that completion needs.
func (e *codexExecTrackers) end(tracker *unifiedExecTracker) {
	e.remove(tracker)
	if e.ended == nil {
		e.ended = make(map[string]*endedCodexCommand)
	}
	e.ended[tracker.launchID] = &endedCodexCommand{
		launchID:  tracker.launchID,
		parentID:  tracker.parentID,
		summary:   tracker.summary,
		createdAt: tracker.createdAt,
	}
}

// endScope ends every live command in scope that started at or before
// through (0: all of them) and returns their item ids.
func (e *codexExecTrackers) endScope(scope string, through uint64) []string {
	var ended []string
	for _, tracker := range e.byLaunch {
		if tracker.scope.ID == scope && (through == 0 || tracker.seq <= through) {
			ended = append(ended, tracker.launchID)
			e.end(tracker)
		}
	}
	return ended
}

// endCodexScope applies way 2: a frame reporting that scope's process table
// ended (a thread's notLoaded, a review run's end) ends every command in it.
// The frame comes in wire order after every start the table had, so all of
// them end.
func (r *Router) endCodexScope(evt provider.ProviderEvent) {
	scope := strings.TrimSpace(evt.ProcessScope.ID)
	if scope == "" {
		return
	}
	r.mu.Lock()
	var ended []string
	if execs := r.codexExecIfPresent(evt.ThreadID); execs != nil && !r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		ended = execs.endScope(scope, 0)
	}
	r.mu.Unlock()
	if len(ended) > 0 {
		r.emitCodexBackgroundChanged(evt.ThreadID)
	}
}

type cappedCommandOutput struct {
	data  []byte
	ring  bool
	start int
	size  int
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
//   - `stop_by_item` (codexCommandStopKey) marks a command whose Stop
//     names it by item id; the app resolves where its process runs. A
//     command in a table no request can reach (a review run's) has no
//     Stop and ends with its run, and a command whose process the wire has
//     not named yet has no Stop until it does.
//   - `process_id` (codexCommandProcessIDKey) is the root thread's process
//     handle, for a window that stops a row by process id. Only a root
//     command carries it: an agent's process id names nothing in the root
//     thread's table, which is the only one such a window can address.
func codexLiveUnifiedExecMeta(tracker unifiedExecTracker) string {
	meta := map[string]any{
		"source": "unifiedExecStartup",
	}
	if trimmed := strings.TrimSpace(tracker.command); trimmed != "" {
		meta["command"] = trimmed
	}
	if processID := strings.TrimSpace(tracker.processID); processID != "" {
		if tracker.scope.Addressable && tracker.scope.ID != "" {
			meta[codexCommandStopKey] = true
		}
		if tracker.parentID == "" {
			meta[codexCommandProcessIDKey] = processID
		}
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

// markCodexUnifiedExecProcessBackgrounded stamps the command whose process
// a typed write_stdin poll named as backgrounded. The poll comes from the
// process table that holds the process, which evt's scope names.
func (r *Router) markCodexUnifiedExecProcessBackgrounded(evt provider.ProviderEvent, processID string) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return
	}
	owner := evt.ProcessScope.ID
	changed := false
	r.mu.Lock()
	if execs := r.codexExecIfPresent(evt.ThreadID); execs != nil && !r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		tracker := execs.byProcessID(owner, processID)
		if tracker == nil {
			if tracker = execs.firstUnbound(owner); tracker != nil {
				execs.rebind(tracker, processID)
			}
		}
		if tracker != nil && !tracker.backgrounded {
			tracker.backgrounded = true
			tracker.updatedAt = time.Now().UnixMilli()
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.emitCodexBackgroundChanged(evt.ThreadID)
	}
}

// codexTerminalForProcess returns a copy of scope's live command for
// processID.
func (r *Router) codexTerminalForProcess(threadID, scope, processID string) (unifiedExecTracker, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tracker := r.codexExecIfPresent(threadID).byProcessID(scope, processID)
	if tracker == nil {
		return unifiedExecTracker{}, false
	}
	return *tracker, true
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
	if execs := r.codexExecIfPresent(evt.ThreadID); execs != nil && !r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		tracker := execs.byLaunch[itemID]
		if tracker == nil && meta.ProcessID != "" {
			tracker = execs.byProcessID(evt.ProcessScope.ID, meta.ProcessID)
		}
		if tracker != nil {
			execs.rebind(tracker, meta.ProcessID)
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

// observeCodexCommandOutput buffers output for Codex unified exec commands
// and prevents the generic command-output path from creating ghost timeline
// rows for background PTYs. An ended command keeps only its completion's
// aggregated output, the replace frame Codex writes just before it.
func (r *Router) observeCodexCommandOutput(evt provider.ProviderEvent) bool {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false
	}
	meta := decodeCodexItemMeta(evt.Meta)
	command := codexCommandFromMeta(evt.Meta)
	r.mu.Lock()
	defer r.mu.Unlock()
	execs := r.codexExecIfPresent(evt.ThreadID)
	tracker := execs.lookup(itemID)
	ended := execs.endedFor(itemID)
	if tracker == nil && ended == nil {
		return false
	}
	if r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		return true
	}
	if ended != nil {
		if evt.Replace {
			ended.output.Replace(evt.Content)
		}
		return true
	}
	execs.rebind(tracker, meta.ProcessID)
	if evt.Replace {
		tracker.output.Replace(evt.Content)
	} else {
		tracker.output.Append(evt.Content)
	}
	if strings.TrimSpace(command) != "" {
		tracker.command = command
	}
	tracker.updatedAt = eventTimestampMillis(evt)
	return true
}

// observeCodexUnifiedExecComplete owns item/completed for tracked unified exec
// startups, live or ended. This mirrors Codex TUI: late completions still
// clear background process state, but chat history only changes while the
// task indicator is running. TerminalInteraction owns only explicit stdin
// interaction marker rows.
func (r *Router) observeCodexUnifiedExecComplete(evt provider.ProviderEvent) (bool, error) {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false, nil
	}

	// Every tool completion of every provider passes here and few are a
	// Codex command, so that is checked before the meta is decoded.
	r.mu.Lock()
	execs := r.codexExecIfPresent(evt.ThreadID)
	tracked := execs.lookup(itemID) != nil || execs.endedFor(itemID) != nil
	r.mu.Unlock()
	if !tracked {
		return false, nil
	}

	command := codexCommandFromMeta(evt.Meta)
	status := codexBackgroundCompletionStatus(evt.Meta)
	exitCode := codexExitCodeFromMeta(evt.Meta)

	var completed unifiedExecTracker
	handled, wasLive := false, false
	r.mu.Lock()
	// A retired session's completion ends nothing the replacement tracks.
	if r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		r.mu.Unlock()
		return true, nil
	}
	execs = r.codexExecIfPresent(evt.ThreadID)
	if live := execs.lookup(itemID); live != nil {
		handled, wasLive = true, true
		completed = *live
		completed.meta = mergeRawJSONObject(live.meta, evt.Meta)
		execs.remove(live)
	} else if ended := execs.endedFor(itemID); ended != nil {
		handled = true
		completed = unifiedExecTracker{
			launchID: ended.launchID, parentID: ended.parentID, summary: ended.summary,
			createdAt: ended.createdAt, output: ended.output, meta: evt.Meta,
		}
		delete(execs.ended, itemID)
	}
	r.mu.Unlock()
	if !handled {
		return false, nil
	}
	completed.status = status
	completed.exitCode = exitCode
	completed.updatedAt = eventTimestampMillis(evt)
	if strings.TrimSpace(command) != "" {
		completed.command = command
	}
	if evt.Content != "" {
		completed.output.Replace(evt.Content)
	}
	if wasLive {
		r.emitCodexBackgroundChanged(evt.ThreadID)
	}
	turnIndex, ok := r.activeRoundTurnIndex(evt.ThreadID)
	if !ok {
		return true, nil
	}
	return true, r.persistCodexUnifiedExecCommand(evt, completed, turnIndex)
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
