package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// History repair undoes two kinds of stored-history debris. Removed
// background sealing moved a thread's settled local rows into import chunks
// whose ids start with "sealed:", which later forks of the thread share. The
// repair folds those rows back into each referencing thread's items and
// payloads. Payload rows that nothing references are pruned.
//
// Both repairs are resumable without a job table: every batch is one writer
// transaction that leaves the logical timeline (timeline_items,
// timeline_payloads, search hits) unchanged, and the durable progress is the
// data itself. A concurrent reader or writer sees one complete
// representation, and mutation paths choose between the imported and local
// representation inside their own transaction, so no thread lock is needed.
// This is the same contract the removed sealing loop ran under.
//
// Every repair transaction is followed by a passive checkpoint
// (checkpointHistoryRepair).

// sealedChunkLow and sealedChunkHigh bound the sealed chunk ids as a range,
// so the (thread_id, chunk_id) and chunk_id indexes answer the prefix.
const (
	sealedChunkLow  = "sealed:"
	sealedChunkHigh = "sealed;"
)

// A repair transaction handles at most historyRepairRows rows and
// historyRepairBytes payload bytes. An unseal transaction moves a chunk's
// rows in pieces of at most historyRepairPieceRows rows, stops taking pieces
// once it has run for historyRepairTime, and always moves at least one
// piece. Each moved row's insert probes every sealed reference covering its
// turn, so on the measured database one 64-row chunk's inserts took about
// 18 ms idle and up to 206 ms with the processors oversubscribed; a piece
// bounds that work. Sealing wrote at most 64 rows and 4 MiB per chunk.
const (
	historyRepairRows      = 256
	historyRepairPieceRows = 16
	historyRepairBytes     = 4 << 20
	historyRepairTime      = 10 * time.Millisecond
)

// historyRepairBudget bounds one unseal transaction.
type historyRepairBudget struct {
	rows  int
	piece int
	bytes int64
	time  time.Duration
}

var defaultHistoryRepairBudget = historyRepairBudget{
	rows:  historyRepairRows,
	piece: historyRepairPieceRows,
	bytes: historyRepairBytes,
	time:  historyRepairTime,
}

// orphanPayloadPage is how many payload rows one read of the orphan scan
// examines.
const orphanPayloadPage = 1000

// checkpointHistoryRepair copies the frames a repair transaction appended to
// the WAL into the database file, outside any transaction. Left in the WAL,
// several transactions' frames are copied by SQLite's automatic checkpoint
// inside whichever commit next passes 1,000 frames, possibly a live write's;
// on the measured database such commits took up to 99 ms. Run after every
// transaction, one checkpoint copies one transaction's frames.
func (s *Store) checkpointHistoryRepair() error {
	if err := s.PassiveCheckpoint(); err != nil {
		return fmt.Errorf("store: checkpoint history repair: %w", err)
	}
	return nil
}

// UnsealStats counts what a repair folded back into a thread's private rows.
type UnsealStats struct {
	// Chunks is the number of sealed chunk references released.
	Chunks int
	// Rows is the number of imported rows moved into items. Rows the thread
	// had overridden are released, not moved.
	Rows int
	// Payloads is the number of payload rows copied into payloads.
	Payloads int
}

func (s *UnsealStats) add(other UnsealStats) {
	s.Chunks += other.Chunks
	s.Rows += other.Rows
	s.Payloads += other.Payloads
}

// OrphanPayloadStats describes unreferenced payload rows. Bytes counts the
// payload data with its append chunks and edit snapshots.
type OrphanPayloadStats struct {
	Payloads int
	Bytes    int64
}

