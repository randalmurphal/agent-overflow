package store

import (
	"database/sql"
	"fmt"
)

// upsertPayloadTx writes a payload whole. A payload a fork shows through a
// row of threadID's goes to the forks first (reownShownPayloadTx).
func upsertPayloadTx(tx *sql.Tx, threadID string, payload Payload, label string) error {
	if err := reownShownPayloadTx(tx, threadID, payload.ID); err != nil {
		return fmt.Errorf("%s give the forks of %s their payload: %w", label, threadID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM payload_chunks WHERE thread_id = ? AND payload_id = ?`, threadID, payload.ID,
	); err != nil {
		return fmt.Errorf("%s clear chunks: %w", label, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM edit_file_snapshots WHERE thread_id = ? AND payload_id = ?`, threadID, payload.ID,
	); err != nil {
		return fmt.Errorf("%s clear edit snapshots: %w", label, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread_id, id) DO UPDATE SET
		    kind = excluded.kind,
		    meta = excluded.meta,
		    data = excluded.data,
		    created_at = excluded.created_at,
		    preview_spans = '',
		    spans = ''`,
		payloadInsertArgs(threadID, payload)...,
	); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

const payloadInsertPrefix = `INSERT INTO payloads (thread_id, id, kind, meta, data, created_at)`
const payloadInsertValues = `(?, ?, ?, ?, ?, ?)`
const payloadInsertSQL = payloadInsertPrefix + ` VALUES ` + payloadInsertValues

// payloadInsertArgs is the bind list payloadInsertSQL takes, in column
// order — shared with the prepared-statement bulk path in
// ApplyImportBatch and with upsertPayloadTx so they cannot drift.
func payloadInsertArgs(threadID string, payload Payload) []any {
	return []any{threadID, payload.ID, payload.Kind, payload.Meta, payloadDataArg(payload.Data), payload.CreatedAt}
}

// payloadDataArg is the bind value of payload data. Every payload data column
// is BLOB NOT NULL, and the driver binds a nil slice as NULL. A nil slice is
// also what the driver scans from a zero-length blob, so an empty payload read
// back from the database arrives as nil.
func payloadDataArg(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
}

func insertPayloadTx(exec sqlExecutor, threadID string, payload Payload, label string) error {
	if _, err := exec.Exec(payloadInsertSQL, payloadInsertArgs(threadID, payload)...); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// There is deliberately no exported bare payload insert or upsert. A
// payload row is only ever window-visible through the item that
// references it, and the history contract's payload half is enforced by
// the threadID parameter every mutator carries (see
// bumpHistoryRevForPayloadTx) —
// an export that wrote `payloads` without naming a thread would be a hole
// in exactly that enforcement. The private upsert also clears derived
// chunks, snapshots, and span blobs, which must stay coupled to the item
// rewrite that made them stale. Every production write goes through an item-coupled writer:
// InsertItemWithPayload / AppendItemWithPayload create the pair,
// UpsertItem replaces it (upsertPayloadTx, items_write.go), and the
// threadID-carrying mutators below change it in place.

// InsertItemWithPayload persists a payload + its matching item atomically
// in a single transaction. Either both land or neither does, so triage
// never leaves an orphan payload row when the item insert fails (Bug B10).
// Thread activity is bumped explicitly via Store.MarkThreadActivity from
// triage at user_text persist / turn settle / approval-or-input request,
// not implicitly here.
func (s *Store) InsertItemWithPayload(item Item, payload Payload) error {
	applyItemDefaults(&item)
	return s.writeItems(item.ThreadID, item.SubagentCard, "insert item+payload", func(tx *sql.Tx, w *cardWrite) error {
		if err := insertPayloadTx(tx, item.ThreadID, payload, "store: insert payload"); err != nil {
			return err
		}
		return insertItemTx(tx, w, item, "store: insert item")
	})
}

// AppendItemWithPayload is the append-at-next-index variant of
// InsertItemWithPayload. The item's ItemIndex is ignored; the store
// computes MAX(item_index)+1 inside the transaction so concurrent
// appenders for the same (thread, turn) cannot collide. Returns the
// assigned item_index. Prefer this over NextItemIndex + InsertItemWithPayload
// when you don't need to force a specific index.
func (s *Store) AppendItemWithPayload(item Item, payload Payload) (int, error) {
	applyItemDefaults(&item)
	err := s.writeItems(item.ThreadID, item.SubagentCard, "append item+payload", func(tx *sql.Tx, w *cardWrite) error {
		next, err := nextItemIndexTx(tx, item.ThreadID, item.TurnIndex, "store: append item+payload next index")
		if err != nil {
			return err
		}
		item.ItemIndex = next
		if err := insertPayloadTx(tx, item.ThreadID, payload, "store: append item+payload insert payload"); err != nil {
			return err
		}
		return insertItemTx(tx, w, item, "store: append item+payload insert item")
	})
	if err != nil {
		return 0, err
	}
	return item.ItemIndex, nil
}

// payloadByIDQuery reads one logical payload of a thread through
// timelinePayloadArms. columns is written against alias `p`.
func payloadByIDQuery(q sqlQueryer, threadID, id, columns string) (string, []any, error) {
	return timelinePayloadArms(q, threadID, func(string, string) string { return columns }, "p.id = ?", []any{id})
}

func (s *Store) GetPayloadMeta(threadID, id string) (PayloadMeta, error) {
	q := s.reader()
	query, args, err := payloadByIDQuery(q, threadID, id, "p.id, p.kind, p.meta, p.created_at")
	if err != nil {
		return PayloadMeta{}, fmt.Errorf("store: get payload meta %s: %w", id, err)
	}
	var pm PayloadMeta
	err = q.QueryRow(query, args...).Scan(&pm.ID, &pm.Kind, &pm.Meta, &pm.CreatedAt)
	if err != nil {
		return PayloadMeta{}, fmt.Errorf("store: get payload meta %s: %w", id, err)
	}
	return pm, nil
}

func (s *Store) GetPayloadData(threadID, id string) ([]byte, error) {
	var data []byte
	query, args, err := payloadByIDQuery(s.reader(), threadID, id, "p.data")
	if err != nil {
		return nil, fmt.Errorf("store: get payload data %s: %w", id, err)
	}
	err = s.reader().QueryRow(query, args...).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("store: get payload data %s: %w", id, err)
	}
	rows, err := s.reader().Query(
		`SELECT data
		   FROM timeline_payload_chunks
		  WHERE thread_id = ? AND payload_id = ?
		  ORDER BY chunk_index`,
		threadID, id,
	)
	if err != nil {
		return nil, fmt.Errorf("store: get payload data chunks %s: %w", id, err)
	}
	defer rows.Close()

	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, fmt.Errorf("store: scan payload chunk %s: %w", id, err)
		}
		data = append(data, chunk...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: payload chunk rows %s: %w", id, err)
	}
	return data, nil
}

// GetPayloadPreview returns up to maxBytes of the payload prefix together
// with the full payload length and a completion flag. For blob-backed
// payloads the slice still happens inside SQLite; for append-backed
// payloads only chunks that overlap the requested prefix are read.
// The completion flag is true only when total <= maxBytes.
func (s *Store) GetPayloadPreview(threadID, id string, maxBytes int) ([]byte, int, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return s.GetPayloadChunk(threadID, id, 0, maxBytes)
}

// GetPayloadChunk returns a bounded payload slice starting at byte offset.
// Slicing happens inside SQLite for the base blob and uses payload chunk
// offsets for append-backed data, so loading the next 256 KB chunk of a
// 50 MB command output never materializes the full payload in Go memory.
func (s *Store) GetPayloadChunk(threadID, id string, offset, maxBytes int) ([]byte, int, bool, error) {
	if offset < 0 {
		offset = 0
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	baseLen, total, err := s.payloadLengths(threadID, id)
	if err != nil {
		return nil, 0, false, err
	}
	if maxBytes == 0 || offset >= total {
		return []byte{}, total, offset >= total, nil
	}

	requestOffset := offset
	limit := offset + maxBytes
	if limit > total {
		limit = total
	}
	result := make([]byte, 0, limit-offset)
	if offset < baseLen {
		baseLimit := limit
		if baseLimit > baseLen {
			baseLimit = baseLen
		}
		var base []byte
		query, args, err := payloadByIDQuery(s.reader(), threadID, id, "p.data AS data")
		if err != nil {
			return nil, 0, false, fmt.Errorf("store: get payload base chunk %s: %w", id, err)
		}
		err = s.reader().QueryRow(
			`SELECT substr(data, ?, ?) FROM (`+query+`)`,
			append([]any{offset + 1, baseLimit - offset}, args...)...,
		).Scan(&base)
		if err != nil {
			return nil, 0, false, fmt.Errorf("store: get payload base chunk %s: %w", id, err)
		}
		result = append(result, base...)
		offset = baseLimit
	}
	if offset < limit {
		rows, err := s.reader().Query(
			`SELECT substr(
			            data,
			            CASE WHEN ? > start_offset THEN ? - start_offset + 1 ELSE 1 END,
			            ?
			        )
			   FROM timeline_payload_chunks
			  WHERE thread_id = ? AND payload_id = ?
			    AND start_offset + data_length > ?
			    AND start_offset < ?
			  ORDER BY chunk_index`,
			offset, offset, limit-offset, threadID, id, offset, limit,
		)
		if err != nil {
			return nil, 0, false, fmt.Errorf("store: get payload chunks %s: %w", id, err)
		}
		defer rows.Close()
		for rows.Next() {
			var chunk []byte
			if err := rows.Scan(&chunk); err != nil {
				return nil, 0, false, fmt.Errorf("store: scan payload chunk %s: %w", id, err)
			}
			remaining := limit - (requestOffset + len(result))
			if remaining <= 0 {
				break
			}
			if len(chunk) > remaining {
				chunk = chunk[:remaining]
			}
			result = append(result, chunk...)
		}
		if err := rows.Err(); err != nil {
			return nil, 0, false, fmt.Errorf("store: payload chunk rows %s: %w", id, err)
		}
	}
	nextOffset := requestOffset + len(result)
	return result, total, nextOffset >= total, nil
}

// payloadLengthsQuery reads a payload's base length and the end of its
// appended chunks.
func payloadLengthsQuery(q sqlQueryer, threadID, id string) (string, []any, error) {
	base, args, err := timelinePayloadArms(q, threadID, func(_, dataLength string) string {
		return dataLength + " AS data_length"
	}, "p.id = ?", []any{id})
	if err != nil {
		return "", nil, err
	}
	return `SELECT data_length,
		        (SELECT MAX(start_offset + data_length)
		           FROM timeline_payload_chunks
		          WHERE thread_id = ? AND payload_id = ?)
		   FROM (` + base + `)`, append([]any{threadID, id}, args...), nil
}

func (s *Store) payloadLengths(threadID, id string) (int, int, error) {
	var baseLen int
	var appendedEnd sql.NullInt64
	query, args, err := payloadLengthsQuery(s.reader(), threadID, id)
	if err != nil {
		return 0, 0, fmt.Errorf("store: get payload length %s: %w", id, err)
	}
	err = s.reader().QueryRow(query, args...).Scan(&baseLen, &appendedEnd)
	if err != nil {
		return 0, 0, fmt.Errorf("store: get payload length %s: %w", id, err)
	}
	totalSize := baseLen
	if appendedEnd.Valid && int(appendedEnd.Int64) > totalSize {
		totalSize = int(appendedEnd.Int64)
	}
	return baseLen, totalSize, nil
}

// AppendPayloadData appends delta to an existing payload as a new ordered
// payload_chunks row and updates its meta and created_at stamp. This keeps
// live streaming payload writes O(delta) instead of rewriting the cumulative
// payload blob on every flush.
//
// threadID is the thread whose history this payload belongs to: payload
// content rides item rows on the wire, so changing it changes what a
// windowed read returns — including the revision of every item row that
// references the payload. See bumpHistoryRevForPayloadTx.
//
// Returns sql.ErrNoRows (wrapped) if no payload matches id. Callers must
// handle "no row yet" by creating the item+payload pair first
// (InsertItemWithPayload / AppendItemWithPayload / UpsertItem); this
// method only handles the append path.
func (s *Store) AppendPayloadData(threadID, id string, delta []byte, meta string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin append payload data %s: %w", id, err)
	}
	defer tx.Rollback()

	if err := appendPayloadDataTx(tx, threadID, id, delta, meta, createdAt); err != nil {
		return err
	}
	if err := bumpHistoryRevForPayloadTx(tx, threadID, id, fmt.Sprintf("store: append payload data %s", id)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit append payload data %s: %w", id, err)
	}
	return nil
}

// appendPayloadDataTx is AppendPayloadData's body inside a caller-owned
// transaction, shared with the combined streaming-flush writers so one
// flush window costs one transaction instead of two.
func appendPayloadDataTx(tx *sql.Tx, threadID, id string, delta []byte, meta string, createdAt int64) error {
	label := fmt.Sprintf("store: append payload data %s", id)
	if err := requireMutablePayloadTx(tx, threadID, id, label); err != nil {
		return err
	}
	result, err := tx.Exec(
		`UPDATE payloads SET meta = ?, created_at = ? WHERE thread_id = ? AND id = ?`,
		meta, createdAt, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := requireRowsAffected(result, label); err != nil {
		return err
	}
	if len(delta) > 0 {
		var chunkIndex int
		var startOffset int
		err := tx.QueryRow(
			`SELECT COALESCE(MAX(chunk_index) + 1, 0),
			        COALESCE(
			            MAX(start_offset + length(data)),
			            (SELECT length(data) FROM payloads WHERE thread_id = ? AND id = ?)
			        )
			   FROM payload_chunks
			  WHERE thread_id = ? AND payload_id = ?`,
			threadID, id, threadID, id,
		).Scan(&chunkIndex, &startOffset)
		if err != nil {
			return fmt.Errorf("store: append payload next chunk %s: %w", id, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			threadID, id, chunkIndex, startOffset, delta, createdAt,
		); err != nil {
			return fmt.Errorf("store: insert payload chunk %s: %w", id, err)
		}
	}
	return nil
}

// ReplacePayloadData replaces an existing payload's data blob in-place,
// clearing any append chunks and updating its meta and created_at stamp in
// the same transaction. Streaming paths use this when a provider completion
// sends an authoritative final payload that should supersede accumulated
// deltas.
//
// threadID names the thread whose history_rev this advances — see
// AppendPayloadData.
func (s *Store) ReplacePayloadData(threadID, id string, data []byte, meta string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replace payload data %s: %w", id, err)
	}
	defer tx.Rollback()
	label := fmt.Sprintf("store: replace payload data %s", id)
	if err := requireMutablePayloadTx(tx, threadID, id, label); err != nil {
		return err
	}

	// Clearing the span columns in the same UPDATE keeps a replaced
	// payload from carrying span blobs computed for the superseded
	// content; the persist tap recomputes them for the new data. The
	// blobs are content-addressed per file, so a stale blob would be
	// inert anyway — clearing just keeps the row honest.
	result, err := tx.Exec(
		`UPDATE payloads SET data = ?, meta = ?, created_at = ?, preview_spans = '', spans = ''
		  WHERE thread_id = ? AND id = ?`,
		payloadDataArg(data), meta, createdAt, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("store: replace payload data %s: %w", id, err)
	}
	if err := requireRowsAffected(result, label); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM payload_chunks WHERE thread_id = ? AND payload_id = ?`, threadID, id,
	); err != nil {
		return fmt.Errorf("store: replace payload data clear chunks %s: %w", id, err)
	}
	if err := bumpHistoryRevForPayloadTx(tx, threadID, id, fmt.Sprintf("store: replace payload data %s", id)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit replace payload data %s: %w", id, err)
	}
	return nil
}

// UpdatePayloadMeta updates only the meta column of an existing payload
// without touching the data blob. Used for signature / preview patches
// that arrive after the blob is already assembled — callers that
// previously did GetPayloadData + re-insert to update meta now avoid
// the full-blob round trip.
//
// threadID names the thread whose history_rev this advances — see
// AppendPayloadData. It is what makes the write a transaction rather than
// a bare statement.
//
// Returns sql.ErrNoRows (wrapped) if no payload matches id.
func (s *Store) UpdatePayloadMeta(threadID, id, meta string) error {
	label := fmt.Sprintf("store: update payload meta %s", id)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update payload meta %s: %w", id, err)
	}
	defer tx.Rollback()
	if err := requireMutablePayloadTx(tx, threadID, id, label); err != nil {
		return err
	}

	result, err := tx.Exec(
		`UPDATE payloads SET meta = ? WHERE thread_id = ? AND id = ?`,
		meta, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := requireRowsAffected(result, label); err != nil {
		return err
	}
	if err := bumpHistoryRevForPayloadTx(tx, threadID, id, label); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update payload meta %s: %w", id, err)
	}
	return nil
}

