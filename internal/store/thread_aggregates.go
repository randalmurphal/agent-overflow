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
		// INNER JOIN matches reader intent: the WHERE clause already
		// requires payloads.kind='proposed_plan', so a row with no
		// payload can never survive a LEFT JOIN here. Using JOIN
		// signals that correlation explicitly and lets SQLite's
		// planner skip the outer-join rewrite pass.
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN payloads ON payloads.id = items.payload_id
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
		// INNER JOIN — the WHERE clause requires payloads.kind to be
		// one of two literal values, so payload-less items can never
		// match regardless of join flavor. Inner-join spells that out.
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN payloads ON payloads.id = items.payload_id
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

// ListLiveBackgroundTasks returns the tray's item set: live background
// launches plus their completion siblings whose `created_at` is inside
// the retention window.
//
// A background launch stays `status='running'` forever (spec invariant
// — the sibling completion row carries the final state). The launch
// and its completion must age out together: returning an orphan launch
// whose completion was pruned would re-render it as "running"
// indefinitely. A launch with no completion yet still surfaces.
//
// Thread-scoped. Live launches surface regardless of turn_index.
// Ordering is (turn_index, item_index) so launches precede completions.
func (s *Store) ListLiveBackgroundTasks(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND (
		      (
		        items.is_background = 1
		        AND items.status = 'running'
		        AND (
		          NOT EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		          )
		          OR EXISTS (
		            SELECT 1 FROM items c
		             WHERE c.thread_id = items.thread_id
		               AND c.completion_of = items.id
		               AND c.created_at >= ?
		          )
		        )
		      )
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
		threadID, retentionCutoffMillis, retentionCutoffMillis, threadID,
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
