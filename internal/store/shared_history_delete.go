package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// deleteSharedHistoryWhereTx applies the same cut as the private item DELETE.
// Kept history remains shared. Partial chunks get deletion overrides; empty
// chunks detach and follow the existing last-reference garbage collection.
func deleteSharedHistoryWhereTx(tx *sql.Tx, threadID, predicate string, args []any) (int64, error) {
	rows, err := tx.Query(`SELECT items.id,items.parent_id,COALESCE(items.payload_id,''),COALESCE(items.input_payload_id,'') FROM thread_import_chunks refs
 JOIN import_history_items items ON items.chunk_id=refs.chunk_id
 WHERE refs.thread_id=? AND `+importedNotOverridden+` AND (`+predicate+`)`, append([]any{threadID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("store: select shared history cut: %w", err)
	}
	var ids []string
	parents := make(map[string]bool)
	payloads := make(map[string]bool)
	for rows.Next() {
		var id, parent, output, input string
		if err := rows.Scan(&id, &parent, &output, &input); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
		if output != "" {
			payloads[output] = true
		}
		if input != "" {
			payloads[input] = true
		}
		if parent != "" {
			parents[parent] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("store: read shared history cut: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := execImportInsertChunks(tx, `INSERT INTO thread_import_item_overrides(thread_id,item_id)`, `(?,?)`, ids,
		func(id string) []any { return []any{threadID, id} }, func(id string) string { return id }, "history deletion override"); err != nil {
		return 0, err
	}
	for start := 0; start < len(ids); start += 256 {
		if err := deleteThreadSearchItemsTx(tx, threadID, ids[start:min(start+256, len(ids))]); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_chunks WHERE thread_id=? AND NOT EXISTS (
 SELECT 1 FROM import_history_items i WHERE i.chunk_id=thread_import_chunks.chunk_id
 AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id=thread_import_chunks.thread_id AND o.item_id=i.id))`, threadID); err != nil {
		return 0, fmt.Errorf("store: detach deleted history chunks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_item_overrides WHERE thread_id=? AND NOT EXISTS (
 SELECT 1 FROM thread_import_chunks refs JOIN import_history_items i ON i.chunk_id=refs.chunk_id
 WHERE refs.thread_id=thread_import_item_overrides.thread_id AND i.id=thread_import_item_overrides.item_id)`, threadID); err != nil {
		return 0, fmt.Errorf("store: release detached history overrides: %w", err)
	}
	// A payload can have a private override without localizing its item.
	// Those rows have no item DELETE to run the ordinary payload collector.
	var payloadIDs []string
	for id := range payloads {
		payloadIDs = append(payloadIDs, id)
	}
	for start := 0; start < len(payloadIDs); start += 256 {
		clause, values := inClause("id", payloadIDs[start:min(start+256, len(payloadIDs))])
		if _, err := tx.Exec(`DELETE FROM payloads WHERE thread_id=? AND `+clause+`
   AND NOT EXISTS(SELECT 1 FROM timeline_items i WHERE i.thread_id=payloads.thread_id AND (i.payload_id=payloads.id OR i.input_payload_id=payloads.id))`, append([]any{threadID}, values...)...); err != nil {
			return 0, fmt.Errorf("store: collect deleted shared payload overrides: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE threads SET history_rev=history_rev+?,history_epoch=history_epoch+? WHERE id=?`, len(ids), len(ids), threadID); err != nil {
		return 0, fmt.Errorf("store: stamp shared history cut: %w", err)
	}
	for parent := range parents {
		if _, err := tx.Exec(stampRowsSQL+` WHERE thread_id=?1 AND rev<>(SELECT history_rev FROM threads WHERE id=?1)
 AND id IN (`+stampedRowIDsFor("?1", "?2", "?2")+`)`, threadID, parent); err != nil {
			return 0, fmt.Errorf("store: stamp shared history cut parent: %w", err)
		}
	}
	return int64(len(ids)), nil
}
