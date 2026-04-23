package store

import (
	"database/sql"
	"fmt"
)

func (s *Store) InsertPayload(p Payload) error {
	_, err := s.db.Exec(
		`INSERT INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Kind, p.Meta, p.Data, p.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert payload: %w", err)
	}
	return nil
}

// InsertItemWithPayload persists a payload + its matching item atomically
// in a single transaction. Either both land or neither does, so triage
// never leaves an orphan payload row when the item insert fails (Bug B10).
// The thread's updated_at column is also bumped in the same transaction
// so the sidebar ordering stays consistent with the newly persisted item.
func (s *Store) InsertItemWithPayload(item Item, payload Payload) error {
	applyItemDefaults(&item)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin insert item+payload tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: insert payload: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), item.ParentID,
		boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
		item.CreatedAt, item.UpdatedAt,
	); err != nil {
		return fmt.Errorf("store: insert item: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		item.UpdatedAt, item.ThreadID,
	); err != nil {
		return fmt.Errorf("store: touch thread updated_at for %s: %w", item.ThreadID, err)
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

	var maxIndex sql.NullInt64
	if err := tx.QueryRow(
		`SELECT MAX(item_index) FROM items WHERE thread_id = ? AND turn_index = ?`,
		item.ThreadID, item.TurnIndex,
	).Scan(&maxIndex); err != nil {
		return 0, fmt.Errorf("store: append item+payload next index: %w", err)
	}
	next := 0
	if maxIndex.Valid {
		next = int(maxIndex.Int64) + 1
	}
	item.ItemIndex = next

	if _, err := tx.Exec(
		`INSERT INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return 0, fmt.Errorf("store: append item+payload insert payload: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary,
		nilIfEmpty(item.PayloadID), item.ParentID,
		boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
		item.CreatedAt, item.UpdatedAt,
	); err != nil {
		return 0, fmt.Errorf("store: append item+payload insert item: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		item.UpdatedAt, item.ThreadID,
	); err != nil {
		return 0, fmt.Errorf("store: append item+payload touch thread %s: %w", item.ThreadID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append item+payload tx: %w", err)
	}
	return next, nil
}

func (s *Store) UpsertPayload(p Payload) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Kind, p.Meta, p.Data, p.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert payload %s: %w", p.ID, err)
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

	if _, err = tx.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	); err != nil {
		return fmt.Errorf("store: upsert turn payload: %w", err)
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
	return data, nil
}

// GetPayloadPreview returns up to maxBytes of the payload prefix together
// with the full payload length and a completion flag. The payload is
// sliced inside SQLite via substr() so a 10 MB blob does not ever cross
// into Go memory when the caller only wants the first 8 KB. The
// completion flag is true only when total <= maxBytes.
func (s *Store) GetPayloadPreview(id string, maxBytes int) ([]byte, int, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	var head []byte
	var total int
	// substr(blob, 1, N) is 1-indexed and clamps to the blob length, so
	// passing a maxBytes larger than the payload returns the full data
	// without an extra branch. length(blob) returns the byte count even
	// for NULL, so the NULL guard is superfluous (INSERT paths never
	// store NULL for data).
	err := s.db.QueryRow(
		`SELECT substr(data, 1, ?) AS head, length(data) AS total FROM payloads WHERE id = ?`,
		maxBytes, id,
	).Scan(&head, &total)
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: get payload preview %s: %w", id, err)
	}
	return head, total, total <= maxBytes, nil
}

// GetPayloadChunk returns a bounded payload slice starting at byte offset.
// Slicing happens inside SQLite, so loading the next 256 KB chunk of a 50 MB
// command output never materializes the full blob in Go memory.
func (s *Store) GetPayloadChunk(id string, offset, maxBytes int) ([]byte, int, bool, error) {
	if offset < 0 {
		offset = 0
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	var chunk []byte
	var total int
	err := s.db.QueryRow(
		`SELECT substr(data, ?, ?) AS chunk, length(data) AS total FROM payloads WHERE id = ?`,
		offset+1, maxBytes, id,
	).Scan(&chunk, &total)
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: get payload chunk %s: %w", id, err)
	}
	nextOffset := offset + len(chunk)
	return chunk, total, nextOffset >= total, nil
}

// AppendPayloadData appends delta to an existing payload's data blob
// in-place, updating its meta and created_at stamp inside one UPDATE.
// Unlike read-append-write (which pulled the full blob through Go on
// every delta), this streams the concatenation inside SQLite so repeated
// appends stay linear rather than quadratic in cumulative size.
//
// Returns sql.ErrNoRows (wrapped) if no payload matches id. Callers
// must handle "no row yet" by inserting via InsertPayload first; this
// method only handles the append path.
func (s *Store) AppendPayloadData(id string, delta []byte, meta string, createdAt int64) error {
	result, err := s.db.Exec(
		`UPDATE payloads SET data = data || ?, meta = ?, created_at = ? WHERE id = ?`,
		delta, meta, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: append payload data %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: append payload data %s", id))
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
