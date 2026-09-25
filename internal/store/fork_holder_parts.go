package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
)

// The parts of a split (splitShownRowsTx) that the local rows' move does
// not carry: imported rows, turn rows and the holder's hides; and the
// holder a hide of inherited rows makes (holdHiddenRowsTx).

// holdHiddenRowsTx runs before threadID hides rows it inherits (rows) that
// readers show through it. A threadID hide applies to every level behind
// threadID, so a new holder takes copies of the rows, which it hides
// behind it (snapshotRowsTx), and each reader reads the holder at the
// depth it read threadID, with its cut there: the readers read the same
// rows before and after.
func holdHiddenRowsTx(tx *sql.Tx, threadID string, rows []inheritedRow, readers []forkLevel) error {
	deepest, err := requireLevelRoomTx(tx, threadID, readers)
	if err != nil {
		return err
	}
	holder, err := createHolderTx(tx, threadID)
	if err != nil {
		return err
	}
	if err := withHistoryBulkLoadTx(tx, holder, func() error { return snapshotRowsTx(tx, holder, rows) }); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, holder); err != nil {
		return fmt.Errorf("store: drop the cards of the rows %s's holder copied: %w", threadID, err)
	}
	return insertLevelTx(tx, holder, readers, deepest)
}

// queryLevels reads lineage rows as (reader, depth, cut turn, cut item).
func queryLevels(tx *sql.Tx, query string, args ...any) ([]forkLevel, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []forkLevel
	for rows.Next() {
		var l forkLevel
		if err := rows.Scan(&l.reader, &l.depth, &l.cut.turn, &l.cut.item); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, l)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// heldImports is what a split took from a thread's imported history.
type heldImports struct {
	ids []string
	// localPayloads name the payloads the taken rows render, which the
	// thread may hold a local row of (ensureLocalPayloadTx).
	localPayloads []heldRow
}

// holdImportedRowsTx gives holder the imported rows of threadID that sel
// selects. The holder references each of their chunks and overrides the
// chunks' other rows, so it shows exactly those; threadID overrides them,
// gives their search rows to the holder, restamps what it shows above them
// (importedRowsLeftTx) and detaches the chunks it now overrides whole
// (detachOverriddenChunksTx). It runs before the holder holds any local
// row or payload, which the chunk overlap triggers refuse to shadow.
func holdImportedRowsTx(tx *sql.Tx, w *cardWrite, threadID, holder string, fromTurn int, sel string, selArgs []any) (heldImports, error) {
	rows, err := tx.Query(`SELECT items.id, refs.chunk_id, refs.min_turn_index, refs.max_turn_index,
		       COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), items.parent_id
		  FROM thread_import_chunks refs
		  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
		 WHERE refs.thread_id = ? AND refs.max_turn_index >= ? AND `+sel+`
		   AND `+importedNotOverridden+`
		 ORDER BY refs.chunk_order`, append([]any{threadID, fromTurn}, selArgs...)...)
	if err != nil {
		return heldImports{}, fmt.Errorf("store: read the imported rows %s's forks read: %w", threadID, err)
	}
	type chunkRef struct {
		id       string
		min, max sql.NullInt64
	}
	var held heldImports
	var chunks []chunkRef
	var parents []string
	seen := make(map[string]bool)
	for rows.Next() {
		var ref chunkRef
		var row heldRow
		var parent string
		if err := rows.Scan(&row.id, &ref.id, &ref.min, &ref.max, &row.payloadID, &row.inputPayloadID, &parent); err != nil {
			return heldImports{}, errors.Join(fmt.Errorf("store: scan an imported row %s's forks read: %w", threadID, err), rows.Close())
		}
		held.ids = append(held.ids, row.id)
		held.localPayloads = append(held.localPayloads, row)
		if parent != "" && !slices.Contains(parents, parent) {
			parents = append(parents, parent)
		}
		if !seen[ref.id] {
			seen[ref.id] = true
			chunks = append(chunks, ref)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return heldImports{}, fmt.Errorf("store: iterate the imported rows %s's forks read: %w", threadID, err)
	}
	if len(held.ids) == 0 {
		return heldImports{}, nil
	}
	chunkIDs := make([]string, len(chunks))
	for order, ref := range chunks {
		chunkIDs[order] = ref.id
		if _, err := tx.Exec(`INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id, min_turn_index, max_turn_index)
			 VALUES (?, ?, ?, ?, ?)`, holder, order, ref.id, ref.min, ref.max); err != nil {
			return heldImports{}, fmt.Errorf("store: give %s's holder imported chunk %s: %w", threadID, ref.id, err)
		}
	}
	chunkList, err := jsonList(chunkIDs)
	if err != nil {
		return heldImports{}, err
	}
	idList, err := jsonList(held.ids)
	if err != nil {
		return heldImports{}, err
	}
	if _, err := tx.Exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id)
		SELECT ?, i.id FROM import_history_items i
		 WHERE i.chunk_id IN (SELECT value FROM json_each(?)) AND i.id NOT IN (SELECT value FROM json_each(?))`,
		holder, chunkList, idList); err != nil {
		return heldImports{}, fmt.Errorf("store: limit %s's holder to the imported rows it takes: %w", threadID, err)
	}
	if _, err := tx.Exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id) SELECT ?, value FROM json_each(?)`,
		threadID, idList); err != nil {
		return heldImports{}, fmt.Errorf("store: override the imported rows %s gave its holder: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE thread_search_rows SET thread_id = ?
		 WHERE thread_id = ? AND source = '`+ThreadSearchSourceImport+`' AND item_id IN (SELECT value FROM json_each(?))`,
		holder, threadID, idList); err != nil {
		return heldImports{}, fmt.Errorf("store: move the search rows of %s's imported rows: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE threads SET history_rev = history_rev + ?, history_epoch = history_epoch + ? WHERE id = ?`,
		len(held.ids), len(held.ids), threadID); err != nil {
		return heldImports{}, fmt.Errorf("store: stamp the imported rows %s gave its holder: %w", threadID, err)
	}
	if err := importedRowsLeftTx(tx, w, threadID, parents); err != nil {
		return heldImports{}, err
	}
	if err := detachOverriddenChunksTx(tx, threadID, chunkIDs); err != nil {
		return heldImports{}, err
	}
	return held, nil
}

// holdTurnsTx gives holder threadID's own turn rows that its readers read
// through it (turns): those after turnsKeptThrough move, and the rest, which
// threadID keeps, are copied under the holder's turn ids, as a fork copies
// the turn rows it owns.
func holdTurnsTx(tx *sql.Tx, threadID, holder string, turns []int, turnsKeptThrough int) error {
	if len(turns) == 0 {
		return nil
	}
	list, err := jsonIntList(turns)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ?2 || ':' || turn_index, ?2, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
		   FROM turns
		  WHERE thread_id = ?1 AND turn_index <= ?4 AND turn_index IN (SELECT value FROM json_each(?3))`,
		threadID, holder, list, turnsKeptThrough); err != nil {
		return fmt.Errorf("store: copy the turns %s's forks read: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE turns SET thread_id = ?2
		 WHERE thread_id = ?1 AND turn_index > ?4 AND turn_index IN (SELECT value FROM json_each(?3))`,
		threadID, holder, list, turnsKeptThrough); err != nil {
		return fmt.Errorf("store: move the turns %s's forks read: %w", threadID, err)
	}
	return nil
}

// hideHeldRowsTx has holder hide the ids it holds below keep, or all of
// them when keep is nil: the thread that gave them is still read before
// keep, and a row it writes there later under one of those ids is not
// the forks' history (trg_items_fork_snapshot).
func hideHeldRowsTx(tx *sql.Tx, holder string, keep *timelineRow) error {
	bound := timelineRow{turn: math.MaxInt32, item: math.MaxInt32}
	if keep != nil {
		bound = *keep
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_fork_hidden (thread_id, item_id)
		SELECT ?1, id FROM items WHERE thread_id = ?1 AND (turn_index, item_index) < (?2, ?3)
		UNION ALL
		SELECT ?1, items.id FROM thread_import_chunks refs
		  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
		 WHERE refs.thread_id = ?1 AND (items.turn_index, items.item_index) < (?2, ?3)
		   AND `+importedNotOverridden, holder, bound.turn, bound.item); err != nil {
		return fmt.Errorf("store: hide the rows holder %s holds: %w", holder, err)
	}
	return nil
}

// queryInts reads one integer column.
func queryInts(q sqlQueryer, query string, args ...any) ([]int, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, v)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
