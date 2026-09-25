package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/threadmode"
)

// Pointer-fork holders (docs/architecture/sqlite-store.md#pointer-forks).
//
// A row a fork shows never changes (fork_triggers.go). A thread that must
// stop showing rows its forks read, because it reverts or deletes them,
// gives them to a holder: a hidden thread (threadmode.ModeHolder) that the
// forks then read them from. The rows move once, one statement per table
// whatever the number of forks, and keep their ids, positions and content,
// so every fork reads the same timeline before and after and no client is
// told anything. splitShownRowsTx gives them to the holder the forks read
// right before the thread when it serves them all (reusableHolderTx), else
// to a new one; a deleted thread its forks still read becomes a holder
// itself (retireToHolderTx). A holder no lineage row names any more is
// marked deleting (trg_thread_fork_lineage_release) and the app deletes it.

// forkSplit names the rows a write takes out of a thread. Its predicate is
// written with unqualified item columns and never names thread_id, so it
// reads the same against a thread's rows, its imported rows and a
// correlated subquery over either.
type forkSplit struct {
	// fromTurn is the first turn a taken row sits in.
	fromTurn int
	// where selects the taken rows at or after fromTurn ("" takes all),
	// with args.
	where string
	args  []any
	// keep, when set, is where the thread's history ends for its readers:
	// the position after the last row it keeps. Readers read the thread
	// only before it from then on, so nothing the thread writes later is
	// theirs, and the holder takes the turn rows from keep's turn on that
	// the readers read. Unset, the split takes named rows and no turn, and
	// readers keep their cuts.
	keep *timelineRow
	// turnsKeptThrough is the last turn whose own turn row the thread
	// keeps. The holder takes the later ones and a copy of the rest.
	turnsKeptThrough int
}

// nothingKept is keep for a split after which a thread's readers read none
// of it.
var nothingKept = timelineRow{turn: 0, item: math.MinInt32}

// forkLevel is one lineage row: reader reads ancestor at depth, before cut.
type forkLevel struct {
	reader string
	depth  int
	cut    timelineRow
}

// moveSel renders the split's selection below cut as one predicate.
func (sp forkSplit) moveSel(cut timelineRow) (string, []any) {
	sel := "turn_index >= ? AND (turn_index, item_index) < (?, ?)"
	args := []any{sp.fromTurn, cut.turn, cut.item}
	if sp.where != "" {
		sel += " AND (" + sp.where + ")"
		args = append(args, sp.args...)
	}
	return sel, args
}

// splitShownRowsTx runs before threadID reverts or deletes the rows sp
// selects. The rows its readers show move to a holder, which every reader
// then reads right before threadID, with its cut there: a new holder the
// readers read at the depth they read threadID, reading threadID one level
// further, or the one they already read there (holderForTx). The rows the
// readers do not show stay for the caller to remove. w is threadID's
// write: the rows leave its cards. It returns how many rows it took.
func splitShownRowsTx(tx *sql.Tx, w *cardWrite, threadID string, sp forkSplit) (int, error) {
	first, found, err := firstOwnRowTx(tx, threadID, sp)
	if err != nil {
		return 0, err
	}
	bound := first
	switch {
	case sp.keep != nil:
		bound = *sp.keep
	case !found:
		return 0, nil
	}
	readers, err := readerLevelsTx(tx, threadID, bound)
	if err != nil || len(readers) == 0 {
		return 0, err
	}
	maxCut := readers[0].cut
	for _, r := range readers[1:] {
		if rowBefore(maxCut, r.cut) {
			maxCut = r.cut
		}
	}
	// The turn rows a reader reads through threadID from keep on: threadID's
	// own ones below the reader's cut turn (forkTurnVisibleSQL). Those
	// threadID inherits the reader reads through the deeper levels as before.
	var turns []int
	if sp.keep != nil {
		if turns, err = queryInts(tx, `SELECT turn_index FROM turns
			 WHERE thread_id = ? AND turn_index >= ? AND turn_index < ? ORDER BY turn_index`,
			threadID, sp.keep.turn, maxCut.turn); err != nil {
			return 0, fmt.Errorf("store: read turns %s's forks read: %w", threadID, err)
		}
	}
	var held []forkLevel
	for _, r := range readers {
		if (found && rowBefore(first, r.cut)) || (len(turns) > 0 && turns[0] < r.cut.turn) {
			held = append(held, r)
		}
	}
	taken := 0
	if len(held) > 0 {
		if taken, err = holdSplitRowsTx(tx, w, threadID, sp, held, maxCut, turns); err != nil {
			return 0, err
		}
	}
	if sp.keep == nil {
		return taken, nil
	}
	list, err := levelList(readers)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE thread_fork_lineage SET cut_turn_index = ?, cut_item_index = ?
		 WHERE ancestor_id = ? AND thread_id IN (SELECT json_extract(value, '$[0]') FROM json_each(?))
		   AND (cut_turn_index, cut_item_index) > (?, ?)`,
		sp.keep.turn, sp.keep.item, threadID, list, sp.keep.turn, sp.keep.item); err != nil {
		return 0, fmt.Errorf("store: end %s for its forks: %w", threadID, err)
	}
	if *sp.keep != nothingKept {
		return taken, nil
	}
	// The readers read nothing of threadID now, unless through its hides,
	// which apply to the levels behind: without any, their levels on it go.
	var hides bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_fork_hidden WHERE thread_id = ?)`, threadID).Scan(&hides); err != nil {
		return 0, fmt.Errorf("store: probe the hides of %s: %w", threadID, err)
	}
	if hides {
		return taken, nil
	}
	ids := make([]string, len(readers))
	for i, r := range readers {
		ids[i] = r.reader
	}
	return taken, dropLevelsTx(tx, w, threadID, ids)
}

