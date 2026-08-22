package store

import (
	"database/sql"
	"fmt"
)

// Thread-wide read queries that back dedicated frontend bindings. These
// exist so surfaces like PlanSidebar, DiffPanelDrawer, and
// BackgroundTaskTray — which need to see the whole thread, not just the
// currently-loaded paging window — don't re-derive from `pane.items`
// after the timeline moved to a windowed model. Each query is
// thread-scoped and returns the minimum item set the frontend needs to
// compute its own views; expensive per-item payload bodies are never
// loaded here (payload meta is carried via the standard LEFT JOIN, but
// `data` stays in the on-demand expansion path).

// ListThreadProposedPlans returns the current assistant-authored plan item for
// the thread whose joined payload_kind equals "proposed_plan". The result is a
// 0-or-1 item slice so the JSON response always carries a stable shape while
// avoiding history payloads the UI no longer presents.
//
// The `role = 'assistant'` filter keeps a user-authored item whose
// payload_kind happens to collide with 'proposed_plan' (possible via
// forks / imports) out of plan UI — only plans the agent actually
// proposed should appear.
func (s *Store) ListThreadProposedPlans(threadID string) ([]Item, error) {
	rows, err := s.reader().Query(
		`SELECT `+itemColumns+`
		   FROM proposed_plans
		   JOIN items
		     ON items.thread_id = proposed_plans.thread_id
		    AND items.id = proposed_plans.item_id
		   JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE proposed_plans.thread_id = ?
		    AND items.role = 'assistant'
		    AND payloads.kind = 'proposed_plan'
		  ORDER BY proposed_plans.version DESC
		  LIMIT 1`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread proposed plans for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proposed plan row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposed plans for %s: %w", threadID, err)
	}
	decorated, err := s.decorateProposedPlanItems(s.reader(), threadID, out)
	if err != nil {
		return nil, fmt.Errorf("store: decorate proposed plans for %s: %w", threadID, err)
	}
	return decorated, nil
}

func (s *Store) GetThreadProposedPlanItem(threadID, itemID string) (Item, bool, error) {
	row := s.reader().QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.id = ?
		    AND items.role = 'assistant'
		    AND payloads.kind = 'proposed_plan'
		  LIMIT 1`,
		threadID, itemID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	decorated, err := s.decorateProposedPlanItems(s.reader(), threadID, []Item{item})
	if err != nil {
		return Item{}, false, fmt.Errorf("store: decorate proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	return decorated[0], true, nil
}

// ListLiveBackgroundTasks returns the tray's item set: live background
// launches plus their completion siblings whose `created_at` is inside
// the retention window.
//
// The tray lists by BACKGROUNDED ANCESTRY, not by top-level-ness
// (docs/specs/agent-visibility.md Q8). Three row classes qualify:
//
//  1. every live launch that is `is_background = 1`, at ANY depth — a
//     nested agent that backgrounded a Bash is running work the user
//     needs a handle on, and hiding it because its parent happened to
//     be an agent is how backgrounded work went invisible;
//  2. every live AGENT LAUNCH that descends from a background launch
//     (subagentLaunchFilterFor — structural, never a tool-name list).
//     Foreground PLAIN tool calls under a background agent stay out:
//     they are the agent's own work, rendered inside its card. Because
//     only a launch can be a parent, this class also supplies every
//     intermediate ancestor between a background root and a nested
//     launch, so the frontend indents by walking `parentId` WITHIN the
//     result instead of asking for rows it was not given;
//  3. the recent completion siblings of that same anchor set.
//
// A background process launch stays `status='running'` forever (spec
// invariant — the sibling completion row carries the final state). The
// launch and its completion must age out together: returning an orphan
// launch whose completion was pruned would re-render it as "running"
// indefinitely. A launch with no completion yet still surfaces unless
// projection meta explicitly marks it inactive with
// `live_background_active=false`.
//
// This is the DISPLAY query only. The reaper and queue gates in
// items_lifecycle.go (HasRunningTopLevelForegroundToolCall,
// HasLiveBackgroundToolCall, HasQueueBlockingBackgroundToolCall,
// CountLiveRunningBackgroundToolCalls,
// MarkLiveBackgroundToolCallsInactive) and paging.go's
// topLevelItemsFilter KEEP `parent_id = ''`: whether the tray SHOWS a
// nested background Bash and whether that Bash blocks the flush queue
// or survives a session teardown are different questions, and the
// second one is still answered at the top level only.
//
// Codex subagents are different: the chat-history spawn card is completed
// immediately while the child thread keeps running. App.ListLiveBackgroundTasks
// adds those via Store.ListLiveCodexSubagentLaunches and projects the tray copy
// as running without mutating the stored card.
//
// Thread-scoped. Live launches surface regardless of turn_index.
// Ordering is (turn_index, item_index) so launches precede completions.
func (s *Store) ListLiveBackgroundTasks(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	// Placeholder order: bg roots (threadID), descendants base hop
	// (threadID), descendants recursive hop (threadID), anchor join
	// (threadID), outer scope (threadID), launch completion window
	// (cutoff), sibling window (cutoff).
	rows, err := s.reader().Query(
		`WITH RECURSIVE bg(id) AS (
		    SELECT id FROM items
		     WHERE thread_id = ?
		       AND kind = 'tool_call'
		       AND is_background = 1
		),
		`+descendantsCTE("items", "SELECT id FROM bg")+`,
		anchors(id) AS (
		    SELECT id FROM bg
		    UNION
		    SELECT i.id
		      FROM rel
		      CROSS JOIN items i ON i.thread_id = ? AND i.id = rel.id
		     WHERE `+subagentLaunchFilterFor("i.")+`
		)
		SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND (
		      (
		        items.id IN (SELECT id FROM anchors)
		        AND items.kind = 'tool_call'
		        AND items.status = 'running'
		        AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		        AND (
		          NOT EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.completion_of <> ''
		          )
		          OR EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.completion_of <> ''
		               AND c.created_at >= ?
		          )
		        )
		      )
		      OR (
		        items.completion_of <> ''
		        AND items.created_at >= ?
		        AND items.completion_of IN (SELECT id FROM anchors)
		      )
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID, threadID, threadID, threadID, threadID, retentionCutoffMillis, retentionCutoffMillis,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list live background tasks for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan background task row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate background tasks for %s: %w", threadID, err)
	}
	return out, nil
}
