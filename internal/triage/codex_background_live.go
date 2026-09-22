package triage

import (
	"sort"
	"strings"

	"agent-overflow/internal/store"
)

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
	for _, item := range state.agents {
		if codexRuntimeActive(item) {
			count++
		}
	}
	for _, tracker := range state.unifiedExec {
		if tracker != nil && strings.TrimSpace(tracker.parentID) == "" {
			count++
		}
	}
	return count
}

// ThreadIDsWithLiveCodexBackgroundTasks snapshots the threads that currently
// own active Codex agents or top-level unified-exec tasks. Project-wide
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
		agentActive := false
		for _, item := range state.agents {
			if codexRuntimeActive(item) {
				agentActive = true
				break
			}
		}
		if agentActive {
			ids = append(ids, threadID)
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