// holdSplitRowsTx moves the rows sp selects below maxCut to a holder and
// gives it to held. A row whose id or position the holder already holds
// stays (holderHoldsSQL). The order is load-bearing: a new holder's
// imported chunks attach before it holds a local row or payload (the chunk
// overlap triggers), and its payloads and the launch copies arrive before
// any reader reads it (trg_payload_chunks_shown_insert).
func holdSplitRowsTx(tx *sql.Tx, w *cardWrite, threadID string, sp forkSplit, held []forkLevel, maxCut timelineRow, turns []int) (int, error) {
	holder, deepest, reused, err := holderForTx(tx, threadID, held)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, fmt.Errorf("store: defer foreign keys for %s's holder: %w", threadID, err)
	}
	span, spanArgs := sp.moveSel(maxCut)
	sel := span + " AND NOT " + holderHoldsSQL
	selArgs := append(append([]any{}, spanArgs...), holderHoldsArgs(holder)...)
	imported, err := holdImportedRowsTx(tx, w, threadID, holder, reused, sp.fromTurn, sel, selArgs)
	if err != nil {
		return 0, err
	}
	moved, err := queryHeldRows(tx, `SELECT id, COALESCE(payload_id, ''), COALESCE(input_payload_id, ''), completion_of
		  FROM items WHERE thread_id = ? AND `+sel, append([]any{threadID}, selArgs...)...)
	if err != nil {
		return 0, fmt.Errorf("store: read the rows %s's forks read: %w", threadID, err)
	}
	var launches []string
	if err := withHistoryBulkLoadTx(tx, holder, func() error {
		var err error
		launches, err = holdSettledLaunchesTx(tx, threadID, holder, span, spanArgs)
		return err
	}); err != nil {
		return 0, err
	}
	drop, err := holdPayloadsTx(tx, threadID, holder, append(moved, imported.localPayloads...), sel, selArgs)
	if err != nil {
		return 0, err
	}
	if !reused {
		if err := insertLevelTx(tx, holder, held, deepest); err != nil {
			return 0, err
		}
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return withHistoryBulkLoadTx(tx, holder, func() error {
			return moveItemRowsTx(tx, w, threadID, holder, sel, selArgs, len(moved))
		})
	}); err != nil {
		return 0, err
	}
	if err := dropPayloadsTx(tx, threadID, drop); err != nil {
		return 0, err
	}
	ids := make([]string, len(moved))
	for i, row := range moved {
		ids[i] = row.id
	}
	list, err := jsonList(ids)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM message_anchors WHERE thread_id = ? AND user_item_id IN (SELECT value FROM json_each(?))`,
		threadID, list); err != nil {
		return 0, fmt.Errorf("store: drop the anchors of the rows %s gave its holder: %w", threadID, err)
	}
	if err := dropUnservedHolderStampsTx(tx, holder); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE thread_search_rows SET thread_id = ?
		 WHERE thread_id = ? AND source = '`+ThreadSearchSourceItem+`' AND item_id IN (SELECT value FROM json_each(?))`,
		holder, threadID, list); err != nil {
		return 0, fmt.Errorf("store: move the search rows of the rows %s gave its holder: %w", threadID, err)
	}
	if err := ownHeldAttachmentsTx(tx, holder, threadID); err != nil {
		return 0, err
	}
	if sp.keep != nil {
		if err := holdTurnsTx(tx, threadID, holder, turns, sp.turnsKeptThrough); err != nil {
			return 0, err
		}
	}
	taken := append(ids, imported.ids...)
	if err := markStraddledAnchorsTx(tx, holder, threadID, append(slices.Clip(taken), launches...)); err != nil {
		return 0, err
	}
	return len(moved) + len(imported.ids), hideHeldRowsTx(tx, holder, taken)
}

