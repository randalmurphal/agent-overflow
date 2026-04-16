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

func (s *Store) UpsertTurnPayload(threadID string, turnIndex int, kind string, payload Payload) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: upsert turn payload begin: %w", err)
	}
	defer tx.Rollback()

	var existingID string
	lookupErr := tx.QueryRow(
		`SELECT payload_id
		 FROM items
		 WHERE thread_id = ? AND turn_index = ? AND kind = ? AND payload_id IS NOT NULL
		 ORDER BY item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	).Scan(&existingID)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return fmt.Errorf("store: upsert turn payload lookup: %w", lookupErr)
	}
	if existingID != "" {
		payload.ID = existingID
	}

	_, err = tx.Exec(
		`INSERT OR REPLACE INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		payload.ID, payload.Kind, payload.Meta, payload.Data, payload.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert turn payload: %w", err)
	}
	return tx.Commit()
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
