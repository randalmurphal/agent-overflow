package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Pointer-fork row ownership (docs/architecture/sqlite-store.md#pointer-forks).
//
// A fork reads the rows before its cut from the thread that holds them,
// and a row another thread shows never changes (fork_triggers.go). A
// thread that must stop showing such rows gives them to a holder, which
// every reader then reads them from (fork_holders.go). A thread takes its
// own snapshot of a row another thread holds only where the two must
// differ from then on (snapshotRowsTx): a fork's settled copy of a row
// still running in its source, a fork's copy of the rest of a turn its
// source is still running, a holder's copy of a launch whose completion it
// took, a fork's copy of an inherited row it writes.

// forkCopyBatch bounds how many ids one statement names.
const forkCopyBatch = 200

// inheritedRow is one row a fork reads from an ancestor, the owner.
type inheritedRow struct {
	id             string
	payloadID      string
	inputPayloadID string
	owner          string
}

// allLevels reads every lineage level.
const allLevels = ""

// inheritedRowColumns is the lineage arms' projection of an inheritedRow.
func inheritedRowColumns(string, string) string {
	return `items.id, COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), l.ancestor_id`
}

// queryInheritedRows reads the inherited rows viewer shows that sel
// selects. sel's Columns are inheritedRowColumns; a lookup sets KeyFirst or
// Turn as timelineArms requires, so the read costs the rows it names.
func queryInheritedRows(q sqlQueryer, viewer string, sel timelineSelection) ([]inheritedRow, error) {
	sel.Columns = inheritedRowColumns
	query, args := inheritedTimelineArms(viewer, allLevels, sel)
	return scanInheritedRows(q, viewer, query, args)
}

// scanInheritedRows runs an inherited-row read, keeping the first row of
// each id: a payload read can name one row twice.
func scanInheritedRows(q sqlQueryer, viewer, query string, args []any) ([]inheritedRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read inherited rows of %s: %w", viewer, err)
	}
	var out []inheritedRow
	seen := make(map[string]bool)
	for rows.Next() {
		var row inheritedRow
		if err := rows.Scan(&row.id, &row.payloadID, &row.inputPayloadID, &row.owner); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan inherited row of %s: %w", viewer, err), rows.Close())
		}
		if !seen[row.id] {
			seen[row.id] = true
			out = append(out, row)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate inherited rows of %s: %w", viewer, err)
	}
	return out, nil
}

