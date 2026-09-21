package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const historyPreparationRows = 64
const historyPreparationBytes = 4 << 20

// Correlation-bearing messages, plans, and background execution records stay
// in the private overlay. Their dependent records and lifecycle queries are
// thread-owned. Completed content can move to the existing immutable arm.
const historyPreparationPredicate = `items.status NOT IN ('running','streaming')
 AND items.kind NOT IN ('user_text','tool_call','tool_completion','workflow_proposal')
 AND items.is_background = 0 AND items.completion_of = ''`

// HistoryPreparationThreads pages the private histories eligible for background
// preparation. The cursor is only scheduling state; committed chunks are the
// durable progress, so restart and cancellation need no separate job records.
func (s *Store) HistoryPreparationThreads(ctx context.Context, after string) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT DISTINCT items.thread_id
 FROM items WHERE `+historyPreparationPredicate+` AND items.thread_id > ?
 ORDER BY items.thread_id LIMIT 32`, after)
	if err != nil {
		return nil, fmt.Errorf("store: list history preparation threads: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read history preparation thread: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PrepareThreadHistory moves a bounded batch into immutable import chunks.
// It preserves logical identities, search rowids, payload bytes and thread
// stamps. A concurrent fork or source mutation sees either representation in
// its entirety. Oversized payloads and edit snapshots keep using the existing
// payload snapshot path; preparation never reads them into memory.
func (s *Store) PrepareThreadHistory(ctx context.Context, threadID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin history preparation: %w", err)
	}
	defer tx.Rollback()
	// The size window bounds the selected bytes as well as the row count.
	// Payloads must be privately owned by this row: deleting it can then
	// release the old graph without affecting another local item.
	query := `WITH candidates AS (
 SELECT items.id, items.turn_index, items.item_index,
 length(CAST(items.meta AS BLOB))+length(CAST(items.summary AS BLOB))+
 COALESCE((SELECT length(data)+length(CAST(meta AS BLOB))+length(CAST(preview_spans AS BLOB))+length(CAST(spans AS BLOB)) FROM payloads WHERE thread_id=items.thread_id AND id=items.payload_id),0)+
 COALESCE((SELECT sum(length(data)) FROM payload_chunks WHERE thread_id=items.thread_id AND payload_id=items.payload_id),0)+
 COALESCE((SELECT length(data)+length(CAST(meta AS BLOB))+length(CAST(preview_spans AS BLOB))+length(CAST(spans AS BLOB)) FROM payloads WHERE thread_id=items.thread_id AND id=items.input_payload_id),0)+
 COALESCE((SELECT sum(length(data)) FROM payload_chunks WHERE thread_id=items.thread_id AND payload_id=items.input_payload_id),0) AS bytes
 FROM items WHERE items.thread_id=? AND ` + historyPreparationPredicate + `
 AND NOT EXISTS (SELECT 1 FROM message_anchors WHERE thread_id=items.thread_id AND user_item_id=items.id)
 AND NOT EXISTS (SELECT 1 FROM payloads WHERE thread_id=items.thread_id AND id IN (items.payload_id,items.input_payload_id) AND kind='proposed_plan')
 AND NOT EXISTS (SELECT 1 FROM proposed_plans WHERE thread_id=items.thread_id AND item_id=items.id)
 AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides WHERE thread_id=items.thread_id AND item_id=items.id)
 AND (items.payload_id IS NULL OR items.input_payload_id IS NULL OR items.payload_id<>items.input_payload_id)
 AND NOT EXISTS (SELECT 1 FROM payload_snapshot_refs WHERE thread_id=items.thread_id AND payload_id IN (items.payload_id,items.input_payload_id))
 AND NOT EXISTS (SELECT 1 FROM edit_file_snapshots WHERE thread_id=items.thread_id AND payload_id IN (items.payload_id,items.input_payload_id))
 AND NOT EXISTS (SELECT 1 FROM items other WHERE other.thread_id=items.thread_id AND other.id<>items.id AND other.payload_id IS NOT NULL AND other.payload_id IN (items.payload_id,items.input_payload_id))
 AND NOT EXISTS (SELECT 1 FROM items other WHERE other.thread_id=items.thread_id AND other.id<>items.id AND other.input_payload_id IS NOT NULL AND other.input_payload_id IN (items.payload_id,items.input_payload_id))
 AND NOT EXISTS (SELECT 1 FROM items child WHERE child.thread_id=items.thread_id AND child.parent_id=items.id AND child.parent_id<>'')
 AND NOT EXISTS (SELECT 1 FROM thread_import_chunks cr JOIN import_history_items child ON child.chunk_id=cr.chunk_id WHERE cr.thread_id=items.thread_id AND child.parent_id=items.id AND child.parent_id<>'')
 AND NOT EXISTS (SELECT 1 FROM thread_import_chunks refs JOIN import_history_payloads p ON p.chunk_id=refs.chunk_id
                  WHERE refs.thread_id=items.thread_id AND p.id IN (items.payload_id,items.input_payload_id))
 ), bounded AS (
 SELECT id,turn_index,item_index,bytes FROM candidates WHERE bytes<=?
 ORDER BY turn_index,item_index LIMIT ?
 ) SELECT id FROM (
 SELECT id,turn_index,item_index, sum(bytes) OVER (ORDER BY turn_index,item_index) AS total FROM bounded
 ) WHERE total<=? ORDER BY turn_index,item_index`
	rows, err := tx.Query(query, threadID, historyPreparationBytes, historyPreparationRows, historyPreparationBytes)
	if err != nil {
		return 0, fmt.Errorf("store: select history preparation batch: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("store: read history preparation batch: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	chunk := importHistoryChunk{id: "sealed:" + uuid.NewString()}
	for _, id := range ids {
		item, err := scanItemRow(tx.QueryRow(`SELECT `+itemHydrationColumns("items.thread_id", "''", "''", "''", "items.rev")+` FROM items WHERE thread_id=? AND id=?`, threadID, id))
		if err != nil {
			return 0, fmt.Errorf("store: read history preparation item: %w", err)
		}
		row := ImportRow{Item: item}
		for _, p := range []struct {
			id  string
			dst **Payload
		}{{item.PayloadID, &row.Payload}, {item.InputPayloadID, &row.InputPayload}} {
			if p.id == "" {
				continue
			}
			payload, err := preparationPayloadTx(tx, threadID, p.id)
			if err != nil {
				return 0, err
			}
			*p.dst = &payload
		}
		chunk.rows = append(chunk.rows, row)
	}
	chunk.minTurn = chunk.rows[0].Item.TurnIndex
	chunk.maxTurn = chunk.rows[len(chunk.rows)-1].Item.TurnIndex
	if _, err := tx.Exec(`INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES(?,?,?,?)`, chunk.id, len(chunk.rows), chunk.minTurn, chunk.maxTurn); err != nil {
		return 0, fmt.Errorf("store: create prepared history chunk: %w", err)
	}
	if err := insertImportHistoryChunkRowsTx(tx, chunk); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE import_history_payloads SET
 preview_spans=(SELECT preview_spans FROM payloads WHERE thread_id=? AND id=import_history_payloads.id),
 spans=(SELECT spans FROM payloads WHERE thread_id=? AND id=import_history_payloads.id)
 WHERE chunk_id=?`, threadID, threadID, chunk.id); err != nil {
		return 0, fmt.Errorf("store: preserve prepared payload highlights: %w", err)
	}
	if err := setHistoryBulkLoadTx(tx, threadID, true, "store: prepare history"); err != nil {
		return 0, err
	}
	clause, args := inClause("id", ids)
	if _, err := tx.Exec(`DELETE FROM items WHERE thread_id=? AND `+clause, append([]any{threadID}, args...)...); err != nil {
		return 0, fmt.Errorf("store: detach prepared private items: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id)
 SELECT ?,COALESCE(MAX(chunk_order)+1,0),? FROM thread_import_chunks WHERE thread_id=?`, threadID, chunk.id, threadID); err != nil {
		return 0, fmt.Errorf("store: attach prepared history: %w", err)
	}
	clause, args = inClause("item_id", ids)
	if _, err := tx.Exec(`UPDATE thread_search_rows SET source='import' WHERE thread_id=? AND source='item' AND `+clause, append([]any{threadID}, args...)...); err != nil {
		return 0, fmt.Errorf("store: preserve prepared search mappings: %w", err)
	}
	if err := setHistoryBulkLoadTx(tx, threadID, false, "store: prepare history"); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit history preparation: %w", err)
	}
	return len(ids), nil
}

func preparationPayloadTx(tx *sql.Tx, threadID, id string) (Payload, error) {
	var p Payload
	if err := tx.QueryRow(`SELECT id,kind,meta,data,created_at FROM payloads WHERE thread_id=? AND id=?`, threadID, id).Scan(&p.ID, &p.Kind, &p.Meta, &p.Data, &p.CreatedAt); err != nil {
		return p, fmt.Errorf("store: read preparation payload: %w", err)
	}
	rows, err := tx.Query(`SELECT data FROM payload_chunks WHERE thread_id=? AND payload_id=? ORDER BY chunk_index`, threadID, id)
	if err != nil {
		return p, fmt.Errorf("store: read preparation payload chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return p, fmt.Errorf("store: read preparation payload chunk: %w", err)
		}
		p.Data = append(p.Data, data...)
	}
	return p, rows.Err()
}
