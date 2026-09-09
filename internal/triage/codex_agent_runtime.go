package triage

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Spawn events and completed history are immutable. These copies belong only
// to the live projection. Never persist them through an item write path.
// See docs/specs/agent-visibility.md#immutable-agent-history.
func (r *Router) codexAgentRuntimeOrLaunch(launch store.Item) store.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.codexBackgroundIfPresent(launch.ThreadID); state != nil {
		if current, ok := state.agents[launch.ID]; ok {
			return current
		}
	}
	return launch
}

func (r *Router) setCodexAgentRuntime(current store.Item) error {
	current.Meta = mergeItemMetaJSON(current.Meta, json.RawMessage(`{"codex_live_projection":true}`))
	r.mu.Lock()
	state := r.codexBackgroundForThread(current.ThreadID)
	if _, exists := state.agents[current.ID]; !exists && len(state.agents) >= subagentProgressCap {
		r.mu.Unlock()
		return fmt.Errorf("Codex agent runtime limit reached for thread %s", current.ThreadID)
	}
	state.agents[current.ID] = current
	progress := r.state(current.ThreadID).subagentProgress[current.ID]
	r.mu.Unlock()
	projected := itemwire.ProjectItems([]store.Item{current}, true)[0]
	r.emit(eventchan.ProviderSubagentProgress, SubagentProgressEvent{ThreadID: current.ThreadID, ItemID: current.ID, ParentID: current.ParentID, Progress: progress, UpdatedAt: current.UpdatedAt, CodexAgent: &projected})
	return nil
}

func (r *Router) CodexAgentRuntimeSnapshot(threadID string) []store.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := []store.Item{}
	if state := r.codexBackgroundIfPresent(threadID); state != nil {
		for _, item := range state.agents {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// ListLiveCodexAgentTasks returns copies for live surfaces, not history items.
func (r *Router) ListLiveCodexAgentTasks(threadID string) []store.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	var items []store.Item
	if state := r.codexBackgroundIfPresent(threadID); state != nil {
		for _, item := range state.agents {
			var meta struct {
				Active  bool             `json:"live_background_active"`
				Runtime codexRuntimeMeta `json:"codex_runtime"`
			}
			if json.Unmarshal([]byte(item.Meta), &meta) != nil || !meta.Active {
				continue
			}
			item.Status, item.IsBackground = statusRunning, true
			if decodeCodexItemMeta(json.RawMessage(item.Meta)).Runtime != nil {
				item.CreatedAt = meta.Runtime.StartedAt
			}
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt < items[j].CreatedAt
		}
		return items[i].ID < items[j].ID
	})
	return items
}

func codexRuntimeActive(item store.Item) bool {
	var meta struct {
		Active bool `json:"live_background_active"`
	}
	return json.Unmarshal([]byte(item.Meta), &meta) == nil && meta.Active
}

// Session teardown closes live executions by adding completion records. The
// snapshots were captured before runtime was cleared; this must not recreate
// session state or write to any original spawn or completed history record.
type closedCodexAgent struct {
	item     store.Item
	progress provider.SubagentProgressMeta
}

func codexExecutionCompletionID(launchID, turnID string, generations map[string]int) string {
	id := ToolCompletionID(launchID)
	if turnID != "" {
		return id + ":turn:" + turnID
	}
	generation := 0
	for _, value := range generations {
		if value > generation {
			generation = value
		}
	}
	if generation > 0 {
		return fmt.Sprintf("%s:run:%d", id, generation)
	}
	return id
}

func (r *Router) finishClosedCodexAgents(items []closedCodexAgent, now int64) {
	for _, closed := range items {
		item := closed.item
		closed.progress.Activity = ""
		meta := decodeCodexItemMeta(json.RawMessage(item.Meta))
		runtime := codexRuntimeMeta{}
		if meta.Runtime != nil {
			runtime = *meta.Runtime
		}
		runtime.Status, runtime.UpdatedAt = "notLoaded", now
		endIndex, _, err := r.store.MaxItemIndexForTurn(item.ThreadID, item.TurnIndex)
		if err != nil {
			r.reportCompletedHistoryConflict(item.ThreadID, item.ParentID, err)
			continue
		}
		fields, err := json.Marshal(map[string]any{subagentProgressMetaKey: closed.progress, "item_status": "killed", "codex_runtime": runtime, "live_background_active": false, "codex_background_end_reason": "session_ended", "codex_execution_started_at": runtime.StartedAt, "codex_execution_completed_at": now, "codex_execution_child_start_index": runtime.ChildStartIndex, "codex_execution_child_end_index": endIndex})
		if err != nil {
			r.reportCompletedHistoryConflict(item.ThreadID, item.ParentID, err)
			continue
		}
		id := codexExecutionCompletionID(item.ID, runtime.TurnID, decodeCodexChildResumeGenerations(json.RawMessage(item.Meta)))
		evt := provider.ProviderEvent{ThreadID: item.ThreadID, ItemID: item.ID, Meta: json.RawMessage(mergeItemMetaJSON(item.Meta, fields)), Timestamp: time.UnixMilli(now)}
		if err := r.synthesizeCodexBackgroundCompletion(evt, item.ID, codexBackgroundCompletionOptions{completionID: id}); err != nil {
			r.reportCompletedHistoryConflict(item.ThreadID, item.ParentID, err)
		}
	}
}