// inheritedRowsByID resolves ids to the inherited rows the viewer shows.
// Ids the viewer owns or does not show are absent from the result.
func inheritedRowsByID(q sqlQueryer, viewer string, ids []string) ([]inheritedRow, error) {
	var out []inheritedRow
	for start := 0; start < len(ids); start += forkCopyBatch {
		clause, args := inClause("items.id", ids[start:min(start+forkCopyBatch, len(ids))])
		rows, err := queryInheritedRows(q, viewer, timelineSelection{KeyFirst: true, Where: clause, WhereArgs: args})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// inheritedPayloadRows reads the inherited rows the viewer shows that name
// payloadID as their payload or input payload.
func inheritedPayloadRows(q sqlQueryer, viewer, payloadID string) ([]inheritedRow, error) {
	return scanInheritedRows(q, viewer, inheritedPayloadRowArms(inheritedRowColumns("", ""), allLevels, "?", "?"),
		repeatArgs(payloadRowArmCount, []any{viewer, payloadID}))
}

// withHistoryBulkLoadTx runs body with threadID's stamp triggers frozen,
// restoring the prior flag. The caller writes the aggregate stamp.
func withHistoryBulkLoadTx(tx *sql.Tx, threadID string, body func() error) error {
	var prior int
	if err := tx.QueryRow(`SELECT history_bulk_load FROM threads WHERE id = ?`, threadID).Scan(&prior); err != nil {
		return fmt.Errorf("store: read history load flag of %s: %w", threadID, err)
	}
	if prior == 0 {
		if _, err := tx.Exec(`UPDATE threads SET history_bulk_load = 1 WHERE id = ?`, threadID); err != nil {
			return fmt.Errorf("store: set history load flag of %s: %w", threadID, err)
		}
	}
	if err := body(); err != nil {
		return err
	}
	if prior == 0 {
		if _, err := tx.Exec(`UPDATE threads SET history_bulk_load = 0 WHERE id = ?`, threadID); err != nil {
			return fmt.Errorf("store: clear history load flag of %s: %w", threadID, err)
		}
	}
	return nil
}

// snapshotRowsTx gives keeper its own copy of rows another thread holds
// (each row's owner): payloads first, because items reference them by
// foreign key, then keeper's hide of the ids, which masks the originals in
// every level keeper reads or is read before, then the rows, their search
// index rows, the cards the copies change (recomputeLocalizedCardsTx), the
// markers of the anchors above them (markRowAnchorsTx) and the ownership
// of the attachments they show. A copy keeps the row's id,
// position and content, so a read returns the same timeline before and
// after. The caller holds keeper's history_bulk_load and accounts for the
// stamp.
func snapshotRowsTx(tx *sql.Tx, keeper string, rows []inheritedRow) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		for _, payloadID := range []string{row.payloadID, row.inputPayloadID} {
			if payloadID == "" {
				continue
			}
			if err := copyPayloadFromTx(tx, keeper, row.owner, payloadID); err != nil {
				return err
			}
		}
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	if err := hideForkRowsTx(tx, keeper, ids); err != nil {
		return err
	}
	groups := make(map[string][]string)
	var owners []string
	for _, row := range rows {
		if _, seen := groups[row.owner]; !seen {
			owners = append(owners, row.owner)
		}
		groups[row.owner] = append(groups[row.owner], row.id)
	}
	// An owner shows each id from its items or, when it has no local row,
	// from its imported history; both are read by id.
	const copyColumns = `SELECT items.id, ?, items.turn_index, items.item_index, items.kind, items.role,
				       items.status, items.summary, items.payload_id, items.input_payload_id,
				       items.parent_id, items.is_background, items.completion_of, items.tool_name,
				       items.decision, items.meta, items.created_at, items.updated_at`
	for _, owner := range owners {
		group := groups[owner]
		for start := 0; start < len(group); start += forkCopyBatch {
			batch := group[start:min(start+forkCopyBatch, len(group))]
			clause, args := inClause("items.id", batch)
			binds := append([]any{keeper, owner}, args...)
			binds = append(append(binds, keeper, owner), args...)
			result, err := tx.Exec(itemInsertPrefix+`
				`+copyColumns+`
				  FROM items WHERE items.thread_id = ? AND `+clause+`
				UNION ALL
				`+copyColumns+`
				  FROM import_history_items items
				  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id
				 WHERE refs.thread_id = ? AND `+clause+` AND `+importedNotOverridden, binds...)
			if err != nil {
				return fmt.Errorf("store: copy rows into %s: %w", keeper, err)
			}
			copied, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: count rows copied into %s: %w", keeper, err)
			}
			if copied != int64(len(batch)) {
				return fmt.Errorf("store: copy rows into %s: copied %d of %d", keeper, copied, len(batch))
			}
		}
	}
	for _, id := range ids {
		if err := indexItemByIDTx(tx, keeper, id); err != nil {
			return err
		}
	}
	if err := recomputeLocalizedCardsTx(tx, keeper, ids, "store: copy rows:"); err != nil {
		return err
	}
	// The stamps of the levels keeper reads count the originals, not the
	// copies keeper may change (fork_walked.go).
	if err := markRowAnchorsTx(tx, keeper, keeper, ids); err != nil {
		return err
	}
	return ownRowAttachmentsTx(tx, keeper, rows)
}

