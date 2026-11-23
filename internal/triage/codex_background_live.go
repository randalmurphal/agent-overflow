package triage

import (
	"encoding/json"
	"sort"
	"strings"

	"agent-overflow/internal/store"
)

// Codex live work is the background work only this router holds: every
// agent whose execution is active, and every unified-exec command the
// session's app-server runs, the root thread's, every agent's at every
// depth and a review run's. A command outlives the turn that started it and
// leaves the live work only by the four ways codexExecTrackers names, so
// closing the session would kill it. The count, the tray and the
// project-wide thread list below all apply this rule; the tray lists the
// agents through ListLiveCodexAgentTasks.
//
// The queue gate is the one deliberate difference: it counts top-level
// commands only (hasActiveCodexTopLevelCommand). A queued send waits for
// the main agent's work. An agent's command is that agent's work, and the
// agent's active execution already holds the queue while it runs; a command
// left behind by an agent that finished is not something the next prompt
// waits on. Claude's queue gate draws the same line
// (store.HasQueueBlockingBackgroundToolCall).

// Tray row meta keys for a Codex command. codexCommandStopKey marks a row
// whose Stop names the command by its item id (StopCodexBackgroundCommand
// in the app); codexCommandProcessIDKey is the app-server process id, which
// only a root command's row carries, for a window that stops by process id.
const (
	codexCommandStopKey      = "stop_by_item"
	codexCommandProcessIDKey = "process_id"
)

// CodexCommandStopsByItem reports whether a Codex command's tray row has a
// Stop that names it by item id.
func CodexCommandStopsByItem(item store.Item) bool {
	var meta struct {
		Stop bool `json:"stop_by_item"`
	}
	return json.Unmarshal([]byte(item.Meta), &meta) == nil && meta.Stop
}

// ListLiveCodexBackgroundTasks returns transient Codex unified exec tray
// rows, an agent's commands parented to its spawn launch. Pending
// foreground commands are included with IsBackground=false; yielded PTYs
// are included with IsBackground=true. Completed commands leave the live
// tray when typed item/completed removes the transient tracker.
func (r *Router) ListLiveCodexBackgroundTasks(threadID string, _ int64, _ int64) []store.Item {
	r.mu.Lock()
	execs := r.codexExecIfPresent(threadID)
	if execs == nil || len(execs.byLaunch) == 0 {
		r.mu.Unlock()
		return nil
	}
	trackers := make([]unifiedExecTracker, 0, len(execs.byLaunch))
	for _, tracker := range execs.byLaunch {
		copied := *tracker
		copied.meta, copied.output = nil, cappedCommandOutput{}
		trackers = append(trackers, copied)
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
			Meta:         codexLiveUnifiedExecMeta(tracker),
			CreatedAt:    tracker.createdAt,
			UpdatedAt:    tracker.updatedAt,
		}
		items = append(items, launch)
	}
	return items
}

// CountLiveCodexBackgroundTasks counts the thread's Codex live work.
func (r *Router) CountLiveCodexBackgroundTasks(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return codexLiveWorkLocked(r.threadStateIfPresent(threadID), false)
}

// codexLiveWorkLocked is the Codex live-work rule for one thread: its live
// commands plus its active agents. With any true it stops at the first
// unit found, for a caller that only asks whether there is any. Caller
// holds r.mu.
func codexLiveWorkLocked(st *threadState, any bool) int {
	if st == nil {
		return 0
	}
	count := len(st.codexExec.byLaunch)
	if any && count > 0 {
		return count
	}
	if st.codexBackground != nil {
		for _, item := range st.codexBackground.agents {
			if codexRuntimeActive(item) {
				count++
				if any {
					return count
				}
			}
		}
	}
	return count
}

// ThreadIDsWithLiveCodexBackgroundTasks snapshots the threads that hold
// Codex live work. Project-wide availability can take one router lock
// instead of probing every historical thread independently.
func (r *Router) ThreadIDsWithLiveCodexBackgroundTasks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for threadID, st := range r.threads {
		if codexLiveWorkLocked(st, true) > 0 {
			ids = append(ids, threadID)
		}
	}
	sort.Strings(ids)
	return ids
}

