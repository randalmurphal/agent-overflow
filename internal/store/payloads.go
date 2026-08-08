package store

import (
	"database/sql"
	"fmt"
)

func upsertPayloadTx(exec sqlExecutor, payload Payload, label string) error {
	if _, err := exec.Exec(`DELETE FROM payload_chunks WHERE payload_id = ?`, payload.ID); err != nil {
		return fmt.Errorf("%s clear chunks: %w", label, err)
	}
	if _, err := exec.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

const payloadInsertSQL = `INSERT INTO payloads (id, kind, meta, data, created_at)
	 VALUES (?, ?, ?, ?, ?)`

// payloadInsertArgs is the bind list payloadInsertSQL takes, in column
// order — shared with the prepared-statement bulk path in
// ApplyImportBatch so the two cannot drift.
func payloadInsertArgs(payload Payload) []any {
	return []any{payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt}
}

func insertPayloadTx(exec sqlExecutor, payload Payload, label string) error {
	if _, err := exec.Exec(payloadInsertSQL, payloadInsertArgs(payload)...); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func (s *Store) InsertPayload(p Payload) error {
	return insertPayloadTx(s.db, p, "store: insert payload")
}

// InsertItemWithPayload persists a payload + its matching item atomically
// in a single transaction. Either both land or neither does, so triage
// never leaves an orphan payload row when the item insert fails (Bug B10).
// Thread activity is bumped explicitly via Store.MarkThreadActivity from
// triage at user_text persist / turn settle / approval-or-input request,
// not implicitly here.
func (s *Store) InsertItemWithPayload(item Item, payload Payload) error {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin insert item+payload tx: %w", err)
	}
	defer tx.Rollback()

	if err := insertPayloadTx(tx, payload, "store: insert payload"); err != nil {
		return err
	}

	if err := insertItemTx(tx, item, "store: insert item"); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit insert item+payload tx: %w", err)
	}
	return nil
}

// AppendItemWithPayload is the append-at-next-index variant of
// InsertItemWithPayload. The item's ItemIndex is ignored; the store
// computes MAX(item_index)+1 inside the transaction so concurrent
// appenders for the same (thread, turn) cannot collide. Returns the
// assigned item_index. Prefer this over NextItemIndex + InsertItemWithPayload
// when you don't need to force a specific index.
func (s *Store) AppendItemWithPayload(item Item, payload Payload) (int, error) {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin append item+payload tx: %w", err)
	}
	defer tx.Rollback()

	next, err := nextItemIndexTx(tx, item.ThreadID, item.TurnIndex, "store: append item+payload next index")
	if err != nil {
		return 0, err
	}
	item.ItemIndex = next

	if err := insertPayloadTx(tx, payload, "store: append item+payload insert payload"); err != nil {
		return 0, err
	}

	if err := insertItemTx(tx, item, "store: append item+payload insert item"); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append item+payload tx: %w", err)
	}
	return next, nil
}

func (s *Store) UpsertPayload(p Payload) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin upsert payload %s: %w", p.ID, err)
	}
	defer tx.Rollback()
	if err := upsertPayloadTx(tx, p, fmt.Sprintf("store: upsert payload %s", p.ID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert payload %s: %w", p.ID, err)
	}
	return nil
}

func (s *Store) GetPayloadMeta(id string) (PayloadMeta, error) {
	row := s.reader().QueryRow(
		`SELECT id, kind, meta, created_at FROM payloads WHERE id = ?`, id,
	)
	var pm PayloadMeta
	err := row.Scan(&pm.ID, &pm.Kind, &pm.Meta, &pm.CreatedAt)
	if err != nil {
		return PayloadMeta{}, fmt.Errorf("store: get payload meta %s: %w", id, err)
	}
	return pm, nil
}

