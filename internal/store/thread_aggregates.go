package store

import (
	"fmt"
)

// Thread-wide read queries that back dedicated frontend bindings. These
// exist so surfaces like PlanSidebar and DiffPanelDrawer, which need to
// see the whole thread, not just the currently-loaded paging window,
// don't re-derive from `pane.items`
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
		   JOIN payloads ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id`+servedItemJoin+`
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

// GetThreadProposedPlanItem returns a proposed plan row as a page reads
// it, content and `rev` from one snapshot (ListWireItems), so the row an
// emitter pushes is the row a client can later prove fresh.
func (s *Store) GetThreadProposedPlanItem(threadID, itemID string) (Item, bool, error) {
	rows, err := s.ListWireItems(threadID, []string{itemID})
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get proposed plan item %s/%s: %w", threadID, itemID, err)
	}
	for _, row := range rows {
		if row.Role == "assistant" && row.PayloadKind == "proposed_plan" {
			return row, true, nil
		}
	}
	return Item{}, false, nil
}