// SealedHistoryThreads lists the threads that still reference a sealed chunk.
func (s *Store) SealedHistoryThreads(ctx context.Context) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT DISTINCT thread_id FROM thread_import_chunks
 WHERE chunk_id >= ? AND chunk_id < ? ORDER BY thread_id`, sealedChunkLow, sealedChunkHigh)
	if err != nil {
		return nil, fmt.Errorf("store: list sealed history threads: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read sealed history thread: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sealed history threads: %w", err)
	}
	return ids, nil
}

// UnsealThreadHistory folds every sealed chunk the thread references back
// into its private rows, one bounded transaction and checkpoint at a time,
// calling pause between transactions. Rows keep their ids, positions, meta
// and timestamps; payloads keep their ids, bytes and highlight spans; search
// rows keep their rowids. A chunk moved over several transactions is between
// them in the state localizeImportedItemTx leaves: each moved row has an
// override hiding its imported copy. The last piece removes the chunk's
// overrides with the reference, and the chunk is deleted once no thread or
// payload snapshot uses it.
//
// Moved rows are written under history_bulk_load and history_rev advances by
// the number of moved rows, so the stamp ends above every revision the batch
// wrote and the epoch stays put. Under the flag the insert trigger stamps
// only the moved row; its parents, carriers and completion siblings read the
// same before and after and keep their stamps. Cancelling ctx stops at the
// next batch boundary and is not an error; a later call resumes from what is
// left.
func (s *Store) UnsealThreadHistory(ctx context.Context, threadID string, pause ChunkPause) (UnsealStats, error) {
	var total UnsealStats
	for ctx.Err() == nil {
		batch, more, err := s.unsealThreadHistoryBatch(threadID, defaultHistoryRepairBudget)
		total.add(batch)
		if err != nil {
			return total, err
		}
		if batch != (UnsealStats{}) {
			if err := s.checkpointHistoryRepair(); err != nil {
				return total, err
			}
		}
		if !more {
			break
		}
		if pause != nil {
			pause()
		}
	}
	return total, nil
}

// sealedHistoryBatchSQL lists a thread's sealed chunk references with their
// row counts. Each moved row's insert probes every remaining reference that
// covers its turn (trg_items_reject_import_position_collision); sealing left
// up to 2,488 references covering one turn of the measured database. Latest
// turns go first, so a probe never visits the references of later turns, and
// within a turn smaller chunks go first, so the largest chunk's rows probe
// the fewest references. The query walks idx_thread_import_chunks_turns,
// named because the planner otherwise takes the (thread_id, chunk_id) key
// for the sealed range and sorts every reference of the thread. It
// sorts one turn's references at a time and stops at the row budget.
const sealedHistoryBatchSQL = `SELECT refs.chunk_id, chunks.item_count
 FROM thread_import_chunks refs INDEXED BY idx_thread_import_chunks_turns
 JOIN import_history_chunks chunks ON chunks.id = refs.chunk_id
 WHERE refs.thread_id = ? AND refs.chunk_id >= ? AND refs.chunk_id < ?
 ORDER BY refs.max_turn_index DESC, chunks.item_count
 LIMIT ?`

type sealedChunkRef struct {
	id   string
	rows int
}

// sealedRow is a chunk row the thread still shows from imported history,
// with the payload bytes moving it copies.
type sealedRow struct {
	id    string
	bytes int64
}

// unsealThreadHistoryBatch moves sealed rows in one transaction, piece by
// piece, until the next piece would pass the row or byte budget or the
// transaction has run for the time budget, always at least one piece. It
// reports whether the thread may still reference a sealed chunk.
func (s *Store) unsealThreadHistoryBatch(threadID string, budget historyRepairBudget) (UnsealStats, bool, error) {
	start := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return UnsealStats{}, false, fmt.Errorf("store: begin history repair: %w", err)
	}
	defer tx.Rollback()

	limit := budget.rows + 1
	rows, err := tx.Query(sealedHistoryBatchSQL, threadID, sealedChunkLow, sealedChunkHigh, limit)
	if err != nil {
		return UnsealStats{}, false, fmt.Errorf("store: select sealed history batch: %w", err)
	}
	var chunks []sealedChunkRef
	for rows.Next() {
		var ref sealedChunkRef
		if err := rows.Scan(&ref.id, &ref.rows); err != nil {
			return UnsealStats{}, false, errors.Join(fmt.Errorf("store: read sealed history batch: %w", err), rows.Close())
		}
		chunks = append(chunks, ref)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return UnsealStats{}, false, fmt.Errorf("store: read sealed history batch: %w", err)
	}

	var stats UnsealStats
	pieces, takenRows, takenBytes := 0, 0, int64(0)
	for _, chunk := range chunks {
		remaining, err := sealedRowsTx(tx, threadID, chunk.id)
		if err != nil {
			return UnsealStats{}, false, err
		}
		for {
			piece := remaining[:min(max(budget.piece, 1), len(remaining))]
			var pieceBytes int64
			for _, row := range piece {
				pieceBytes += row.bytes
			}
			if pieces > 0 && (takenRows+len(piece) > budget.rows || takenBytes+pieceBytes > budget.bytes || time.Since(start) >= budget.time) {
				return commitUnsealBatch(tx, threadID, stats, true)
			}
			// Rows written under the flag are stamped with the current
			// history_rev; commitUnsealBatch adds the moved row count before
			// commit and clears it.
			if len(piece) > 0 && stats.Rows == 0 {
				if err := beginImportItemHistoryTx(tx, threadID, len(piece)); err != nil {
					return UnsealStats{}, false, err
				}
			}
			remaining = remaining[len(piece):]
			last := len(remaining) == 0
			payloads, err := moveSealedRowsTx(tx, threadID, chunk.id, piece, last)
			if err != nil {
				return UnsealStats{}, false, err
			}
			pieces++
			takenRows += len(piece)
			takenBytes += pieceBytes
			stats.Rows += len(piece)
			stats.Payloads += payloads
			if last {
				if err := releaseSealedChunkTx(tx, threadID, chunk.id); err != nil {
					return UnsealStats{}, false, err
				}
				stats.Chunks++
				break
			}
		}
	}
	return commitUnsealBatch(tx, threadID, stats, len(chunks) == limit)
}

func commitUnsealBatch(tx *sql.Tx, threadID string, stats UnsealStats, more bool) (UnsealStats, bool, error) {
	if err := finishImportItemHistoryTx(tx, threadID, stats.Rows); err != nil {
		return UnsealStats{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return UnsealStats{}, false, fmt.Errorf("store: commit history repair: %w", err)
	}
	return stats, more, nil
}

// sealedRowsTx lists the chunk's rows the thread has not overridden, in
// timeline order.
func sealedRowsTx(tx *sql.Tx, threadID, chunkID string) ([]sealedRow, error) {
	rows, err := tx.Query(`SELECT i.id,
   COALESCE((SELECT length(p.data) FROM import_history_payloads p WHERE p.chunk_id = i.chunk_id AND p.id = i.payload_id), 0)
 + COALESCE((SELECT length(p.data) FROM import_history_payloads p WHERE p.chunk_id = i.chunk_id AND p.id = i.input_payload_id), 0)
 FROM import_history_items i
 WHERE i.chunk_id = ? AND NOT EXISTS (
   SELECT 1 FROM thread_import_item_overrides o WHERE o.thread_id = ? AND o.item_id = i.id)
 ORDER BY i.turn_index, i.item_index`, chunkID, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: read sealed rows of %s: %w", chunkID, err)
	}
	var sealed []sealedRow
	for rows.Next() {
		var row sealedRow
		if err := rows.Scan(&row.id, &row.bytes); err != nil {
			return nil, errors.Join(fmt.Errorf("store: read sealed rows of %s: %w", chunkID, err), rows.Close())
		}
		sealed = append(sealed, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read sealed rows of %s: %w", chunkID, err)
	}
	return sealed, nil
}

// moveSealedRowsTx moves the given rows of one chunk into the thread's
// private rows with their payloads and search mappings. Unless the piece is
// the chunk's last, each moved row gets an override so the chunk, which the
// thread still references, no longer shows it. It returns the number of
// payload rows copied. The caller holds history_bulk_load.
func moveSealedRowsTx(tx *sql.Tx, threadID, chunkID string, piece []sealedRow, last bool) (int, error) {
	if len(piece) == 0 {
		return 0, nil
	}
	ids := make([]string, len(piece))
	for i, row := range piece {
		ids[i] = row.id
	}
	clause, idArgs := inClause("i.id", ids)
	if !last {
		if _, err := tx.Exec(`INSERT INTO thread_import_item_overrides (thread_id, item_id)
 SELECT ?, i.id FROM import_history_items i WHERE i.chunk_id = ? AND `+clause,
			append([]any{threadID, chunkID}, idArgs...)...); err != nil {
			return 0, fmt.Errorf("store: override moved sealed rows of %s: %w", chunkID, err)
		}
	}
	// Payloads first: items reference them by foreign key. A payload the
	// thread already overlays locally keeps its local bytes.
	result, err := tx.Exec(`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at, preview_spans, spans)
 SELECT ?, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
   FROM import_history_payloads p
  WHERE p.chunk_id = ?
    AND NOT EXISTS (SELECT 1 FROM payloads local WHERE local.thread_id = ? AND local.id = p.id)
    AND EXISTS (SELECT 1 FROM import_history_items i
                 WHERE i.chunk_id = p.chunk_id AND (i.payload_id = p.id OR i.input_payload_id = p.id)
                   AND `+clause+`)`, append([]any{threadID, chunkID, threadID}, idArgs...)...)
	if err != nil {
		return 0, fmt.Errorf("store: restore sealed payloads of %s: %w", chunkID, err)
	}
	payloads, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count restored sealed payloads of %s: %w", chunkID, err)
	}
	result, err = tx.Exec(`INSERT INTO items (
    id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, input_payload_id, parent_id, is_background, completion_of,
    tool_name, decision, meta, created_at, updated_at
 )
 SELECT i.id, ?, i.turn_index, i.item_index, i.kind, i.role, i.status, i.summary,
        i.payload_id, i.input_payload_id, i.parent_id, i.is_background, i.completion_of,
        i.tool_name, i.decision, i.meta, i.created_at, i.updated_at
   FROM import_history_items i
  WHERE i.chunk_id = ? AND `+clause+`
  ORDER BY i.turn_index, i.item_index`, append([]any{threadID, chunkID}, idArgs...)...)
	if err != nil {
		return 0, fmt.Errorf("store: restore sealed rows of %s: %w", chunkID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count restored sealed rows of %s: %w", chunkID, err)
	}
	if inserted != int64(len(ids)) {
		return 0, fmt.Errorf("store: restore sealed rows of %s: moved %d of %d rows", chunkID, inserted, len(ids))
	}

	// A moved row keeps its search rowid and indexed text. Rows that had no
	// mapping, such as rows a running index build had not reached, are
	// indexed now.
	searchClause, searchArgs := inClause("item_id", ids)
	if _, err := tx.Exec(`UPDATE OR IGNORE thread_search_rows SET source = ?
 WHERE thread_id = ? AND source = ? AND `+searchClause,
		append([]any{ThreadSearchSourceItem, threadID, ThreadSearchSourceImport}, searchArgs...)...); err != nil {
		return 0, fmt.Errorf("store: restore sealed search rows of %s: %w", chunkID, err)
	}
	if err := indexUnmappedItemsTx(tx, threadID, ids); err != nil {
		return 0, err
	}
	return int(payloads), nil
}

// releaseSealedChunkTx drops the thread's reference to a chunk whose rows it
// has all moved or overridden, with the chunk's overrides and its remaining
// import-side search rows, which belong to rows an override hides.
func releaseSealedChunkTx(tx *sql.Tx, threadID, chunkID string) error {
	if err := deleteThreadSearchWhereTx(tx,
		`thread_id = ? AND source = ? AND item_id IN (SELECT id FROM import_history_items WHERE chunk_id = ?)`,
		[]any{threadID, ThreadSearchSourceImport, chunkID}); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM thread_import_item_overrides
 WHERE thread_id = ? AND item_id IN (SELECT id FROM import_history_items WHERE chunk_id = ?)`, threadID, chunkID); err != nil {
		return fmt.Errorf("store: release sealed overrides of %s: %w", chunkID, err)
	}
	result, err := tx.Exec(`DELETE FROM thread_import_chunks WHERE thread_id = ? AND chunk_id = ?`, threadID, chunkID)
	if err != nil {
		return fmt.Errorf("store: detach sealed chunk %s: %w", chunkID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: detach sealed chunk %s", chunkID)); err != nil {
		return err
	}
	return releaseDetachedChunkTx(tx, chunkID)
}

