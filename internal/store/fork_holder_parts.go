package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
)

// The parts of a split (splitShownRowsTx) that the local rows' move does
// not carry: imported rows, turn rows, payloads and the holder's hides.

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
// selects. A chunk the holder references already gives it its rows once
// the holder stops overriding them. A new holder references each other
// chunk and overrides the chunk's other rows, so it shows exactly those;
// it runs before the holder holds any local row or payload, which the
// chunk overlap triggers refuse to shadow. A reused holder may hold such
// rows, so it takes copies of the rows of those chunks instead
// (snapshotRowsTx). threadID overrides the rows, gives the holder their
// search rows (drops them for the copies, which index their own),
// restamps what it shows above them (importedRowsLeftTx) and detaches the
// chunks it now overrides whole (detachOverriddenChunksTx).
func holdImportedRowsTx(tx *sql.Tx, w *cardWrite, threadID, holder string, reused bool, fromTurn int, sel string, selArgs []any) (heldImports, error) {
	rows, err := tx.Query(`SELECT items.id, refs.chunk_id, refs.min_turn_index, refs.max_turn_index,
		       COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), items.parent_id,
		       EXISTS (SELECT 1 FROM thread_import_chunks own WHERE own.thread_id = ? AND own.chunk_id = refs.chunk_id)
		  FROM thread_import_chunks refs
		  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
		 WHERE refs.thread_id = ? AND refs.max_turn_index >= ? AND `+sel+`
		   AND `+importedNotOverridden+`
		 ORDER BY refs.chunk_order`, append([]any{holder, threadID, fromTurn}, selArgs...)...)
	if err != nil {
		return heldImports{}, fmt.Errorf("store: read the imported rows %s's forks read: %w", threadID, err)
	}
	var held heldImports
	var attach []heldChunk
	var parents, touched, referenced, shared, copied []string
	var copies []inheritedRow
	for rows.Next() {
		var ref heldChunk
		var row heldRow
		var parent string
		var inHolder bool
		if err := rows.Scan(&row.id, &ref.id, &ref.min, &ref.max, &row.payloadID, &row.inputPayloadID, &parent, &inHolder); err != nil {
			return heldImports{}, errors.Join(fmt.Errorf("store: scan an imported row %s's forks read: %w", threadID, err), rows.Close())
		}
		held.ids = append(held.ids, row.id)
		held.localPayloads = append(held.localPayloads, row)
		if parent != "" && !slices.Contains(parents, parent) {
			parents = append(parents, parent)
		}
		newChunk := !slices.Contains(touched, ref.id)
		if newChunk {
			touched = append(touched, ref.id)
		}
		switch {
		case inHolder:
			referenced = append(referenced, row.id)
			shared = append(shared, row.id)
		case reused:
			copies = append(copies, inheritedRow{id: row.id, payloadID: row.payloadID, inputPayloadID: row.inputPayloadID, owner: threadID})
			copied = append(copied, row.id)
		default:
			shared = append(shared, row.id)
			if newChunk {
				attach = append(attach, ref)
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return heldImports{}, fmt.Errorf("store: iterate the imported rows %s's forks read: %w", threadID, err)
	}
	if len(held.ids) == 0 {
		return heldImports{}, nil
	}
	idList, err := jsonList(held.ids)
	if err != nil {
		return heldImports{}, err
	}
	if len(attach) > 0 {
		if err := attachHeldChunksTx(tx, threadID, holder, attach, idList); err != nil {
			return heldImports{}, err
		}
	}
	if len(referenced) > 0 {
		list, err := jsonList(referenced)
		if err != nil {
			return heldImports{}, err
		}
		if _, err := tx.Exec(`DELETE FROM thread_import_item_overrides WHERE thread_id = ? AND item_id IN (SELECT value FROM json_each(?))`,
			holder, list); err != nil {
			return heldImports{}, fmt.Errorf("store: give %s's holder the imported rows it references: %w", threadID, err)
		}
	}
	if err := withHistoryBulkLoadTx(tx, holder, func() error { return snapshotRowsTx(tx, holder, copies) }); err != nil {
		return heldImports{}, err
	}
	if _, err := tx.Exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id) SELECT ?, value FROM json_each(?)`,
		threadID, idList); err != nil {
		return heldImports{}, fmt.Errorf("store: override the imported rows %s gave its holder: %w", threadID, err)
	}
	if len(shared) > 0 {
		list, err := jsonList(shared)
		if err != nil {
			return heldImports{}, err
		}
		if _, err := tx.Exec(`UPDATE thread_search_rows SET thread_id = ?
			 WHERE thread_id = ? AND source = '`+ThreadSearchSourceImport+`' AND item_id IN (SELECT value FROM json_each(?))`,
			holder, threadID, list); err != nil {
			return heldImports{}, fmt.Errorf("store: move the search rows of %s's imported rows: %w", threadID, err)
		}
	}
	if err := deleteThreadSearchItemsTx(tx, threadID, copied); err != nil {
		return heldImports{}, err
	}
	if _, err := tx.Exec(`UPDATE threads SET history_rev = history_rev + ?, history_epoch = history_epoch + ? WHERE id = ?`,
		len(held.ids), len(held.ids), threadID); err != nil {
		return heldImports{}, fmt.Errorf("store: stamp the imported rows %s gave its holder: %w", threadID, err)
	}
	if err := importedRowsLeftTx(tx, w, threadID, parents); err != nil {
		return heldImports{}, err
	}
	if err := detachOverriddenChunksTx(tx, threadID, touched); err != nil {
		return heldImports{}, err
	}
	return held, nil
}

// heldChunk is an imported chunk reference a split gives a holder.
type heldChunk struct {
	id       string
	min, max sql.NullInt64
}

// attachHeldChunksTx has holder reference chunks after the ones it
// references, overriding every row of them but the ones it takes (taken,
// a JSON array).
func attachHeldChunksTx(tx *sql.Tx, threadID, holder string, chunks []heldChunk, taken string) error {
	ids := make([]string, len(chunks))
	for i, ref := range chunks {
		ids[i] = ref.id
		if _, err := tx.Exec(`INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id, min_turn_index, max_turn_index)
			 VALUES (?1, (SELECT COALESCE(MAX(chunk_order) + 1, 0) FROM thread_import_chunks WHERE thread_id = ?1), ?2, ?3, ?4)`,
			holder, ref.id, ref.min, ref.max); err != nil {
			return fmt.Errorf("store: give %s's holder imported chunk %s: %w", threadID, ref.id, err)
		}
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id)
		SELECT ?, i.id FROM import_history_items i
		 WHERE i.chunk_id IN (SELECT value FROM json_each(?)) AND i.id NOT IN (SELECT value FROM json_each(?))`,
		holder, list, taken); err != nil {
		return fmt.Errorf("store: limit %s's holder to the imported rows it takes: %w", threadID, err)
	}
	return nil
}

