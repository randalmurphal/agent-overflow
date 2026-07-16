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

func insertPayloadTx(exec sqlExecutor, payload Payload, label string) error {
	if _, err := exec.Exec(
		`INSERT INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
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

// UpsertTurnPayload writes payload bytes for (thread, turn, kind) and links
// the latest matching item to the stored payload. If an existing item already
// has a payload_id, its payload row is replaced in place so we don't orphan
// the old one. If the matching item has no payload_id yet (NULL), we insert
// the new payload under the caller-supplied id and update the item's
// payload_id column in the same transaction — without that link, the newly
// inserted payload would be unreachable from any item and never garbage
// collected.
//
// When no matching item exists yet, we still insert the payload so the caller
// (typically the triage router) can persist the item immediately afterward
// under the same payload id.
func (s *Store) UpsertTurnPayload(threadID string, turnIndex int, kind string, payload Payload) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: upsert turn payload begin: %w", err)
	}
	defer tx.Rollback()

	// Preferred path: the latest item already has a payload_id. Replace the
	// payload row in place keyed by that id.
	var existingID string
	lookupLinked := tx.QueryRow(
		`SELECT payload_id
		 FROM items
		 WHERE thread_id = ? AND turn_index = ? AND kind = ? AND payload_id IS NOT NULL
		 ORDER BY item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	).Scan(&existingID)
	if lookupLinked != nil && lookupLinked != sql.ErrNoRows {
		return fmt.Errorf("store: upsert turn payload lookup linked: %w", lookupLinked)
	}
	if existingID != "" {
		payload.ID = existingID
	}

	if err := upsertPayloadTx(tx, payload, "store: upsert turn payload"); err != nil {
		return err
	}

	// Fallback path: we may have just inserted a brand new payload. If there
	// is an item for this (thread, turn, kind) that still has a NULL
	// payload_id, link it to the new payload now so it never becomes an
	// orphan. This is a no-op when the preferred path above already matched.
	if existingID == "" {
		var unlinkedItemID string
		lookupUnlinked := tx.QueryRow(
			`SELECT id
			 FROM items
			 WHERE thread_id = ? AND turn_index = ? AND kind = ? AND payload_id IS NULL
			 ORDER BY item_index DESC
			 LIMIT 1`,
			threadID, turnIndex, kind,
		).Scan(&unlinkedItemID)
		if lookupUnlinked != nil && lookupUnlinked != sql.ErrNoRows {
			return fmt.Errorf("store: upsert turn payload lookup unlinked item: %w", lookupUnlinked)
		}
		if unlinkedItemID != "" {
			if _, err := tx.Exec(
				`UPDATE items SET payload_id = ? WHERE id = ?`,
				payload.ID, unlinkedItemID,
			); err != nil {
				return fmt.Errorf("store: upsert turn payload link item %s: %w", unlinkedItemID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: upsert turn payload commit: %w", err)
	}
	return nil
}

func (s *Store) GetPayloadMeta(id string) (PayloadMeta, error) {
	row := s.db.QueryRow(
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
	err := s.db.QueryRow(`SELECT data FROM payloads WHERE id = ?`, id).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("store: get payload data %s: %w", id, err)
	}
	rows, err := s.db.Query(
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
		err := s.db.QueryRow(
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
		rows, err := s.db.Query(
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
	err := s.db.QueryRow(
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit append payload data %s: %w", id, err)
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
	if err := s.db.QueryRow(`SELECT spans FROM payloads WHERE id = ?`, id).Scan(&spans); err != nil {
		return "", fmt.Errorf("store: get payload spans %s: %w", id, err)
	}
	return spans, nil
}

func (s *Store) ListPayloadMetas(threadID string) ([]PayloadMeta, error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.kind, p.meta, p.created_at
		 FROM payloads p
		 INNER JOIN items i ON i.payload_id = p.id
		 WHERE i.thread_id = ?
		 ORDER BY i.turn_index, i.item_index`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list payload metas for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var metas []PayloadMeta
	for rows.Next() {
		var pm PayloadMeta
		if err := rows.Scan(&pm.ID, &pm.Kind, &pm.Meta, &pm.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan payload meta row: %w", err)
		}
		metas = append(metas, pm)
	}
	return metas, rows.Err()
}