// UpdatePayloadSpans stores the version-stamped highlight span blobs for
// an existing payload: previewSpans covers the inline-diff preview
// patches (joined into item list reads), spans the full data blob (read
// by the on-demand payload loads). Written by the app-layer persist tap
// after payload persistence; both columns are always set together so a
// recompute can never leave one half stale.
//
// threadID names the thread whose history_rev this advances: preview
// spans ride the item row on the wire (Item.PayloadPreviewSpans), so a
// backfill genuinely changes what a windowed read returns.
//
// It is the one payload mutator that bumps a THREAD without stamping the
// item rows (bumpHistoryRevForPayloadTx explains the split): the thread
// that holds the payload, not the forks that show its rows. Spans are a
// derived highlight cache the client version-checks against the payload
// content it already holds, so a held window whose spans are behind is
// still a correct window and must not be forced to re-page.
//
// Returns sql.ErrNoRows (wrapped) if no payload matches id — the
// span worker racing a thread deletion hits this and treats it as a
// benign drop.
func (s *Store) UpdatePayloadSpans(threadID, id, previewSpans, spans string) error {
	label := fmt.Sprintf("store: update payload spans %s", id)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update payload spans %s: %w", id, err)
	}
	defer tx.Rollback()
	// Spans are a cache of the payload's content, so they are written where
	// the content lives: a pointer fork's inherited payload gets its spans
	// on the ancestor's row, and every fork that reads it sees them.
	holder, err := payloadHolderTx(tx, threadID, id)
	if err != nil {
		return err
	}
	if err := ensureLocalPayloadTx(tx, holder, id, label); err != nil {
		return err
	}

	result, err := tx.Exec(
		`UPDATE payloads SET preview_spans = ?, spans = ? WHERE thread_id = ? AND id = ?`,
		previewSpans, spans, holder, id,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := requireRowsAffected(result, label); err != nil {
		return err
	}
	if err := bumpHistoryRevTx(tx, holder, label); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update payload spans %s: %w", id, err)
	}
	return nil
}

// GetPayloadSpans returns a payload's full-data span blob (the spans
// column). Empty string means "not computed" — the caller falls back to
// the highlight RPC path.
func (s *Store) GetPayloadSpans(threadID, id string) (string, error) {
	var spans string
	query, args, err := payloadByIDQuery(s.reader(), threadID, id, "p.spans")
	if err != nil {
		return "", fmt.Errorf("store: get payload spans %s: %w", id, err)
	}
	if err := s.reader().QueryRow(query, args...).Scan(&spans); err != nil {
		return "", fmt.Errorf("store: get payload spans %s: %w", id, err)
	}
	return spans, nil
}