// holdTurnsTx gives holder threadID's own turn rows that its readers read
// through it (turns): those after turnsKeptThrough move, and the rest, which
// threadID keeps, are copied. Both take the holder's turn ids, as a fork's
// copies do: turn_id is a global key, and threadID's next turn at a moved
// index takes the same id again. A turn the holder already holds is the
// one its readers read, and threadID's stays for the caller to remove or
// keep.
func holdTurnsTx(tx *sql.Tx, threadID, holder string, turns []int, turnsKeptThrough int) error {
	if len(turns) == 0 {
		return nil
	}
	list, err := jsonIntList(turns)
	if err != nil {
		return err
	}
	const notHeld = ` AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = ?2 AND held.turn_index = turns.turn_index)`
	if _, err := tx.Exec(`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
		 SELECT ?2 || ':' || turn_index, ?2, turn_index, started_at, completed_at,
		    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
		   FROM turns
		  WHERE thread_id = ?1 AND turn_index <= ?4 AND turn_index IN (SELECT value FROM json_each(?3))`+notHeld,
		threadID, holder, list, turnsKeptThrough); err != nil {
		return fmt.Errorf("store: copy the turns %s's forks read: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE turns SET thread_id = ?2, turn_id = ?2 || ':' || turn_index
		 WHERE thread_id = ?1 AND turn_index > ?4 AND turn_index IN (SELECT value FROM json_each(?3))`+notHeld,
		threadID, holder, list, turnsKeptThrough); err != nil {
		return fmt.Errorf("store: move the turns %s's forks read: %w", threadID, err)
	}
	return nil
}

// hideHeldRowsTx has holder hide the ids it took (ids) below keep, or all
// of them when keep is nil: the thread that gave them is still read before
// keep, and a row it writes there later under one of those ids is not
// the forks' history (trg_items_fork_snapshot).
func hideHeldRowsTx(tx *sql.Tx, holder string, ids []string, keep *timelineRow) error {
	if len(ids) == 0 {
		return nil
	}
	bound := timelineRow{turn: math.MaxInt32, item: math.MaxInt32}
	if keep != nil {
		bound = *keep
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_fork_hidden (thread_id, item_id)
		SELECT ?1, id FROM items WHERE thread_id = ?1 AND id IN (SELECT value FROM json_each(?4))
		   AND (turn_index, item_index) < (?2, ?3)
		UNION ALL
		SELECT ?1, items.id FROM import_history_items items
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id AND refs.thread_id = ?1
		 WHERE items.id IN (SELECT value FROM json_each(?4)) AND (items.turn_index, items.item_index) < (?2, ?3)
		   AND `+importedNotOverridden, holder, bound.turn, bound.item, list); err != nil {
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

// holdPayloadsTx gives holder the payloads of the rows that move: a payload
// threadID still renders through a row it keeps is copied, any other one
// moves with its append chunks and edit snapshots. The rows still name
// threadID's payload until they move, under deferred foreign keys. A
// payload the holder already holds is the one its rows render, and those
// that move render it too: every row of threadID that renders a payload a
// fork shows went to the holder with the payload, and threadID changes
// the payload only after that (reownShownPayloadTx). threadID's copy goes
// once no row of threadID renders it.
func holdPayloadsTx(tx *sql.Tx, threadID, holder string, rows []heldRow, sel string, selArgs []any) ([]string, error) {
	seen := make(map[string]bool)
	var move, drop []string
	// The numbered parameters bind the thread and the payload; each ? of
	// the two selections takes the next number, so selArgs bind twice.
	kept := `SELECT EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND payload_id = ?2 AND NOT (` + sel + `))
		    OR EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND input_payload_id = ?2 AND NOT (` + sel + `))
		    OR ` + importedPayloadReferenceSQL("?1", "?2")
	for _, row := range rows {
		for _, id := range []string{row.payloadID, row.inputPayloadID} {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			var local, holds bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM payloads WHERE thread_id = ?1 AND id = ?3),
				    EXISTS (SELECT 1 FROM payloads WHERE thread_id = ?2 AND id = ?3)`, threadID, holder, id).Scan(&local, &holds); err != nil {
				return nil, fmt.Errorf("store: inspect payload %s/%s: %w", threadID, id, err)
			}
			if !local {
				continue
			}
			var shared bool
			args := append([]any{threadID, id}, selArgs...)
			args = append(args, selArgs...)
			if err := tx.QueryRow(kept, args...).Scan(&shared); err != nil {
				return nil, fmt.Errorf("store: probe payload %s/%s: %w", threadID, id, err)
			}
			switch {
			case holds && !shared:
				drop = append(drop, id)
			case holds:
			case shared:
				if err := copyPayloadFromTx(tx, holder, threadID, id); err != nil {
					return nil, err
				}
			default:
				move = append(move, id)
			}
		}
	}
	if len(move) == 0 {
		return drop, nil
	}
	list, err := jsonList(move)
	if err != nil {
		return nil, err
	}
	for _, table := range payloadTables {
		if _, err := tx.Exec(`UPDATE `+table.name+` SET thread_id = ? WHERE thread_id = ? AND `+table.column+
			` IN (SELECT value FROM json_each(?))`, holder, threadID, list); err != nil {
			return nil, fmt.Errorf("store: move %s's %s to its holder: %w", threadID, table.name, err)
		}
	}
	return drop, nil
}

// payloadTables are the tables that hold a payload, its row last.
var payloadTables = []struct{ name, column string }{
	{"payload_chunks", "payload_id"}, {"edit_file_snapshots", "payload_id"}, {"payloads", "id"},
}

// dropPayloadsTx deletes threadID's copies of payloads its holder holds,
// once no row of threadID renders them (holdPayloadsTx).
func dropPayloadsTx(tx *sql.Tx, threadID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	for _, table := range payloadTables {
		if _, err := tx.Exec(`DELETE FROM `+table.name+` WHERE thread_id = ? AND `+table.column+
			` IN (SELECT value FROM json_each(?))`, threadID, list); err != nil {
			return fmt.Errorf("store: drop %s's %s its holder holds: %w", threadID, table.name, err)
		}
	}
	return nil
}
