package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
)

// importHistoryTargetRows is a target, not a hard split: chunks never divide
// one turn. That makes a shared prefix end only at a complete-turn boundary,
// which is the point the importer can safely resume row construction without
// carrying mutable tool/stream state from the prefix.
const importHistoryTargetRows = 256

type importHistoryChunk struct {
	id      string
	rows    []ImportRow
	minTurn int
	maxTurn int
}

type importHistoryPayloadRow struct {
	itemID  string
	payload Payload
}

func importRowsSharedTx(tx *sql.Tx, threadID string, rows []ImportRow) error {
	if len(rows) == 0 {
		return nil
	}
	chunks, err := buildImportHistoryChunks(rows)
	if err != nil {
		return err
	}

	var nextOrder int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(chunk_order) + 1, 0)
		   FROM thread_import_chunks
		  WHERE thread_id = ?`, threadID,
	).Scan(&nextOrder); err != nil {
		return fmt.Errorf("store: read next import history chunk order for %s: %w", threadID, err)
	}

	for i := range chunks {
		chunk := &chunks[i]
		result, err := tx.Exec(
			`INSERT OR IGNORE INTO import_history_chunks (
			    id, item_count, min_turn_index, max_turn_index
			 ) VALUES (?, ?, ?, ?)`,
			chunk.id, len(chunk.rows), chunk.minTurn, chunk.maxTurn,
		)
		if err != nil {
			return fmt.Errorf("store: insert import history chunk %s: %w", chunk.id, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: count inserted import history chunk %s: %w", chunk.id, err)
		}
		if inserted == 1 {
			if err := insertImportHistoryChunkRowsTx(tx, *chunk); err != nil {
				return err
			}
		} else if err := verifyImportHistoryChunkTx(tx, *chunk); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id)
			 VALUES (?, ?, ?)`,
			threadID, nextOrder+i, chunk.id,
		); err != nil {
			return fmt.Errorf(
				"store: attach import history chunk %s to thread %s at %d: %w",
				chunk.id, threadID, nextOrder+i, err,
			)
		}
	}
	return nil
}

func buildImportHistoryChunks(rows []ImportRow) ([]importHistoryChunk, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	chunks := make([]importHistoryChunk, 0, (len(rows)+importHistoryTargetRows-1)/importHistoryTargetRows)
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) {
			turn := rows[end].Item.TurnIndex
			turnEnd := end + 1
			for turnEnd < len(rows) && rows[turnEnd].Item.TurnIndex == turn {
				turnEnd++
			}
			if end > start && turnEnd-start > importHistoryTargetRows {
				break
			}
			end = turnEnd
			if end-start >= importHistoryTargetRows {
				break
			}
		}
		if end == start {
			return nil, fmt.Errorf("store: partition import history made no progress at row %d", start)
		}
		chunkRows := rows[start:end]
		id, err := importHistoryChunkID(chunkRows)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, importHistoryChunk{
			id:      id,
			rows:    chunkRows,
			minTurn: chunkRows[0].Item.TurnIndex,
			maxTurn: chunkRows[len(chunkRows)-1].Item.TurnIndex,
		})
		start = end
	}
	return chunks, nil
}

