package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// The source's unsettled rows below a fork's cut, and what the fork does
// with each (docs/architecture/sqlite-store.md#pointer-forks).

// forkUnsettledRowsTx decides what the fork does with the source's
// unsettled rows below the cut, read through idx_items_unsettled, and
// returns every row the fork hides:
//
//   - a background launch with no completion inside the cut is live work
//     of the source's provider process, which the fork's own process can
//     never finish. It is hidden (the fork shows no ghost row that can
//     never complete).
//   - a settled background launch (running forever beside its completion
//     sibling, invariant 24) is finished history and stays.
//   - every other running or streaming row is settled in the fork's copy.
//
// A background completion lands at the write head, possibly turns after its
// launch, so a settled launch's completion is read unless the cut is at the
// source's tail, where every row the source has is inside it. Unsettled
// imported history is not examined: imported rows come from
// a finished provider history. The hidden launches and the rows the cut
// itself hides are expanded to everything that hangs off them
// (forkHiddenClosureTx).
func forkUnsettledRowsTx(tx *sql.Tx, sourceID string, plan forkCut) (hidden, settle []string, err error) {
	rows, err := tx.Query(forkUnsettledRowsSQL, sourceID, plan.turn, plan.item, sourceID, plan.turn, plan.item)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read unsettled rows of %s: %w", sourceID, err)
	}
	type unsettled struct {
		id         string
		background bool
		kind       string
		turn       int
		live       bool
	}
	var found []unsettled
	for rows.Next() {
		var row unsettled
		if err := rows.Scan(&row.id, &row.background, &row.kind, &row.turn, &row.live); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("store: scan unsettled row: %w", err), rows.Close())
		}
		found = append(found, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, fmt.Errorf("store: iterate unsettled rows of %s: %w", sourceID, err)
	}

	tail, err := forkCutAtTailTx(tx, sourceID, plan)
	if err != nil {
		return nil, nil, err
	}
	var roots, settled []string
	candidates := make([]unsettled, 0, len(found))
	var completed []unsettled
	for _, row := range found {
		switch {
		case !row.background:
			candidates = append(candidates, row)
		case row.live:
			roots = append(roots, row.id)
		default:
			completed = append(completed, row)
			if !tail {
				settled = append(settled, row.id)
			}
		}
	}
	losing, err := launchesLosingCompletionTx(tx, sourceID, settled,
		"(turn_index, item_index) < (?, ?)", []any{plan.turn, plan.item})
	if err != nil {
		return nil, nil, err
	}
	roots = append(roots, losing...)
	lost := make(map[string]bool, len(losing))
	for _, id := range losing {
		lost[id] = true
	}
	for _, row := range completed {
		if !lost[row.id] && row.kind != "tool_call" {
			candidates = append(candidates, row)
		}
	}
	hidden, err = forkHiddenClosureTx(tx, sourceID, append(append([]string{}, plan.hidden...), roots...))
	if err != nil {
		return nil, nil, err
	}
	skip := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		skip[id] = true
	}
	for _, row := range candidates {
		if !skip[row.id] {
			settle = append(settle, row.id)
		}
	}
	return hidden, settle, nil
}