// CodexLiveWork reports whether the thread holds Codex live work, with the
// summary of one unit of it for a reader that names what it waits on: the
// first row the tray lists, the earliest live command, else the earliest
// active agent (ListLiveCodexAgentTasks's order). It copies nothing.
func (r *Router) CodexLiveWork(threadID string) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false, ""
	}
	var first *unifiedExecTracker
	for _, tracker := range st.codexExec.byLaunch {
		if first == nil || tracker.createdAt < first.createdAt ||
			tracker.createdAt == first.createdAt && tracker.launchID < first.launchID {
			first = tracker
		}
	}
	if first != nil {
		return true, firstNonEmptyString(first.summary, "command_execution")
	}
	if st.codexBackground == nil {
		return false, ""
	}
	var agent store.Item
	var agentStart int64
	found := false
	for _, item := range st.codexBackground.agents {
		if !codexRuntimeActive(item) {
			continue
		}
		start := codexAgentStartedAt(item)
		if !found || start < agentStart || start == agentStart && item.ID < agent.ID {
			agent, agentStart, found = item, start, true
		}
	}
	if !found {
		return false, ""
	}
	return true, firstNonEmptyString(agent.Summary, agent.ToolName)
}

// hasActiveCodexTopLevelCommand is the queue gate's half of the rule above:
// a live command of the root thread.
func (r *Router) hasActiveCodexTopLevelCommand(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if execs := r.codexExecIfPresent(threadID); execs != nil {
		for _, tracker := range execs.byLaunch {
			if tracker.parentID == "" {
				return true
			}
		}
	}
	return false
}

// CodexCommand is one live command as a stop sees it: where its process
// runs and the check that would confirm it gone.
type CodexCommand struct {
	LaunchID  string
	ProcessID string
	// ScopeID and Addressable are the process scope the command started in
	// (provider.ProcessScope).
	ScopeID     string
	Addressable bool
	Summary     string
	// Seq orders the thread's command starts (CodexScopeCheck.Through).
	Seq uint64
	// Check confirms the command gone through its scope's list
	// (ConfirmCodexScope).
	Check CodexScopeCheck
}

// CodexScopeCheck is a request to confirm which of a scope's commands are
// gone, pinned to the session that asked and to the commands that had
// started when it asked: Codex answers for its process table as it is when
// the request is read, so a command that started after the request cannot
// be judged by it.
type CodexScopeCheck struct {
	ScopeID string
	Token   string
	Through uint64
}

// LiveCodexCommand returns the live command launchID, and false when no
// live command has that item id.
func (r *Router) LiveCodexCommand(threadID, launchID string) (CodexCommand, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tracker := r.codexExecIfPresent(threadID).lookup(strings.TrimSpace(launchID))
	if tracker == nil {
		return CodexCommand{}, false
	}
	return r.codexCommandLocked(threadID, tracker), true
}

// LiveCodexCommands returns the thread's live commands, in no order.
func (r *Router) LiveCodexCommands(threadID string) []CodexCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	execs := r.codexExecIfPresent(threadID)
	if execs == nil {
		return nil
	}
	commands := make([]CodexCommand, 0, len(execs.byLaunch))
	for _, tracker := range execs.byLaunch {
		commands = append(commands, r.codexCommandLocked(threadID, tracker))
	}
	return commands
}

// LiveRootCodexCommandForProcess resolves a process id among the root
// thread's live commands, the only ones a row stopped by process id can
// name (codexCommandProcessIDKey).
func (r *Router) LiveRootCodexCommandForProcess(threadID, processID string) (CodexCommand, bool) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return CodexCommand{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if execs := r.codexExecIfPresent(threadID); execs != nil {
		for _, tracker := range execs.byLaunch {
			if tracker.parentID == "" && tracker.processID == processID {
				return r.codexCommandLocked(threadID, tracker), true
			}
		}
	}
	return CodexCommand{}, false
}

// codexCommandLocked copies tracker for a stop. Caller holds r.mu.
func (r *Router) codexCommandLocked(threadID string, tracker *unifiedExecTracker) CodexCommand {
	return CodexCommand{
		LaunchID:    tracker.launchID,
		ProcessID:   tracker.processID,
		ScopeID:     tracker.scope.ID,
		Addressable: tracker.scope.Addressable && tracker.scope.ID != "",
		Summary:     tracker.summary,
		Seq:         tracker.seq,
		Check:       r.codexScopeCheckLocked(threadID, tracker.scope.ID),
	}
}

