package store

import (
	"database/sql"
	"fmt"
)

// ensureLocalPayloadTx gives one thread a mutable payload overlay when the
// requested payload currently comes from immutable imported history. Copying
// it is representation-only: timeline_payloads resolves to the same bytes
// before and after, so callers bump history_rev only for the mutation that
// follows.
func ensureLocalPayloadTx(tx *sql.Tx, threadID, payloadID, label string) error {
	var local bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?)`, threadID, payloadID).Scan(&local); err != nil {
		return fmt.Errorf("%s inspect local payload: %w", label, err)
	}
	if local {
		return nil
	}

	// No local row exists, so the imported row is the whole logical payload.
	result, err := tx.Exec(
		`INSERT OR IGNORE INTO payloads (
		    thread_id, id, kind, meta, data, created_at, preview_spans, spans
		 )
		 SELECT refs.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
		   FROM import_history_payloads p
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		  WHERE p.id = ? AND refs.thread_id = ?`,
		payloadID, threadID,
	)
	if err != nil {
		return fmt.Errorf("%s copy imported payload %s/%s: %w", label, threadID, payloadID, err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("%s count copied imported payload %s/%s: %w", label, threadID, payloadID, err)
	}

	var exists int
	if err := tx.QueryRow(
		`SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?`,
		threadID, payloadID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("%s payload %s/%s: %w", label, threadID, payloadID, err)
	}
	return nil
}

// localizeImportedItemTx copies one immutable imported item into the thread's
// mutable overlay. The explicit override is inserted first because the items
// trigger rejects accidental shadowing. Item INSERT history accounting is
// suppressed while the representation changes; the caller's subsequent
// UPDATE or DELETE advances the public stamp exactly once.
func localizeImportedItemTx(tx *sql.Tx, threadID, itemID, label string) (bool, error) {
	var payloadID, inputPayloadID string
	err := tx.QueryRow(
		`SELECT COALESCE(imported.payload_id, ''), COALESCE(imported.input_payload_id, '')
		   FROM import_history_items imported
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
		  WHERE imported.id = ? AND refs.thread_id = ?
		    AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o
		      WHERE o.thread_id = refs.thread_id AND o.item_id = imported.id)`,
		itemID, threadID,
	).Scan(&payloadID, &inputPayloadID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s find imported item %s/%s: %w", label, threadID, itemID, err)
	}
	for _, id := range []string{payloadID, inputPayloadID} {
		if id != "" {
			if err := ensureLocalPayloadTx(tx, threadID, id, label); err != nil {
				return false, err
			}
		}
	}
	if err := setHistoryBulkLoadTx(tx, threadID, true, label); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO thread_import_item_overrides (thread_id, item_id) VALUES (?, ?)`,
		threadID, itemID,
	); err != nil {
		return false, fmt.Errorf("%s mark imported item override %s/%s: %w", label, threadID, itemID, err)
	}
	result, err := tx.Exec(
		`INSERT INTO items (
		    id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of,
		    tool_name, decision, meta, created_at, updated_at
		 )
		 SELECT imported.id, ?, imported.turn_index, imported.item_index,
		        imported.kind, imported.role, imported.status, imported.summary,
		        imported.payload_id, imported.input_payload_id, imported.parent_id,
		        imported.is_background, imported.completion_of, imported.tool_name,
		        imported.decision, imported.meta, imported.created_at, imported.updated_at
		   FROM import_history_items imported
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
		  WHERE imported.id = ? AND refs.thread_id = ?`,
		threadID, itemID, threadID,
	)
	if err != nil {
		return false, fmt.Errorf("%s copy imported item %s/%s: %w", label, threadID, itemID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("%s copy imported item %s/%s", label, threadID, itemID)); err != nil {
		return false, err
	}
	// The override moves this item from the import arm to the item arm, so its
	// index row moves with it. The caller's mutation re-indexes the new text.
	if err := deleteThreadSearchItemsTx(tx, threadID, []string{itemID}); err != nil {
		return false, err
	}
	if err := indexItemByIDTx(tx, threadID, itemID); err != nil {
		return false, err
	}
	if err := setHistoryBulkLoadTx(tx, threadID, false, label); err != nil {
		return false, err
	}
	return true, nil
}

func setHistoryBulkLoadTx(tx *sql.Tx, threadID string, enabled bool, label string) error {
	from, to := 0, 1
	if !enabled {
		from, to = 1, 0
	}
	result, err := tx.Exec(
		`UPDATE threads SET history_bulk_load = ? WHERE id = ? AND history_bulk_load = ?`,
		to, threadID, from,
	)
	if err != nil {
		return fmt.Errorf("%s set history materialization flag for %s: %w", label, threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("%s set history materialization flag for %s", label, threadID))
}

func requireMutableItemTx(tx *sql.Tx, threadID, itemID, label string) error {
	var exists int
	err := tx.QueryRow(
		`SELECT 1 FROM items WHERE thread_id = ? AND id = ?`,
		threadID, itemID,
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("%s inspect local item %s/%s: %w", label, threadID, itemID, err)
	}
	localized, err := localizeImportedItemTx(tx, threadID, itemID, label)
	if err != nil {
		return err
	}
	if !localized {
		return fmt.Errorf("%s %s/%s: %w", label, threadID, itemID, sql.ErrNoRows)
	}
	return nil
}
