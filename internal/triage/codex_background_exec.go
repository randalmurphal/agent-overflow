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
	parentID     string
	status       string
	exitCode     int
	meta         json.RawMessage
	output       cappedCommandOutput
	createdAt    int64
	updatedAt    int64
}

// codexExecTrackers is the live unified-exec commands of one provider
// process: the root thread's and every agent thread's. It is process
// state (processBound), so a replacement session starts with none.
// Every writer refuses a retired session's frame (frameRetiredLocked).
type codexExecTrackers struct {
	// byLaunch maps the command item id to its tracker.
	byLaunch map[string]*unifiedExecTracker
	// byProcess maps an owner and an app-server process id to the launch
	// id, so TerminalInteraction events correlate without a scan. Each
	// Codex thread numbers its own processes, so a process id names a
	// command only together with its owner: "" for the root thread, the
	// spawn launch id for an agent's thread (tracker.parentID).
	byProcess map[codexProcessKey]string
	// ended maps the item id of a command that left the live work before
	// its item/completed arrived: a Stop found no process, its agent's
	// thread is gone, or a revert cleaned it. Codex can still write the
	// command's output and completion afterwards (it drops an exited
	// process from its table before it writes the end, and a clean answers
	// before the completions it causes), and they are that command's: they
	// are handled as a live command's would be, and the completion removes
	// the entry. An entry whose completion never comes lasts as long as
	// the process; it keeps no output.
	ended map[string]*unifiedExecTracker
}

type codexProcessKey struct {
	owner     string
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

// forOwnFrame returns the tracker that a command's own output or completion
// updates, live or ended, and whether it is live; nil-safe.
func (e *codexExecTrackers) forOwnFrame(launchID string) (tracker *unifiedExecTracker, live bool) {
	if e == nil {
		return nil, false
	}
	if tracker = e.byLaunch[launchID]; tracker != nil {
		return tracker, true
	}
	return e.ended[launchID], false
}

func (e *codexExecTrackers) add(tracker *unifiedExecTracker) {
	e.byLaunch[tracker.launchID] = tracker
	if tracker.processID != "" {
		e.byProcess[codexProcessKey{tracker.parentID, tracker.processID}] = tracker.launchID
	}
}

// byProcessID returns owner's tracker for processID, repairing the index
// from a scan when it is stale.
func (e *codexExecTrackers) byProcessID(owner, processID string) *unifiedExecTracker {
	processID = strings.TrimSpace(processID)
	if e == nil || processID == "" {
		return nil
	}
	key := codexProcessKey{owner, processID}
	if launchID := e.byProcess[key]; launchID != "" {
		if tracker := e.byLaunch[launchID]; tracker != nil && tracker.parentID == owner {
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
		if tracker.parentID == owner && tracker.processID == processID {
			e.byProcess[key] = tracker.launchID
			return tracker
		}
	}
	return nil
}

func (e *codexExecTrackers) firstUnbound(owner string) *unifiedExecTracker {
	for _, tracker := range e.byLaunch {
		if tracker.parentID == owner && tracker.processID == "" {
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
		delete(e.byProcess, codexProcessKey{tracker.parentID, tracker.processID})
	}
	tracker.processID = processID
	e.byProcess[codexProcessKey{tracker.parentID, processID}] = tracker.launchID
}

// remove forgets tracker, live or ended. The process index entry goes only
// when it still names tracker: an ended command's process id can belong to
// a newer command by now.
func (e *codexExecTrackers) remove(tracker *unifiedExecTracker) {
	key := codexProcessKey{tracker.parentID, tracker.processID}
	if tracker.processID != "" && e.byProcess[key] == tracker.launchID {
		delete(e.byProcess, key)
	}
	delete(e.byLaunch, tracker.launchID)
	delete(e.ended, tracker.launchID)
}

// end takes a live tracker out of the live work when its command stopped
// before its item/completed arrived, and keeps it in ended for the frames
// that can still come.
func (e *codexExecTrackers) end(tracker *unifiedExecTracker) {
	e.remove(tracker)
	tracker.output = cappedCommandOutput{}
	if e.ended == nil {
		e.ended = make(map[string]*unifiedExecTracker)
	}
	e.ended[tracker.launchID] = tracker
}

// endOwnedBy ends every live command owner's thread runs and reports how
// many it ended.
func (e *codexExecTrackers) endOwnedBy(owner string) int {
	ended := 0
	for _, tracker := range e.byLaunch {
		if tracker.parentID == owner {
			e.end(tracker)
			ended++
		}
	}
	return ended
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

// markCodexUnifiedExecProcessBackgrounded stamps the command whose process
// a typed write_stdin poll named as backgrounded. The poll comes from the
// thread that owns the process, so evt's parent scope names the owner.
func (r *Router) markCodexUnifiedExecProcessBackgrounded(evt provider.ProviderEvent, processID string) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return
	}
	owner := eventParentID(evt)
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

// codexTerminalForProcess returns a copy of owner's live command for
// processID.
func (r *Router) codexTerminalForProcess(threadID, owner, processID string) (unifiedExecTracker, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tracker := r.codexExecIfPresent(threadID).byProcessID(owner, processID)
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
			tracker = execs.byProcessID(eventParentID(evt), meta.ProcessID)
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
	execs := r.codexExecIfPresent(evt.ThreadID)
	tracker, live := execs.forOwnFrame(itemID)
	if tracker == nil {
		r.mu.Unlock()
		return false
	}
	if r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		r.mu.Unlock()
		return true
	}
	now := eventTimestampMillis(evt)
	if live {
		execs.rebind(tracker, meta.ProcessID)
	}
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
// running. TerminalInteraction owns only explicit stdin interaction marker
// rows.
func (r *Router) observeCodexUnifiedExecComplete(evt provider.ProviderEvent) (bool, error) {
	itemID := strings.TrimSpace(eventItemID(evt))
	if itemID == "" {
		return false, nil
	}

	// Every tool completion of every provider passes here and few are a
	// tracked unified exec, so that is checked before the meta is decoded.
	r.mu.Lock()
	tracked, _ := r.codexExecIfPresent(evt.ThreadID).forOwnFrame(itemID)
	r.mu.Unlock()
	if tracked == nil {
		return false, nil
	}

	command := codexCommandFromMeta(evt.Meta)
	status := codexBackgroundCompletionStatus(evt.Meta)
	exitCode := codexExitCodeFromMeta(evt.Meta)

	var tracker unifiedExecTracker
	handled, wasLive := false, false
	r.mu.Lock()
	// A retired session's completion ends nothing the replacement tracks.
	if r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		r.mu.Unlock()
		return true, nil
	}
	execs := r.codexExecIfPresent(evt.ThreadID)
	now := eventTimestampMillis(evt)
	if own, live := execs.forOwnFrame(itemID); own != nil {
		handled, wasLive = true, live
		own.status = status
		own.exitCode = exitCode
		own.updatedAt = now
		if strings.TrimSpace(command) != "" {
			own.command = command
		}
		own.meta = mergeRawJSONObject(own.meta, evt.Meta)
		if evt.Content != "" {
			own.output.Replace(evt.Content)
		}
		tracker = *own
		execs.remove(own)
	}
	r.mu.Unlock()
	if !handled {
		return false, nil
	}
	if wasLive {
		r.emitCodexBackgroundChanged(evt.ThreadID)
	}
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