// forkRunningTurnRowsTx returns the rows of the cut turn the fork shows,
// other than the ones it settles, when the source is still running that
// turn. The source's provider keeps writing that turn's rows below the
// cut: queued messages fold, move to the turn's end and take their echo's
// ids, a Codex spawn takes its child's identity. The fork's history is the
// turn as it stands now, so the fork owns a copy of each (snapshotRowsTx)
// and those writes change no row it shows (fork_triggers.go). A cut turn
// the source has settled costs one probe of idx_turns_inflight; a running
// one, a read of its rows.
func forkRunningTurnRowsTx(tx *sql.Tx, forkID, sourceID string, plan forkCut, settle []string) ([]inheritedRow, error) {
	var running bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM turns WHERE thread_id = ? AND turn_index = ? AND completed_at IS NULL)`,
		sourceID, plan.turn).Scan(&running); err != nil {
		return nil, fmt.Errorf("store: read whether %s runs turn %d: %w", sourceID, plan.turn, err)
	}
	if !running {
		return nil, nil
	}
	rows, err := queryInheritedRows(tx, forkID, timelineSelection{
		Turn: "?", TurnArgs: []any{plan.turn},
		Where: "(items.turn_index, items.item_index) < (?, ?)", WhereArgs: []any{plan.turn, plan.item},
	})
	if err != nil {
		return nil, err
	}
	settling := make(map[string]bool, len(settle))
	for _, id := range settle {
		settling[id] = true
	}
	return slices.DeleteFunc(rows, func(row inheritedRow) bool { return settling[row.id] }), nil
}

// launchesLosingCompletionTx returns the candidates, settled background
// launches of threadID's timeline, that no ending completion row matching
// kept completes (a predicate on unqualified item columns, with keptArgs);
// a parked stop completes nothing (agent_stops.go). A history that keeps
// such a launch without its completion would show it settled by a
// completion it does not have. Fork creation hides the
// launch from the fork (forkUnsettledRowsTx); a split gives the holder a
// settled copy for the forks that read the completion there, and the
// thread that keeps the launch revives it
// (trg_items_revive_bg_launch_on_completion_move).
func launchesLosingCompletionTx(tx *sql.Tx, threadID string, candidates []string, kept string, keptArgs []any) ([]string, error) {
	var losing []string
	for _, id := range candidates {
		query, args, err := timelineArms(tx, threadID, timelineSelection{
			Columns:   func(string, string) string { return "1" },
			KeyFirst:  true,
			Where:     "items.completion_of <> '' AND items.completion_of = ? AND items.status <> '" + ItemStatusParked + "' AND (" + kept + ")",
			WhereArgs: append([]any{id}, keptArgs...),
		})
		if err != nil {
			return nil, err
		}
		var completed bool
		if err := tx.QueryRow(`SELECT EXISTS(`+query+`)`, args...).Scan(&completed); err != nil {
			return nil, fmt.Errorf("store: read completion of %s/%s: %w", threadID, id, err)
		}
		if !completed {
			losing = append(losing, id)
		}
	}
	return losing, nil
}

// forkUnsettledRowsSQL reads the running and streaming rows of a source's
// timeline below a cut, its own and the ones it inherits, through
// idx_items_unsettled. It binds (source, cut turn, cut item) twice.
var forkUnsettledRowsSQL = `
		SELECT items.id, items.is_background, items.kind, items.turn_index,
		       COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		  FROM items
		 WHERE items.thread_id = ? AND items.status IN ('running', 'streaming')
		   AND (items.turn_index, items.item_index) < (?, ?)
		UNION ALL
		SELECT items.id, items.is_background, items.kind, items.turn_index,
		       COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
		  FROM thread_fork_lineage l
		  CROSS JOIN items ON items.thread_id = l.ancestor_id
		 WHERE l.thread_id = ? AND items.status IN ('running', 'streaming')
		   AND (items.turn_index, items.item_index) < (?, ?)
		   AND ` + inheritedItemVisibleSQL

// forkCutAtTailTx reports whether the cut follows every row of the source's
// timeline.
func forkCutAtTailTx(tx *sql.Tx, sourceID string, plan forkCut) (bool, error) {
	query, binds, err := timelineArms(tx, sourceID, timelineSelection{
		Columns:   timelineIDColumns,
		Where:     "(items.turn_index, items.item_index) >= (?, ?)",
		WhereArgs: []any{plan.turn, plan.item},
		OrderBy:   "turn_index, item_index",
		Limit:     1,
	})
	if err != nil {
		return false, err
	}
	var id string
	var turn, item int
	switch err := tx.QueryRow(query, binds...).Scan(&id, &turn, &item); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("store: read past the fork cut of %s: %w", sourceID, err)
	}
	return false, nil
}

// forkHiddenClosureTx expands the rows a fork hides to what hangs off them
// in the source's timeline: rows under them through parent_id and rows that
// complete them through completion_of, in any order. No row the fork shows
// then references a row it hides.
func forkHiddenClosureTx(tx *sql.Tx, sourceID string, roots []string) ([]string, error) {
	seen := make(map[string]bool, len(roots))
	var out []string
	frontier := roots
	for len(frontier) > 0 {
		subtree, err := timelineSubtreeTx(tx, sourceID, frontier)
		if err != nil {
			return nil, err
		}
		var added []string
		for _, id := range subtree {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
				added = append(added, id)
			}
		}
		frontier = nil
		for start := 0; start < len(added); start += forkCopyBatch {
			clause, args := inClause("items.completion_of", added[start:min(start+forkCopyBatch, len(added))])
			query, binds, err := timelineArms(tx, sourceID, timelineSelection{
				Columns:   timelineIDColumns,
				Where:     "items.completion_of <> '' AND " + clause,
				WhereArgs: args,
				OrderBy:   "turn_index, item_index",
			})
			if err != nil {
				return nil, err
			}
			ids, err := queryIDs(tx, "SELECT id FROM ("+query+")", binds...)
			if err != nil {
				return nil, fmt.Errorf("store: read completions of hidden rows in %s: %w", sourceID, err)
			}
			for _, id := range ids {
				if !seen[id] {
					frontier = append(frontier, id)
				}
			}
		}
	}
	return out, nil
}

// timelineSubtreeTx returns roots and every row under them in viewer's
// timeline, walking parent_id through every physical arm.
func timelineSubtreeTx(q sqlQueryer, viewer string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	walk, args, err := descendantsWalk(q, viewer, roots, func(string) string { return "1" })
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(walk+` SELECT id FROM rel`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read subtree in %s: %w", viewer, err)
	}
	out := append([]string{}, roots...)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan subtree row: %w", err), rows.Close())
		}
		out = append(out, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate subtree in %s: %w", viewer, err)
	}
	return out, nil
}