// dropUnservedHolderStampsTx drops the stamps a holder took with its rows
// that serve no read: dirty and readTime ones. A clean stamp moved with
// its anchor's rows and counts what the holder's readers read under it
// (fork_walked.go); nothing recomputes a holder's stamps
// (subagentStampsFrozen), and a copy the holder takes gets none.
func dropUnservedHolderStampsTx(tx *sql.Tx, holder string) error {
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ? AND state <> `+aggCleanLiteral, holder); err != nil {
		return fmt.Errorf("store: drop the unserved cards holder %s took: %w", holder, err)
	}
	return nil
}

// firstOwnRowTx is the first of threadID's own rows, local or imported,
// that sp selects.
func firstOwnRowTx(tx *sql.Tx, threadID string, sp forkSplit) (timelineRow, bool, error) {
	sel := timelineSelection{
		Columns: timelineIDColumns,
		Turn:    "?", TurnArgs: []any{sp.fromTurn}, FromTurn: true,
		OrderBy: "turn_index, item_index",
		Limit:   1,
	}
	if sp.where != "" {
		sel.Where, sel.WhereArgs = "("+sp.where+")", sp.args
	}
	query, args := ownTimelineArms(threadID, sel)
	var row timelineRow
	err := tx.QueryRow(query, args...).Scan(&row.id, &row.turn, &row.item)
	if errors.Is(err, sql.ErrNoRows) {
		return timelineRow{}, false, nil
	}
	if err != nil {
		return timelineRow{}, false, fmt.Errorf("store: read the first row %s takes: %w", threadID, err)
	}
	return row, true, nil
}

// readerLevelsSQL lists the lineage rows that read ?1 past (?2, ?3): one
// range of idx_thread_fork_lineage_ancestor, empty for a position after
// every reader's cut.
const readerLevelsSQL = `SELECT thread_id, depth, cut_turn_index, cut_item_index FROM thread_fork_lineage
		 WHERE ancestor_id = ? AND (cut_turn_index, cut_item_index) > (?, ?)`

// readerLevelsTx lists the lineage rows that read threadID past after.
func readerLevelsTx(tx *sql.Tx, threadID string, after timelineRow) ([]forkLevel, error) {
	out, err := queryLevels(tx, readerLevelsSQL, threadID, after.turn, after.item)
	if err != nil {
		return nil, fmt.Errorf("store: list the forks that read %s: %w", threadID, err)
	}
	return out, nil
}

// rowBefore reports whether a sits before b in timeline order.
func rowBefore(a, b timelineRow) bool {
	return a.turn < b.turn || (a.turn == b.turn && a.item < b.item)
}

// createHolderTx creates an empty holder of threadID's rows. It has no
// project, workspace, session or title index row: nothing runs in it,
// nothing lists it and no project delete waits for it.
func createHolderTx(tx *sql.Tx, threadID string) (string, error) {
	id := entityid.New()
	now := time.Now().UnixMilli()
	result, err := tx.Exec(`INSERT INTO threads (id, title, provider, model, workspace_path, mode,
		    reasoning_effort, created_at, updated_at, fork_source_thread_id, fork_source_title)
		 SELECT ?, title, provider, model, '', ?, reasoning_effort, ?, ?, id, title
		   FROM threads WHERE id = ?`, id, threadmode.ModeHolder, now, now, threadID)
	if err != nil {
		return "", fmt.Errorf("store: create a holder for %s: %w", threadID, err)
	}
	if err := requireRowsAffected(result, "store: create a holder for "+threadID); err != nil {
		return "", err
	}
	return id, nil
}

// heldRow is a local row a split moves.
type heldRow struct {
	id, payloadID, inputPayloadID, completionOf string
}

func queryHeldRows(tx *sql.Tx, query string, args ...any) ([]heldRow, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []heldRow
	for rows.Next() {
		var row heldRow
		if err := rows.Scan(&row.id, &row.payloadID, &row.inputPayloadID, &row.completionOf); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, row)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// moveItemRowsTx moves threadID's rows sel selects to holder. The rows
// leave threadID's cards; the stamp moves once for all of them, rows and
// epoch, as the per-row trigger would have moved it, and before them, so
// the rows the trigger restamps above each (trg_items_rev_update) are
// served at the new revision. The caller holds both threads'
// history_bulk_load.
func moveItemRowsTx(tx *sql.Tx, w *cardWrite, threadID, holder, sel string, selArgs []any, want int) error {
	if want == 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE threads SET history_rev = history_rev + ?, history_epoch = history_epoch + ? WHERE id = ?`,
		want, want, threadID); err != nil {
		return fmt.Errorf("store: stamp the rows %s moves: %w", threadID, err)
	}
	rows, err := tx.Query(`UPDATE items SET thread_id = ? WHERE thread_id = ? AND `+sel+` RETURNING `+subagentRowColumns(""),
		append([]any{holder, threadID}, selArgs...)...)
	if err != nil {
		return fmt.Errorf("store: move the rows %s's forks read: %w", threadID, err)
	}
	var moved []subagentRow
	for rows.Next() {
		row, err := scanSubagentRow(rows)
		if err != nil {
			return errors.Join(fmt.Errorf("store: scan a row %s moved: %w", threadID, err), rows.Close())
		}
		moved = append(moved, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: move the rows %s's forks read: %w", threadID, err)
	}
	if len(moved) != want {
		return fmt.Errorf("store: move the rows %s's forks read: moved %d of %d", threadID, len(moved), want)
	}
	for _, row := range moved {
		w.deleted(row)
	}
	return nil
}