// indexUnmappedItemsTx indexes the named local rows that have no item-side
// search mapping.
func indexUnmappedItemsTx(tx *sql.Tx, threadID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	clause, args := inClause("i.id", ids)
	rows, err := tx.Query(`SELECT i.id, i.kind, i.status, i.summary FROM items i
 WHERE i.thread_id = ? AND `+clause+`
   AND NOT EXISTS (SELECT 1 FROM thread_search_rows r WHERE r.thread_id = i.thread_id AND r.item_id = i.id AND r.source = ?)`,
		append(append([]any{threadID}, args...), ThreadSearchSourceItem)...)
	if err != nil {
		return fmt.Errorf("store: read unindexed restored rows: %w", err)
	}
	type unindexed struct{ id, kind, status, summary string }
	var pending []unindexed
	for rows.Next() {
		var row unindexed
		if err := rows.Scan(&row.id, &row.kind, &row.status, &row.summary); err != nil {
			return errors.Join(fmt.Errorf("store: scan unindexed restored row: %w", err), rows.Close())
		}
		pending = append(pending, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: read unindexed restored rows: %w", err)
	}
	for _, row := range pending {
		if err := indexSettledItemTx(tx, threadID, row.id, row.kind, row.status, row.summary); err != nil {
			return err
		}
	}
	return nil
}

// releaseDetachedChunkTx deletes a chunk no thread references. Payload
// snapshots that still borrow its bytes take a private copy first, because
// their foreign key keeps the chunk otherwise.
func releaseDetachedChunkTx(tx *sql.Tx, chunkID string) error {
	var referenced bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_import_chunks WHERE chunk_id = ?)`, chunkID).Scan(&referenced); err != nil {
		return fmt.Errorf("store: inspect sealed chunk %s: %w", chunkID, err)
	}
	if referenced {
		return nil
	}
	if _, err := tx.Exec(`UPDATE payload_snapshots
    SET data = (SELECT p.data FROM import_history_payloads p
                 WHERE p.chunk_id = payload_snapshots.chunk_id AND p.id = payload_snapshots.payload_id),
        chunk_id = NULL
  WHERE chunk_id = ?`, chunkID); err != nil {
		return fmt.Errorf("store: copy sealed chunk %s into payload snapshots: %w", chunkID, err)
	}
	if _, err := tx.Exec(`DELETE FROM import_history_chunks WHERE id = ?`, chunkID); err != nil {
		return fmt.Errorf("store: delete sealed chunk %s: %w", chunkID, err)
	}
	return nil
}

// ReleaseDetachedSealedChunks deletes sealed chunks that no thread references
// and only payload snapshots keep, one chunk per transaction and checkpoint
// with pause between them. It returns how many chunks it released. Cancelling ctx stops
// at the next chunk and is not an error.
func (s *Store) ReleaseDetachedSealedChunks(ctx context.Context, pause ChunkPause) (int, error) {
	released := 0
	after := ""
	for ctx.Err() == nil {
		var chunkID string
		err := s.db.QueryRow(`SELECT c.id FROM import_history_chunks c
 WHERE c.id >= ? AND c.id < ? AND c.id > ?
   AND NOT EXISTS (SELECT 1 FROM thread_import_chunks r WHERE r.chunk_id = c.id)
 ORDER BY c.id LIMIT 1`, sealedChunkLow, sealedChunkHigh, after).Scan(&chunkID)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return released, fmt.Errorf("store: find detached sealed chunk: %w", err)
		}
		if released > 0 && pause != nil {
			pause()
			if ctx.Err() != nil {
				break
			}
		}
		tx, err := s.db.Begin()
		if err != nil {
			return released, fmt.Errorf("store: begin sealed chunk release: %w", err)
		}
		if err := releaseDetachedChunkTx(tx, chunkID); err != nil {
			return released, errors.Join(err, tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return released, fmt.Errorf("store: commit sealed chunk release: %w", err)
		}
		released++
		after = chunkID
		if err := s.checkpointHistoryRepair(); err != nil {
			return released, err
		}
	}
	return released, nil
}

// payloadReferencedSQL is true while payload (thread, id) is in use: a
// logical timeline row of the thread names it, or a payload snapshot borrows
// its bytes. Append chunks, edit snapshots and snapshot references are the
// payload's own children and cascade with it. Every payload write is coupled
// to the item write that references it in one transaction, so a payload this
// predicate rejects inside a writer transaction has no pending referrer.
func payloadReferencedSQL(thread, payload string) string {
	return `(` + logicalPayloadReferenceSQL(thread, payload) + `
	      OR EXISTS (SELECT 1 FROM payload_snapshots borrowed
	                  WHERE borrowed.source_thread_id = ` + thread + ` AND borrowed.payload_id = ` + payload + `))`
}

// payloadStoredBytesSQL is what deleting one payload row frees.
const payloadStoredBytesSQL = `length(p.data)
 + COALESCE((SELECT sum(length(c.data)) FROM payload_chunks c WHERE c.thread_id = p.thread_id AND c.payload_id = p.id), 0)
 + COALESCE((SELECT sum(length(e.content)) FROM edit_file_snapshots e WHERE e.thread_id = p.thread_id AND e.payload_id = p.id), 0)`

type orphanPayload struct {
	threadID string
	id       string
	bytes    int64
}

// scanOrphanPayloads walks payloads in primary-key order one page per read
// and hands each page's unreferenced rows to visit. Cancelling ctx stops at
// the next page.
func (s *Store) scanOrphanPayloads(ctx context.Context, visit func([]orphanPayload) error) error {
	afterThread, afterID := "", ""
	for ctx.Err() == nil {
		rows, err := s.reader().Query(`SELECT p.thread_id, p.id,
 CASE WHEN `+payloadReferencedSQL("p.thread_id", "p.id")+` THEN -1 ELSE `+payloadStoredBytesSQL+` END
 FROM payloads p
 WHERE (p.thread_id, p.id) > (?, ?)
 ORDER BY p.thread_id, p.id
 LIMIT ?`, afterThread, afterID, orphanPayloadPage)
		if err != nil {
			return fmt.Errorf("store: scan orphan payloads: %w", err)
		}
		var page []orphanPayload
		scanned := 0
		for rows.Next() {
			var row orphanPayload
			if err := rows.Scan(&row.threadID, &row.id, &row.bytes); err != nil {
				return errors.Join(fmt.Errorf("store: read orphan payload scan: %w", err), rows.Close())
			}
			scanned++
			afterThread, afterID = row.threadID, row.id
			if row.bytes >= 0 {
				page = append(page, row)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("store: read orphan payload scan: %w", err)
		}
		if len(page) > 0 {
			if err := visit(page); err != nil {
				return err
			}
		}
		if scanned < orphanPayloadPage {
			return nil
		}
	}
	return nil
}

// CountOrphanPayloads reports the payload rows nothing references. It only
// reads.
func (s *Store) CountOrphanPayloads(ctx context.Context) (OrphanPayloadStats, error) {
	var stats OrphanPayloadStats
	err := s.scanOrphanPayloads(ctx, func(page []orphanPayload) error {
		for _, row := range page {
			stats.Payloads++
			stats.Bytes += row.bytes
		}
		return nil
	})
	return stats, err
}

// PruneOrphanPayloads deletes the payload rows nothing references, in
// transactions of at most historyRepairRows rows and historyRepairBytes
// bytes, each followed by a checkpoint, with pause between them. Each delete re-checks the reference
// predicate inside its transaction, so a row referenced since the scan is
// kept. Deleting a fork's payload can release the snapshot that was the last
// borrower of its source payload; such sources are checked again in the same
// call. Cancelling ctx stops at the next transaction and is not an error.
func (s *Store) PruneOrphanPayloads(ctx context.Context, pause ChunkPause) (OrphanPayloadStats, error) {
	var stats OrphanPayloadStats
	var released []orphanPayload
	wrote := false
	prune := func(page []orphanPayload) error {
		for start := 0; start < len(page) && ctx.Err() == nil; {
			end, bytes := start, int64(0)
			for end < len(page) && end-start < historyRepairRows && (end == start || bytes+page[end].bytes <= historyRepairBytes) {
				bytes += page[end].bytes
				end++
			}
			if wrote && pause != nil {
				pause()
				if ctx.Err() != nil {
					return nil
				}
			}
			batch, sources, err := s.pruneOrphanPayloadBatch(page[start:end])
			wrote = true
			stats.Payloads += batch.Payloads
			stats.Bytes += batch.Bytes
			released = append(released, sources...)
			if err != nil {
				return err
			}
			if batch.Payloads > 0 {
				if err := s.checkpointHistoryRepair(); err != nil {
					return err
				}
			}
			start = end
		}
		return nil
	}
	if err := s.scanOrphanPayloads(ctx, prune); err != nil {
		return stats, err
	}
	for len(released) > 0 && ctx.Err() == nil {
		candidates := released
		released = nil
		for i := range candidates {
			var bytes int64
			err := s.reader().QueryRow(`SELECT `+payloadStoredBytesSQL+` FROM payloads p WHERE p.thread_id = ? AND p.id = ?`,
				candidates[i].threadID, candidates[i].id).Scan(&bytes)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return stats, fmt.Errorf("store: size released snapshot source: %w", err)
			}
			candidates[i].bytes = bytes
		}
		if err := prune(candidates); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// pruneOrphanPayloadBatch deletes the rows of one batch that are still
// unreferenced. It returns what it deleted and the source payloads of
// snapshots the deletes released.
func (s *Store) pruneOrphanPayloadBatch(batch []orphanPayload) (OrphanPayloadStats, []orphanPayload, error) {
	var stats OrphanPayloadStats
	tx, err := s.db.Begin()
	if err != nil {
		return stats, nil, fmt.Errorf("store: begin orphan payload prune: %w", err)
	}
	defer tx.Rollback()
	var released []orphanPayload
	for _, row := range batch {
		var source orphanPayload
		err := tx.QueryRow(`SELECT s.source_thread_id, s.payload_id FROM payload_snapshot_refs r
 JOIN payload_snapshots s ON s.id = r.snapshot_id
 WHERE r.thread_id = ? AND r.payload_id = ? AND s.source_thread_id IS NOT NULL`, row.threadID, row.id).Scan(&source.threadID, &source.id)
		borrowed := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return OrphanPayloadStats{}, nil, fmt.Errorf("store: read orphan payload snapshot: %w", err)
		}
		result, err := tx.Exec(`DELETE FROM payloads WHERE thread_id = ? AND id = ?
 AND NOT `+payloadReferencedSQL("payloads.thread_id", "payloads.id"), row.threadID, row.id)
		if err != nil {
			return OrphanPayloadStats{}, nil, fmt.Errorf("store: delete orphan payload %s/%s: %w", row.threadID, row.id, err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return OrphanPayloadStats{}, nil, fmt.Errorf("store: count deleted orphan payload %s/%s: %w", row.threadID, row.id, err)
		}
		if deleted == 0 {
			continue
		}
		stats.Payloads++
		stats.Bytes += row.bytes
		if borrowed {
			released = append(released, source)
		}
	}
	if err := tx.Commit(); err != nil {
		return OrphanPayloadStats{}, nil, fmt.Errorf("store: commit orphan payload prune: %w", err)
	}
	return stats, released, nil
}

func queryStringsTx(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		values = append(values, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return values, nil
}