// copyPayloadFromTx copies one payload from the thread that holds it, with
// its append chunks and edit snapshots. A payload threadID already holds is
// left alone.
func copyPayloadFromTx(tx *sql.Tx, threadID, owner, payloadID string) error {
	var held bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?)`, threadID, payloadID).Scan(&held); err != nil {
		return fmt.Errorf("store: inspect payload %s/%s: %w", threadID, payloadID, err)
	}
	if held {
		return nil
	}
	result, err := tx.Exec(`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at, preview_spans, spans)
		SELECT ?, id, kind, meta, data, created_at, preview_spans, spans FROM payloads WHERE thread_id = ? AND id = ?`,
		threadID, owner, payloadID)
	if err != nil {
		return fmt.Errorf("store: copy payload %s into %s: %w", payloadID, threadID, err)
	}
	copied, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count payload %s copied into %s: %w", payloadID, threadID, err)
	}
	if copied == 1 {
		if _, err := tx.Exec(`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at)
			SELECT ?, payload_id, chunk_index, start_offset, data, created_at FROM payload_chunks WHERE thread_id = ? AND payload_id = ?`,
			threadID, owner, payloadID); err != nil {
			return fmt.Errorf("store: copy payload %s chunks into %s: %w", payloadID, threadID, err)
		}
		if _, err := tx.Exec(`INSERT INTO edit_file_snapshots (thread_id, payload_id, path, content, created_at)
			SELECT ?, payload_id, path, content, created_at FROM edit_file_snapshots WHERE thread_id = ? AND payload_id = ?`,
			threadID, owner, payloadID); err != nil {
			return fmt.Errorf("store: copy payload %s edit snapshots into %s: %w", payloadID, threadID, err)
		}
		return nil
	}
	result, err = tx.Exec(`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at, preview_spans, spans)
		SELECT ?, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
		  FROM import_history_payloads p
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		 WHERE refs.thread_id = ? AND p.id = ?
		 LIMIT 1`, threadID, owner, payloadID)
	if err != nil {
		return fmt.Errorf("store: copy imported payload %s into %s: %w", payloadID, threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: copy payload %s of %s into %s", payloadID, owner, threadID)); err != nil {
		return err
	}
	return nil
}

func hideForkRowsTx(tx *sql.Tx, threadID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_fork_hidden (thread_id, item_id)
		SELECT ?, value FROM json_each(?)`, threadID, list); err != nil {
		return fmt.Errorf("store: hide inherited rows in %s: %w", threadID, err)
	}
	return nil
}

func queryIDs(q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

// shadowInheritedItemTx is copy-on-write for one inherited row. It reports
// false when threadID does not show itemID as an inherited row. The caller's
// mutation of the copy advances the stamp; a mutation of a row a thread
// forked from threadID shows is refused (trg_items_shown_update).
func shadowInheritedItemTx(tx *sql.Tx, threadID, itemID string) (bool, error) {
	rows, err := inheritedRowsByID(tx, threadID, []string{itemID})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return snapshotRowsTx(tx, threadID, rows)
	}); err != nil {
		return false, err
	}
	return true, nil
}

// shadowInheritedPayloadTx is copy-on-write for an inherited payload: it
// copies every inherited row that references it, which brings the payload.
// A payload row is only ever read beside the item that references it, so a
// payload copied without its rows would diverge from what they render.
func shadowInheritedPayloadTx(tx *sql.Tx, threadID, payloadID string) (bool, error) {
	rows, err := inheritedPayloadRows(tx, threadID, payloadID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return snapshotRowsTx(tx, threadID, rows)
	}); err != nil {
		return false, err
	}
	return true, nil
}

// payloadHolderTx names the thread whose row holds a payload threadID reads:
// threadID itself, or the nearest ancestor that holds it. A cache written to
// the holder (UpdatePayloadSpans, PutEditFileSnapshot) is seen by every fork
// that reads it.
func payloadHolderTx(tx *sql.Tx, threadID, payloadID string) (string, error) {
	var holder string
	var depth int
	err := tx.QueryRow(`SELECT ?, 0 WHERE `+payloadHeldBySQL("?", "?")+`
		UNION ALL
		SELECT l.ancestor_id, l.depth FROM thread_fork_lineage l
		 WHERE l.thread_id = ? AND `+payloadHeldBySQL("l.ancestor_id", "?")+`
		ORDER BY 2
		LIMIT 1`,
		threadID, threadID, payloadID, threadID, payloadID, threadID, payloadID, payloadID,
	).Scan(&holder, &depth)
	if errors.Is(err, sql.ErrNoRows) {
		return threadID, nil
	}
	if err != nil {
		return "", fmt.Errorf("store: resolve payload %s holder for %s: %w", payloadID, threadID, err)
	}
	return holder, nil
}
