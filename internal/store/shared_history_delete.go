package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// deleteSharedHistoryItemTx cuts one imported row, found through its id
// index rather than by probing every chunk the thread references.
func deleteSharedHistoryItemTx(tx *sql.Tx, threadID, itemID string) (int64, error) {
	return deleteSharedHistoryTx(tx, threadID,
		"import_history_items items\n CROSS JOIN thread_import_chunks refs ON refs.chunk_id=items.chunk_id",
		"items.id = ?", []any{itemID})
}

// deleteSharedHistoryFromTurnTx applies a cut whose predicate only removes
// rows at or after fromTurn. Chunk references whose turn range ends before
// it are not read.
func deleteSharedHistoryFromTurnTx(tx *sql.Tx, threadID string, fromTurn int, predicate string, args []any) (int64, error) {
	return deleteSharedHistoryTx(tx, threadID,
		"thread_import_chunks refs\n CROSS JOIN import_history_items items ON items.chunk_id=refs.chunk_id",
		"refs.max_turn_index >= ? AND ("+predicate+")", append([]any{fromTurn}, args...))
}

// deleteSharedHistoryTx applies the same cut as the private item DELETE.
// Kept history remains shared. Partial chunks get deletion overrides; empty
// chunks detach and follow the existing last-reference garbage collection.
// Only the chunks the cut touched can become empty, and only their rows'
// overrides can be left without an attached row.
func deleteSharedHistoryTx(tx *sql.Tx, threadID, source, predicate string, args []any) (int64, error) {
	rows, err := tx.Query(`SELECT items.id,items.chunk_id,items.parent_id,COALESCE(items.payload_id,''),COALESCE(items.input_payload_id,'') FROM `+source+`
 WHERE refs.thread_id=? AND `+importedNotOverridden+` AND (`+predicate+`)`, append([]any{threadID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("store: select shared history cut: %w", err)
	}
	var ids []string
	chunks := make(map[string]bool)
	parents := make(map[string]bool)
	payloads := make(map[string]bool)
	for rows.Next() {
		var id, chunk, parent, output, input string
		if err := rows.Scan(&id, &chunk, &parent, &output, &input); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
		chunks[chunk] = true
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
	if err := detachOverriddenChunksTx(tx, threadID, slices.Collect(maps.Keys(chunks))); err != nil {
		return 0, err
	}
	// A payload can have a private override without localizing its item.
	// Those rows have no item DELETE to run the ordinary payload collector.
	payloadIDs := slices.Collect(maps.Keys(payloads))
	for start := 0; start < len(payloadIDs); start += 256 {
		clause, values := inClause("id", payloadIDs[start:min(start+256, len(payloadIDs))])
		if _, err := tx.Exec(`DELETE FROM payloads WHERE thread_id=? AND `+clause+`
   AND NOT `+logicalPayloadReferenceSQL("payloads.thread_id", "payloads.id"), append([]any{threadID}, values...)...); err != nil {
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

// detachOverriddenChunksTx detaches the named chunk references whose every
// row this thread overrides, then releases the overrides of those rows:
// with the chunk gone they hide nothing. Rows are read before the detach,
// because the last reference's detach garbage-collects the chunk.
func detachOverriddenChunksTx(tx *sql.Tx, threadID string, chunkIDs []string) error {
	for start := 0; start < len(chunkIDs); start += 256 {
		clause, values := inClause("chunk_id", chunkIDs[start:min(start+256, len(chunkIDs))])
		rows, err := tx.Query(`SELECT chunk_id FROM thread_import_chunks WHERE thread_id=? AND `+clause+` AND NOT EXISTS (
 SELECT 1 FROM import_history_items i WHERE i.chunk_id=thread_import_chunks.chunk_id
 AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id=thread_import_chunks.thread_id AND o.item_id=i.id))`,
			append([]any{threadID}, values...)...)
		if err != nil {
			return fmt.Errorf("store: find overridden history chunks: %w", err)
		}
		var empty []string
		for rows.Next() {
			var chunk string
			if err := rows.Scan(&chunk); err != nil {
				return errors.Join(fmt.Errorf("store: scan overridden history chunk: %w", err), rows.Close())
			}
			empty = append(empty, chunk)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("store: read overridden history chunks: %w", err)
		}
		for _, chunk := range empty {
			if err := detachOverriddenChunkTx(tx, threadID, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

func detachOverriddenChunkTx(tx *sql.Tx, threadID, chunkID string) error {
	rows, err := tx.Query(`SELECT id FROM import_history_items WHERE chunk_id=?`, chunkID)
	if err != nil {
		return fmt.Errorf("store: read detached history chunk %s: %w", chunkID, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return errors.Join(fmt.Errorf("store: scan detached history row: %w", err), rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: read detached history chunk %s: %w", chunkID, err)
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_chunks WHERE thread_id=? AND chunk_id=?`, threadID, chunkID); err != nil {
		return fmt.Errorf("store: detach deleted history chunk %s: %w", chunkID, err)
	}
	for start := 0; start < len(ids); start += 256 {
		clause, values := inClause("item_id", ids[start:min(start+256, len(ids))])
		if _, err := tx.Exec(`DELETE FROM thread_import_item_overrides WHERE thread_id=? AND `+clause+` AND NOT EXISTS (
 SELECT 1 FROM import_history_items i CROSS JOIN thread_import_chunks refs ON refs.chunk_id=i.chunk_id
 WHERE i.id=thread_import_item_overrides.item_id AND refs.thread_id=thread_import_item_overrides.thread_id)`,
			append([]any{threadID}, values...)...); err != nil {
			return fmt.Errorf("store: release detached history overrides: %w", err)
		}
	}
	return nil
}