// holdSettledLaunchesTx gives holder a settled copy of each background
// launch threadID keeps whose completions all leave it
// (launchesLosingCompletionTx): the completions in span move to the holder
// or, when it holds their id or position, are the caller's to remove.
// Either leaves the launch in threadID settled with no completion
// (background_settle_triggers.go); the forks that read the completion from
// the holder read the launch there, with it. A launch the holder already
// holds is the one they read. It returns the ids of the copies.
func holdSettledLaunchesTx(tx *sql.Tx, threadID, holder, span string, spanArgs []any) ([]string, error) {
	targets, err := queryIDs(tx, `SELECT completion_of FROM items WHERE thread_id = ? AND completion_of <> '' AND `+span,
		append([]any{threadID}, spanArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: read the completions %s's split takes: %w", threadID, err)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	list, err := jsonList(targets)
	if err != nil {
		return nil, err
	}
	args := append([]any{threadID, list}, spanArgs...)
	launches, err := scanInheritedRows(tx, threadID, `SELECT id, COALESCE(payload_id, ''), COALESCE(input_payload_id, ''), thread_id
		  FROM items WHERE thread_id = ? AND id IN (SELECT value FROM json_each(?))
		   AND kind = 'tool_call' AND status = 'running' AND is_background = 1
		   AND NOT (`+span+`) AND NOT `+holderHoldsSQL, append(args, holderHoldsArgs(holder)...))
	if err != nil || len(launches) == 0 {
		return nil, err
	}
	ids := make([]string, len(launches))
	for i, row := range launches {
		ids[i] = row.id
	}
	losing, err := launchesLosingCompletionTx(tx, threadID, ids, "NOT ("+span+")", spanArgs)
	if err != nil || len(losing) == 0 {
		return nil, err
	}
	keep := make(map[string]bool, len(losing))
	for _, id := range losing {
		keep[id] = true
	}
	var copies []inheritedRow
	var copied []string
	for _, row := range launches {
		if keep[row.id] {
			copies = append(copies, row)
			copied = append(copied, row.id)
		}
	}
	return copied, snapshotRowsTx(tx, holder, copies)
}
