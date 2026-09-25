package triage

import (
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// A Codex execution's tool count is Agent Overflow's own: Codex reports
// none for a child. It is the number of tool_call rows stored directly
// under the spawn in the spawn's turn within the execution's bounds, the
// rows its completion card's body is sliced to
// (codex_execution_child_start_index, codex_execution_child_end_index].
// A nested agent's spawn is one tool call of the execution; the rows
// under it count toward the nested agent.
//
// The completion's number is read from the stored rows when the
// completion is written (synthesizeCodexBackgroundCompletion). The live
// number, subagentProgress[launch].ToolUses, is kept by increment: an
// execution's start seeds it from the rows already in its bounds, which
// is zero unless AO observed the execution after it began, and every
// tool_call row inserted under the spawn after that adds one.

// codexExecutionTools is a running execution's live tool count. Rows of
// the spawn's turn at or before afterIndex are counted or precede the
// execution, so a row past it is new; tool_call rows are always appended
// to their turn.
type codexExecutionTools struct {
	turnIndex  int
	afterIndex int
	count      int
	// parentID is the spawn's own parent, for the progress frame.
	parentID string
}

// startCodexExecutionTools begins the live tool count of the execution
// that starts after startIndex in the spawn's turn. It replaces the
// previous execution's count, which the max rule of
// mergeSubagentProgress would otherwise keep.
func (r *Router) startCodexExecutionTools(launch store.Item, startIndex int) error {
	count, last, err := r.store.SubagentExecutionToolCallsAfter(launch.ThreadID, launch.ID, launch.TurnIndex, startIndex)
	if err != nil {
		return fmt.Errorf("seed Codex execution tool count %s: %w", launch.ID, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackgroundForThread(launch.ThreadID)
	if _, exists := state.tools[launch.ID]; !exists && len(state.tools) >= subagentProgressCap {
		return fmt.Errorf("Codex execution tool count limit reached for thread %s", launch.ThreadID)
	}
	state.tools[launch.ID] = codexExecutionTools{turnIndex: launch.TurnIndex, afterIndex: last, count: count, parentID: launch.ParentID}
	progress := r.state(launch.ThreadID).subagentProgress[launch.ID]
	progress.ToolUses = count
	r.putLiveSubagentProgressLocked(launch.ThreadID, launch.ID, progress)
	return nil
}

// countCodexExecutionToolCall adds a row just written to the live tool
// count of the running execution it belongs to, and announces the count.
// Every other row, and a rewrite of a counted one, leaves it alone.
func (r *Router) countCodexExecutionToolCall(item store.Item) {
	if item.Kind != itemKindToolCall || item.ParentID == "" {
		return
	}
	r.mu.Lock()
	state := r.codexBackgroundIfPresent(item.ThreadID)
	if state == nil {
		r.mu.Unlock()
		return
	}
	tools, ok := state.tools[item.ParentID]
	if !ok || item.TurnIndex != tools.turnIndex || item.ItemIndex <= tools.afterIndex {
		r.mu.Unlock()
		return
	}
	tools.afterIndex, tools.count = item.ItemIndex, tools.count+1
	state.tools[item.ParentID] = tools
	progress := r.state(item.ThreadID).subagentProgress[item.ParentID]
	progress.ToolUses = tools.count
	r.putLiveSubagentProgressLocked(item.ThreadID, item.ParentID, progress)
	r.mu.Unlock()
	r.emit(eventchan.ProviderSubagentProgress, SubagentProgressEvent{
		ThreadID:  item.ThreadID,
		ItemID:    item.ParentID,
		ParentID:  tools.parentID,
		Progress:  progress,
		UpdatedAt: item.UpdatedAt,
	})
}