func importHistoryChunkID(rows []ImportRow) (string, error) {
	digest := sha256.New()
	hashString(digest, "agent-overflow/import-history-chunk/v1")
	hashInt64(digest, int64(len(rows)))
	for i := range rows {
		row := rows[i]
		item := row.Item
		hashString(digest, item.ID)
		hashInt64(digest, int64(item.TurnIndex))
		hashInt64(digest, int64(item.ItemIndex))
		hashString(digest, item.Kind)
		hashString(digest, item.Role)
		hashString(digest, item.Status)
		hashString(digest, item.Summary)
		hashString(digest, item.PayloadID)
		hashString(digest, item.InputPayloadID)
		hashString(digest, item.ParentID)
		hashBool(digest, item.IsBackground)
		hashString(digest, item.CompletionOf)
		hashString(digest, item.ToolName)
		hashString(digest, item.Decision)
		hashString(digest, item.Meta)
		hashInt64(digest, item.CreatedAt)
		hashInt64(digest, item.UpdatedAt)
		for _, payload := range []*Payload{row.InputPayload, row.Payload} {
			if payload == nil {
				hashBool(digest, false)
				continue
			}
			hashBool(digest, true)
			hashString(digest, payload.ID)
			hashString(digest, payload.Kind)
			hashString(digest, payload.Meta)
			hashBytes(digest, payload.Data)
			hashInt64(digest, payload.CreatedAt)
			// Imported payloads are born without derived highlight caches. Those
			// caches remain thread-local and are populated only on demand.
			hashString(digest, "")
			hashString(digest, "")
		}
	}
	return "import-chunk:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func hashString(dst hash.Hash, value string) {
	hashBytes(dst, []byte(value))
}

func hashBytes(dst hash.Hash, value []byte) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = dst.Write(size[:])
	_, _ = dst.Write(value)
}

func hashInt64(dst hash.Hash, value int64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], uint64(value))
	_, _ = dst.Write(encoded[:])
}

func hashBool(dst hash.Hash, value bool) {
	if value {
		_, _ = dst.Write([]byte{1})
		return
	}
	_, _ = dst.Write([]byte{0})
}

func insertImportHistoryChunkRowsTx(tx *sql.Tx, chunk importHistoryChunk) error {
	payloads := make([]importHistoryPayloadRow, 0, len(chunk.rows))
	for i := range chunk.rows {
		row := chunk.rows[i]
		for _, payload := range []*Payload{row.InputPayload, row.Payload} {
			if payload != nil {
				payloads = append(payloads, importHistoryPayloadRow{itemID: row.Item.ID, payload: *payload})
			}
		}
	}
	if err := execImportInsertChunks(
		tx,
		`INSERT INTO import_history_payloads (chunk_id, id, kind, meta, data, created_at, preview_spans, spans)`,
		`(?, ?, ?, ?, ?, ?, ?, ?)`,
		payloads,
		func(row importHistoryPayloadRow) []any {
			return []any{
				chunk.id, row.payload.ID, row.payload.Kind, row.payload.Meta,
				row.payload.Data, row.payload.CreatedAt, "", "",
			}
		},
		func(row importHistoryPayloadRow) string { return row.itemID },
		"shared import payload",
	); err != nil {
		return err
	}
	return execImportInsertChunks(
		tx,
		`INSERT INTO import_history_items (
		    chunk_id, id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of,
		    tool_name, decision, meta, created_at, updated_at
		)`,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chunk.rows,
		func(row ImportRow) []any {
			item := row.Item
			return []any{
				chunk.id, item.ID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role,
				item.Status, item.Summary, nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID),
				item.ParentID, boolToInt(item.IsBackground), item.CompletionOf, item.ToolName,
				item.Decision, item.Meta, item.CreatedAt, item.UpdatedAt,
			}
		},
		func(row ImportRow) string { return row.Item.ID },
		"shared import item",
	)
}

func verifyImportHistoryChunkTx(tx *sql.Tx, chunk importHistoryChunk) error {
	var itemCount, minTurn, maxTurn int
	if err := tx.QueryRow(
		`SELECT item_count, min_turn_index, max_turn_index
		   FROM import_history_chunks
		  WHERE id = ?`, chunk.id,
	).Scan(&itemCount, &minTurn, &maxTurn); err != nil {
		return fmt.Errorf("store: verify reused import history chunk %s: %w", chunk.id, err)
	}
	if itemCount != len(chunk.rows) || minTurn != chunk.minTurn || maxTurn != chunk.maxTurn {
		return fmt.Errorf(
			"store: import history chunk %s digest collision: stored=(%d,%d,%d) incoming=(%d,%d,%d)",
			chunk.id, itemCount, minTurn, maxTurn, len(chunk.rows), chunk.minTurn, chunk.maxTurn,
		)
	}
	return nil
}