// PinCodexScopeCheck pins a check of scope's commands to now, for a caller
// about to ask Codex about the scope: a command that starts after this is
// not covered by the answer.
func (r *Router) PinCodexScopeCheck(threadID, scopeID string) CodexScopeCheck {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codexScopeCheckLocked(threadID, scopeID)
}

// CodexScopeCheckFor pins a check of scope's commands to now, and reports
// false when the scope holds no live command to check.
func (r *Router) CodexScopeCheckFor(threadID, scopeID string) (CodexScopeCheck, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	execs := r.codexExecIfPresent(threadID)
	if execs == nil {
		return CodexScopeCheck{}, false
	}
	for _, tracker := range execs.byLaunch {
		if tracker.scope.ID == scopeID && tracker.scope.Addressable {
			return r.codexScopeCheckLocked(threadID, scopeID), true
		}
	}
	return CodexScopeCheck{}, false
}

func (r *Router) codexScopeCheckLocked(threadID, scopeID string) CodexScopeCheck {
	check := CodexScopeCheck{ScopeID: scopeID}
	if id := r.identityIfPresent(threadID); id != nil {
		check.Token = id.sessionToken
	}
	if execs := r.codexExecIfPresent(threadID); execs != nil {
		check.Through = execs.lastSeq
	}
	return check
}

// ConfirmCodexScope applies way 4 of codexExecTrackers: Codex's answer to
// check. listed holds the item ids the scope's list returned; notLoaded
// reports that Codex has no such thread loaded, which ends every command in
// it. A command check covers (it started by check.Through) that the answer
// does not hold leaves the live work. An answer for a session other than
// the one that asked changes nothing. Returns the item ids it ended.
func (r *Router) ConfirmCodexScope(threadID string, check CodexScopeCheck, listed []string, notLoaded bool) []string {
	held := make(map[string]bool, len(listed))
	for _, itemID := range listed {
		held[itemID] = true
	}
	r.mu.Lock()
	var ended []string
	if execs := r.codexExecIfPresent(threadID); execs != nil && r.codexCheckCurrentLocked(threadID, check) {
		for _, tracker := range execs.byLaunch {
			if tracker.scope.ID != check.ScopeID || tracker.seq > check.Through || (!notLoaded && held[tracker.launchID]) {
				continue
			}
			ended = append(ended, tracker.launchID)
			execs.end(tracker)
		}
	}
	r.mu.Unlock()
	if len(ended) > 0 {
		sort.Strings(ended)
		r.emitCodexBackgroundChanged(threadID)
	}
	return ended
}

// codexCheckCurrentLocked reports whether check was asked of the session
// that runs the thread now. Caller holds r.mu.
func (r *Router) codexCheckCurrentLocked(threadID string, check CodexScopeCheck) bool {
	id := r.identityIfPresent(threadID)
	if id == nil || id.stopped {
		return false
	}
	return id.sessionToken == check.Token
}

// CodexExecChanged returns a channel that closes at the next change to the
// thread's live Codex work, for a caller waiting for commands to leave. Take
// it before reading the state it waits on, so no change falls between. An
// entry lives until that change; one per thread at most.
func (r *Router) CodexExecChanged(threadID string) <-chan struct{} {
	r.codexExecChangedMu.Lock()
	defer r.codexExecChangedMu.Unlock()
	changed := r.codexExecChanged[threadID]
	if changed == nil {
		if r.codexExecChanged == nil {
			r.codexExecChanged = make(map[string]chan struct{})
		}
		changed = make(chan struct{})
		r.codexExecChanged[threadID] = changed
	}
	return changed
}

func (r *Router) signalCodexExecChanged(threadID string) {
	r.codexExecChangedMu.Lock()
	if changed := r.codexExecChanged[threadID]; changed != nil {
		close(changed)
		delete(r.codexExecChanged, threadID)
	}
	r.codexExecChangedMu.Unlock()
}
