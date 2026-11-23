package triage

import (
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/store"
)

// Codex live work is the background work only this router holds: every
// agent whose execution is active, and every unified-exec command the
// session's app-server runs, the root thread's and every agent's, at every
// depth. An agent's command outlives the agent's turn and ends only with
// the agent's thread or the session (codex_background_exec.go), so closing
// the session would kill it. The count, the tray and the project-wide
// thread list below all apply this rule; the tray lists the agents through
// ListLiveCodexAgentTasks.
//
// The queue gate is the one deliberate difference: it counts top-level
// commands only (hasActiveCodexTopLevelCommand). A queued send waits for
// the main agent's work. An agent's command is that agent's work, and the
// agent's active execution already holds the queue while it runs; a command
// left behind by an agent that finished is not something the next prompt
// waits on. Claude's queue gate draws the same line
// (store.HasQueueBlockingBackgroundToolCall).

// ListLiveCodexBackgroundTasks returns transient Codex unified exec tray
// rows, an agent's commands parented to its spawn launch. Pending
// foreground commands are included with IsBackground=false; yielded PTYs
// are included with IsBackground=true. Completed commands leave the live
// tray when typed item/completed removes the transient tracker.
func (r *Router) ListLiveCodexBackgroundTasks(threadID string, _ int64, _ int64) []store.Item {
	r.mu.Lock()
	execs := r.codexExecIfPresent(threadID)
	if execs == nil {
		r.mu.Unlock()
		return nil
	}
	trackers := make([]unifiedExecTracker, 0, len(execs.byLaunch))
	for _, tracker := range execs.byLaunch {
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

// CountLiveCodexBackgroundTasks counts the thread's Codex live work.
func (r *Router) CountLiveCodexBackgroundTasks(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return codexLiveWorkLocked(r.threadStateIfPresent(threadID))
}

// codexLiveWorkLocked is the Codex live-work rule for one thread. Caller
// holds r.mu.
func codexLiveWorkLocked(st *threadState) int {
	if st == nil {
		return 0
	}
	count := len(st.codexExec.byLaunch)
	if st.codexBackground != nil {
		for _, item := range st.codexBackground.agents {
			if codexRuntimeActive(item) {
				count++
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
		if codexLiveWorkLocked(st) > 0 {
			ids = append(ids, threadID)
		}
	}
	sort.Strings(ids)
	return ids
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

// CodexTerminalForProcess resolves the live command a tray row's process id
// names: its launch id, and its owner ("" for the root thread, the spawn
// launch id for an agent's thread), which is the scope a stop of it must
// target. ok is false when no live command has the id. Each Codex thread
// numbers its processes on its own, so two owners' commands can share an
// id; a stop by id cannot tell them apart, and that is an error.
func (r *Router) CodexTerminalForProcess(threadID, processID string) (launchID, owner string, ok bool, err error) {
	processID = strings.TrimSpace(processID)
	if processID == "" {
		return "", "", false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var matches []*unifiedExecTracker
	if execs := r.codexExecIfPresent(threadID); execs != nil {
		for _, tracker := range execs.byLaunch {
			if tracker.processID == processID {
				matches = append(matches, tracker)
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", "", false, nil
	case 1:
		return matches[0].launchID, matches[0].parentID, true, nil
	default:
		launches := make([]string, 0, len(matches))
		for _, tracker := range matches {
			launches = append(launches, tracker.launchID)
		}
		sort.Strings(launches)
		return "", "", false, fmt.Errorf("process id %s on thread %s names %d live commands (%s) run by different agents; a stop by process id cannot tell them apart", processID, threadID, len(matches), strings.Join(launches, ", "))
	}
}

// CodexTerminalOwners lists, sorted, the agent launches whose threads run
// the thread's live commands: the scopes beyond the root thread that a
// cleanup of every background terminal must reach.
func (r *Router) CodexTerminalOwners(threadID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	execs := r.codexExecIfPresent(threadID)
	if execs == nil {
		return nil
	}
	seen := map[string]bool{}
	var owners []string
	for _, tracker := range execs.byLaunch {
		if tracker.parentID != "" && !seen[tracker.parentID] {
			seen[tracker.parentID] = true
			owners = append(owners, tracker.parentID)
		}
	}
	sort.Strings(owners)
	return owners
}

// SettleCodexTerminal ends the live command launchID when the host learned
// that its process is gone: a stop found no such process. A completion
// Codex still writes for it is the command's own (codexExecTrackers.ended).
// Reports whether a command was ended.
func (r *Router) SettleCodexTerminal(threadID, launchID string) bool {
	r.mu.Lock()
	execs := r.codexExecIfPresent(threadID)
	tracker := execs.lookup(launchID)
	if tracker != nil {
		execs.end(tracker)
	}
	r.mu.Unlock()
	if tracker == nil {
		return false
	}
	r.emitCodexBackgroundChanged(threadID)
	return true
}

// SettleCodexAgentTerminals ends every live command the agent launch owner
// runs when the host learned that the agent's thread is gone, and reports
// how many it ended.
func (r *Router) SettleCodexAgentTerminals(threadID, owner string) int {
	return r.settleCodexAgentTerminals(threadID, owner, "")
}

// settleCodexAgentTerminals is SettleCodexAgentTerminals for a host call
// (sessionToken "") or a provider frame, which a retired session's token
// turns into a no-op.
func (r *Router) settleCodexAgentTerminals(threadID, owner, sessionToken string) int {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return 0
	}
	r.mu.Lock()
	removed := 0
	if execs := r.codexExecIfPresent(threadID); execs != nil && !r.frameRetiredLocked(threadID, sessionToken) {
		removed = execs.endOwnedBy(owner)
	}
	r.mu.Unlock()
	if removed > 0 {
		r.emitCodexBackgroundChanged(threadID)
	}
	return removed
}
