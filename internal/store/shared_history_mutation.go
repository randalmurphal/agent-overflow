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
	result, err := tx.Exec(
		`INSERT OR IGNORE INTO payloads (
		    thread_id, id, kind, meta, data, created_at, preview_spans, spans
		 )
		 SELECT thread_id, id, kind, meta, data, created_at, preview_spans, spans
		   FROM timeline_payloads
		  WHERE thread_id = ? AND id = ?`,
		threadID, payloadID,
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
		   FROM thread_import_chunks refs
		   JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
		  WHERE refs.thread_id = ? AND imported.id = ?`,
		threadID, itemID,
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
		   FROM thread_import_chunks refs
		   JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
		  WHERE refs.thread_id = ? AND imported.id = ?`,
		threadID, threadID, itemID,
	)
	if err != nil {
		return false, fmt.Errorf("%s copy imported item %s/%s: %w", label, threadID, itemID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("%s copy imported item %s/%s", label, threadID, itemID)); err != nil {
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

// materializeSharedHistoryTx moves the active imported base of one thread
// into its mutable overlay without changing the logical timeline or history
// stamps. Structural operations such as truncation can then use the existing
// FK-cascading DELETE machinery atomically. The expensive path is reserved for
// those rare operations; ordinary live continuation remains an append-only
// overlay and keeps every shared chunk attached.
func materializeSharedHistoryTx(tx *sql.Tx, threadID, label string) error {
	var chunkCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM thread_import_chunks WHERE thread_id = ?`,
		threadID,
	).Scan(&chunkCount); err != nil {
		return fmt.Errorf("%s count shared history for %s: %w", label, threadID, err)
	}
	if chunkCount == 0 {
		return nil
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO payloads (
		    thread_id, id, kind, meta, data, created_at, preview_spans, spans
		 )
		 SELECT refs.thread_id, payload.id, payload.kind, payload.meta, payload.data,
		        payload.created_at, payload.preview_spans, payload.spans
		   FROM thread_import_chunks refs
		   JOIN import_history_payloads payload ON payload.chunk_id = refs.chunk_id
		  WHERE refs.thread_id = ?
		    AND EXISTS (
		        SELECT 1
		          FROM import_history_items imported
		          LEFT JOIN thread_import_item_overrides overrides
		            ON overrides.thread_id = refs.thread_id
		           AND overrides.item_id = imported.id
		         WHERE imported.chunk_id = refs.chunk_id
		           AND overrides.item_id IS NULL
		           AND (imported.payload_id = payload.id OR imported.input_payload_id = payload.id)
		    )`,
		threadID,
	); err != nil {
		return fmt.Errorf("%s copy shared payloads for %s: %w", label, threadID, err)
	}
	if err := setHistoryBulkLoadTx(tx, threadID, true, label); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO items (
		    id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of,
		    tool_name, decision, meta, created_at, updated_at
		 )
		 SELECT imported.id, refs.thread_id, imported.turn_index, imported.item_index,
		        imported.kind, imported.role, imported.status, imported.summary,
		        imported.payload_id, imported.input_payload_id, imported.parent_id,
		        imported.is_background, imported.completion_of, imported.tool_name,
		        imported.decision, imported.meta, imported.created_at, imported.updated_at
		   FROM thread_import_chunks refs
		   JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
		   LEFT JOIN thread_import_item_overrides overrides
		     ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
		   LEFT JOIN items local
		     ON local.thread_id = refs.thread_id AND local.id = imported.id
		  WHERE refs.thread_id = ?
		    AND overrides.item_id IS NULL
		    AND local.id IS NULL`,
		threadID,
	); err != nil {
		return fmt.Errorf("%s copy shared items for %s: %w", label, threadID, err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_chunks WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("%s detach shared history for %s: %w", label, threadID, err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_item_overrides WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("%s clear shared overrides for %s: %w", label, threadID, err)
	}
	if err := setHistoryBulkLoadTx(tx, threadID, false, label); err != nil {
		return err
	}
	return nil
}