func (s *Store) GetPayloadData(id string) ([]byte, error) {
	var data []byte
	err := s.reader().QueryRow(`SELECT data FROM payloads WHERE id = ?`, id).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("store: get payload data %s: %w", id, err)
	}
	rows, err := s.reader().Query(
		`SELECT data
		   FROM payload_chunks
		  WHERE payload_id = ?
		  ORDER BY chunk_index`,
		id,
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
func (s *Store) GetPayloadPreview(id string, maxBytes int) ([]byte, int, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return s.GetPayloadChunk(id, 0, maxBytes)
}

// GetPayloadChunk returns a bounded payload slice starting at byte offset.
// Slicing happens inside SQLite for the base blob and uses payload chunk
// offsets for append-backed data, so loading the next 256 KB chunk of a
// 50 MB command output never materializes the full payload in Go memory.
func (s *Store) GetPayloadChunk(id string, offset, maxBytes int) ([]byte, int, bool, error) {
	if offset < 0 {
		offset = 0
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	baseLen, total, err := s.payloadLengths(id)
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
		err := s.reader().QueryRow(
			`SELECT substr(data, ?, ?) FROM payloads WHERE id = ?`,
			offset+1, baseLimit-offset, id,
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
			   FROM payload_chunks
			  WHERE payload_id = ?
			    AND start_offset + length(data) > ?
			    AND start_offset < ?
			  ORDER BY chunk_index`,
			offset, offset, limit-offset, id, offset, limit,
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

func (s *Store) payloadLengths(id string) (int, int, error) {
	var baseLen int
	var appendedEnd sql.NullInt64
	err := s.reader().QueryRow(
		`SELECT length(data),
		        (SELECT MAX(start_offset + length(data))
		           FROM payload_chunks
		          WHERE payload_id = payloads.id)
		   FROM payloads
		  WHERE id = ?`,
		id,
	).Scan(&baseLen, &appendedEnd)
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
// Returns sql.ErrNoRows (wrapped) if no payload matches id. Callers
// must handle "no row yet" by inserting via InsertPayload first; this
// method only handles the append path.
func (s *Store) AppendPayloadData(id string, delta []byte, meta string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin append payload data %s: %w", id, err)
	}
	defer tx.Rollback()

	if err := appendPayloadDataTx(tx, id, delta, meta, createdAt); err != nil {
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
func appendPayloadDataTx(tx *sql.Tx, id string, delta []byte, meta string, createdAt int64) error {
	result, err := tx.Exec(
		`UPDATE payloads SET meta = ?, created_at = ? WHERE id = ?`,
		meta, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: append payload data %s: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: append payload data %s", id)); err != nil {
		return err
	}
	if len(delta) > 0 {
		var chunkIndex int
		var startOffset int
		err := tx.QueryRow(
			`SELECT COALESCE(MAX(chunk_index) + 1, 0),
			        COALESCE(
			            MAX(start_offset + length(data)),
			            (SELECT length(data) FROM payloads WHERE id = ?)
			        )
			   FROM payload_chunks
			  WHERE payload_id = ?`,
			id, id,
		).Scan(&chunkIndex, &startOffset)
		if err != nil {
			return fmt.Errorf("store: append payload next chunk %s: %w", id, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO payload_chunks (payload_id, chunk_index, start_offset, data, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			id, chunkIndex, startOffset, delta, createdAt,
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
func (s *Store) ReplacePayloadData(id string, data []byte, meta string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replace payload data %s: %w", id, err)
	}
	defer tx.Rollback()

	// Clearing the span columns in the same UPDATE keeps a replaced
	// payload from carrying span blobs computed for the superseded
	// content; the persist tap recomputes them for the new data. The
	// blobs are content-addressed per file, so a stale blob would be
	// inert anyway — clearing just keeps the row honest.
	result, err := tx.Exec(
		`UPDATE payloads SET data = ?, meta = ?, created_at = ?, preview_spans = '', spans = '' WHERE id = ?`,
		data, meta, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: replace payload data %s: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: replace payload data %s", id)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM payload_chunks WHERE payload_id = ?`, id); err != nil {
		return fmt.Errorf("store: replace payload data clear chunks %s: %w", id, err)
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
// Returns sql.ErrNoRows (wrapped) if no payload matches id.
func (s *Store) UpdatePayloadMeta(id, meta string) error {
	result, err := s.db.Exec(
		`UPDATE payloads SET meta = ? WHERE id = ?`,
		meta, id,
	)
	if err != nil {
		return fmt.Errorf("store: update payload meta %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update payload meta %s", id))
}

// UpdatePayloadSpans stores the version-stamped highlight span blobs for
// an existing payload: previewSpans covers the inline-diff preview
// patches (joined into item list reads), spans the full data blob (read
// by the on-demand payload loads). Written by the app-layer persist tap
// after payload persistence; both columns are always set together so a
// recompute can never leave one half stale.
//
// Returns sql.ErrNoRows (wrapped) if no payload matches id — the
// span worker racing a thread deletion hits this and treats it as a
// benign drop.
func (s *Store) UpdatePayloadSpans(id, previewSpans, spans string) error {
	result, err := s.db.Exec(
		`UPDATE payloads SET preview_spans = ?, spans = ? WHERE id = ?`,
		previewSpans, spans, id,
	)
	if err != nil {
		return fmt.Errorf("store: update payload spans %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update payload spans %s", id))
}

// GetPayloadSpans returns a payload's full-data span blob (the spans
// column). Empty string means "not computed" — the caller falls back to
// the highlight RPC path.
func (s *Store) GetPayloadSpans(id string) (string, error) {
	var spans string
	if err := s.reader().QueryRow(`SELECT spans FROM payloads WHERE id = ?`, id).Scan(&spans); err != nil {
		return "", fmt.Errorf("store: get payload spans %s: %w", id, err)
	}
	return spans, nil
}
