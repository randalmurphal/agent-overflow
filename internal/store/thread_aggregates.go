package store

import (
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

// ListThreadProposedPlans returns every assistant-authored item for the
// thread whose joined payload_kind equals "proposed_plan", newest-first.
// Ordering is (turn_index DESC, item_index DESC) so the PlanSidebar can
// render the most recent plans at the top without an additional sort
// pass in the frontend. Returns an empty slice (not nil) when no plans
// exist so the JSON response always carries a stable shape.
//
// The `role = 'assistant'` filter keeps a user-authored item whose
// payload_kind happens to collide with 'proposed_plan' (possible via
// forks / imports) out of the sidebar — only plans the agent actually
// proposed should appear.
func (s *Store) ListThreadProposedPlans(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.role = 'assistant'
		    AND payloads.kind = 'proposed_plan'
		  ORDER BY items.turn_index DESC, items.item_index DESC`,
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
	return out, nil
}

// ListThreadDiffPayloads returns every item for the thread that
// contributes to the cumulative diff view. Two kinds qualify:
//
//   - `diff` payloads unconditionally (every row is a real patch).
//   - `tool_result` payloads whose meta carries
//     `inlineDiff.availability == "exact_patch"` — tool_results are
//     produced by most tool-call paths but only the ones that emit
//     an inline patch are relevant to the cumulative diff.
//
// The meta check is done in SQL via `json_extract` so large threads
// don't pay the cost of shipping every tool_result over Wails just for
// the frontend to discard the non-diff ones (a long agent session can
// easily produce thousands of tool_results; only a fraction are
// diffs). Keeps the selector logic aligned with
// `selectAgentDiffEntries` in the frontend — that function still runs
// as a second pass to build the AgentDiffEntry shape.
//
// Ordering is (turn_index, item_index) ASC, matching the frontend
// selector's expectation that entries arrive in chronological order.
func (s *Store) ListThreadDiffPayloads(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND (
		      payloads.kind = 'diff'
		      OR (
		        payloads.kind = 'tool_result'
		        AND json_extract(payloads.meta, '$.inlineDiff.availability') = 'exact_patch'
		      )
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread diff payloads for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan diff payload row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate diff payloads for %s: %w", threadID, err)
	}
	return out, nil
}

// ListLiveBackgroundTasks returns the item set the BackgroundTaskTray
// consumes: every still-running background launch plus every completion
// row whose `completion_of` points at a background launch and whose
// `created_at` is at or after `retentionCutoffMillis` (the tray keeps
// completed rows visible for a brief grace window so users can see the
// "finished" state).
//
// Thread-scoped. Running tasks are returned regardless of their
// turn_index so a background task launched in a now-paged-out turn still
// surfaces in the tray. Ordering is (turn_index, item_index) so launch
// rows appear before their completion in the returned slice.
func (s *Store) ListLiveBackgroundTasks(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND (
		      (items.is_background = 1 AND items.status = 'running')
		      OR (
		        items.completion_of <> ''
		        AND items.created_at >= ?
		        AND items.completion_of IN (
		          SELECT id FROM items
		           WHERE thread_id = ? AND is_background = 1
		        )
		      )
		    )
		  ORDER BY items.turn_index, items.item_index`,
		threadID, retentionCutoffMillis, threadID,
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
