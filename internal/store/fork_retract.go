package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A pointer fork's own changes to the rows it inherits
// (docs/architecture/sqlite-store.md#pointer-forks). The rows belong to an
// ancestor, so the fork never deletes them: a revert lowers its cut, and a
// delete of one row hides it.

// retractInheritedTx is a fork's own revert of rows it inherits. The rows
// are an ancestor's, so they stay: the fork's cut moves to just after the
// last row it shows before the first reverted inherited row, and the
// inherited rows past that point the revert keeps (a promoted anchor's
// same-turn content) become the fork's own copies (snapshotRowsTx), with
// the turn rows the fork then shows. predicate selects the reverted rows
// with unqualified item columns, all at or after fromTurn. The caller
// removes its own reverted rows first (splitShownRowsTx, then the delete),
// so the rows the fork's own forks read stay theirs. The stamps of the
// fork's copied anchors, whose subtrees can hold the rows that leave, are
// recomputed by w's finish (forkCopyStampsTx).
func retractInheritedTx(tx *sql.Tx, w *cardWrite, threadID string, fromTurn int, predicate string, args []any) error {
	var depth int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM thread_fork_lineage WHERE thread_id = ?`, threadID).Scan(&depth); err != nil {
		return fmt.Errorf("store: read fork lineage of %s: %w", threadID, err)
	}
	if depth == 0 {
		return nil
	}
	query, queryArgs := inheritedTimelineArms(threadID, allLevels, timelineSelection{
		Columns: timelineIDColumns,
		Turn:    "?", TurnArgs: []any{fromTurn}, FromTurn: true,
		Where: "(" + predicate + ")", WhereArgs: args,
		OrderBy: "turn_index, item_index",
		Limit:   1,
	})
	var first timelineRow
	err := tx.QueryRow(query, queryArgs...).Scan(&first.id, &first.turn, &first.item)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read the first inherited row %s reverts: %w", threadID, err)
	}
	copies, err := forkCopyStampsTx(tx, threadID)
	if err != nil {
		return err
	}
	survivors, err := queryInheritedRows(tx, threadID, timelineSelection{
		Turn: "?", TurnArgs: []any{first.turn}, FromTurn: true,
		Where:     "(items.turn_index, items.item_index) > (?, ?) AND NOT (" + predicate + ")",
		WhereArgs: append([]any{first.turn, first.item}, args...),
	})
	if err != nil {
		return err
	}
	last, found, err := lastRowThroughTurnTx(tx, threadID, first.turn,
		"(items.turn_index, items.item_index) < (?, ?)", first.turn, first.item)
	if err != nil {
		return err
	}
	cut := timelineRow{turn: last.turn, item: last.item + 1}
	through := cut.turn
	if !found {
		cut, through = first, first.turn
	}
	for _, row := range survivors {
		turn, err := inheritedRowTurnTx(tx, threadID, row.id)
		if err != nil {
			return err
		}
		through = max(through, turn)
	}
	// The fork owns the row of the turn its cut falls in and of every turn
	// it keeps rows of past the cut, copied before the cut hides them.
	if found || len(survivors) > 0 {
		if _, err := tx.Exec(
			`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
			 SELECT ? || ':' || turn_index, ?, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
			   FROM timeline_turns t
			  WHERE t.thread_id = ? AND t.turn_index >= ? AND t.turn_index <= ?
			    AND NOT EXISTS (SELECT 1 FROM turns own WHERE own.thread_id = ? AND own.turn_index = t.turn_index)`,
			threadID, threadID, threadID, cut.turn, through, threadID,
		); err != nil {
			return fmt.Errorf("store: copy fork %s cut turns: %w", threadID, err)
		}
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return snapshotRowsTx(tx, threadID, survivors)
	}); err != nil {
		return err
	}
	if !found {
		// Nothing inherited survives before the first reverted row: the
		// fork reads no ancestor from here on. Its hides stay, since its
		// own forks read through them.
		if _, err := tx.Exec(`DELETE FROM thread_fork_lineage WHERE thread_id = ?`, threadID); err != nil {
			return fmt.Errorf("store: unlink fork %s: %w", threadID, err)
		}
		w.levelsDropped = true
		return forkViewChangedTx(tx, w, threadID, copies)
	}
	if _, err := tx.Exec(
		`UPDATE thread_fork_lineage SET cut_turn_index = ?, cut_item_index = ?
		  WHERE thread_id = ? AND (cut_turn_index, cut_item_index) > (?, ?)`,
		cut.turn, cut.item, threadID, cut.turn, cut.item,
	); err != nil {
		return fmt.Errorf("store: lower fork %s cut: %w", threadID, err)
	}
	if _, err := tx.Exec(
		`UPDATE threads SET fork_cut_turn_index = ?, fork_cut_item_index = ? WHERE id = ?`,
		cut.turn, cut.item, threadID,
	); err != nil {
		return fmt.Errorf("store: record fork %s cut: %w", threadID, err)
	}
	if err := dropEmptyHolderLevelsTx(tx, w, threadID); err != nil {
		return err
	}
	return forkViewChangedTx(tx, w, threadID, copies)
}

// inheritedRowTurnTx reads the turn of one inherited row threadID shows.
func inheritedRowTurnTx(tx *sql.Tx, threadID, itemID string) (int, error) {
	query, args := inheritedTimelineArms(threadID, allLevels, timelineSelection{
		Columns:  timelineIDColumns,
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{itemID},
	})
	var row timelineRow
	if err := tx.QueryRow(query, args...).Scan(&row.id, &row.turn, &row.item); err != nil {
		return 0, fmt.Errorf("store: read the turn of %s/%s: %w", threadID, itemID, err)
	}
	return row.turn, nil
}

// forkViewChangedTx records a change to which inherited rows threadID
// shows: its stamps move (bumpForkViewTx), its turn-error pair is
// recomputed, because the change writes no row or turn whose trigger
// would, and w's finish recomputes the stamps of copies, the copied
// anchors forkCopyStampsTx listed before the change.
func forkViewChangedTx(tx *sql.Tx, w *cardWrite, threadID string, copies []string) error {
	if err := bumpForkViewTx(tx, threadID); err != nil {
		return err
	}
	if err := recomputeTurnErrorsTx(tx, threadID); err != nil {
		return err
	}
	w.subtreesChanged(copies)
	return nil
}

// forkCopyStampsSQL lists the stamped rows below a pointer fork's cut. Only
// a copy of an inherited row sits there, and a copy is the only own row
// whose subtree can hold inherited rows, so these are the stamps a change
// to the inherited rows the fork shows can move.
const forkCopyStampsSQL = `SELECT s.item_id FROM thread_fork_lineage l
  CROSS JOIN subagent_aggregates s ON s.thread_id = l.thread_id
  CROSS JOIN items i ON i.thread_id = s.thread_id AND i.id = s.item_id
 WHERE l.thread_id = ? AND l.depth = 1
   AND (i.turn_index, i.item_index) < (l.cut_turn_index, l.cut_item_index)`

// forkCopyStampsTx runs forkCopyStampsSQL, before the change it serves
// moves the cut.
func forkCopyStampsTx(q sqlQueryer, threadID string) ([]string, error) {
	ids, err := queryIDs(q, forkCopyStampsSQL, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: list copied anchors of fork %s: %w", threadID, err)
	}
	return ids, nil
}

// bumpForkViewTx records a change to which inherited rows a thread shows.
// Rows left the timeline, so the epoch moves with the revision.
func bumpForkViewTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(
		`UPDATE threads SET history_rev = history_rev + 1, history_epoch = history_epoch + 1 WHERE id = ?`, threadID,
	); err != nil {
		return fmt.Errorf("store: stamp fork %s view: %w", threadID, err)
	}
	return nil
}

// readersShowingInheritedSQL lists the levels on ?1 of the threads that
// read through ?1 and show the row ?2 of ancestor ?3 at (?4, ?5): the
// lineage visibility rule for the reader's level of ?3, which sits behind
// its level of ?1.
const readersShowingInheritedSQL = `SELECT r.thread_id, r.depth, r.cut_turn_index, r.cut_item_index
  FROM thread_fork_lineage r
 WHERE r.ancestor_id = ?1
   AND EXISTS (SELECT 1 FROM thread_fork_lineage l
                WHERE l.thread_id = r.thread_id AND l.ancestor_id = ?3 AND l.depth > r.depth
                  AND (l.cut_turn_index, l.cut_item_index) > (?4, ?5)
                  AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ?2)
                  AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                                    JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ?2
                                   WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))`

// hideInheritedItemTx removes one inherited row from threadID's timeline.
// It reports false when threadID does not show itemID as inherited. The
// threads that read the row through threadID keep it: a holder takes a
// copy for them first (holdHiddenRowsTx).
func hideInheritedItemTx(tx *sql.Tx, w *cardWrite, threadID, itemID string) (bool, error) {
	query, args := inheritedTimelineArms(threadID, allLevels, timelineSelection{
		Columns: func(string, string) string {
			return "items.id, COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), l.ancestor_id, items.turn_index, items.item_index"
		},
		KeyFirst: true,
		Where:    "items.id = ?", WhereArgs: []any{itemID},
	})
	var row inheritedRow
	var at timelineRow
	err := tx.QueryRow(query, args...).Scan(&row.id, &row.payloadID, &row.inputPayloadID, &row.owner, &at.turn, &at.item)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read inherited row %s/%s: %w", threadID, itemID, err)
	}
	readers, err := queryLevels(tx, readersShowingInheritedSQL, threadID, itemID, row.owner, at.turn, at.item)
	if err != nil {
		return false, fmt.Errorf("store: list the forks of %s that show %s: %w", threadID, itemID, err)
	}
	if len(readers) > 0 {
		if err := holdHiddenRowsTx(tx, threadID, []inheritedRow{row}, readers); err != nil {
			return false, err
		}
	}
	copies, err := forkCopyStampsTx(tx, threadID)
	if err != nil {
		return false, err
	}
	if err := hideForkRowsTx(tx, threadID, []string{itemID}); err != nil {
		return false, err
	}
	if err := dropEmptyHolderLevelsTx(tx, w, threadID); err != nil {
		return false, err
	}
	return true, forkViewChangedTx(tx, w, threadID, copies)
}
