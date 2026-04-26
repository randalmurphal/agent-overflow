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
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM proposed_plans
		   JOIN items
		     ON items.thread_id = proposed_plans.thread_id
		    AND items.id = proposed_plans.item_id
		   JOIN payloads ON payloads.id = items.payload_id
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
	decorated, err := s.decorateProposedPlanItems(threadID, out)
	if err != nil {
		return nil, fmt.Errorf("store: decorate proposed plans for %s: %w", threadID, err)
	}
	return decorated, nil
}

func (s *Store) GetThreadProposedPlanItem(threadID, itemID string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   JOIN payloads ON payloads.id = items.payload_id
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
	decorated, err := s.decorateProposedPlanItems(threadID, []Item{item})
	if err != nil {
		return Item{}, false, fmt.Errorf("store: decorate proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	return decorated[0], true, nil
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
