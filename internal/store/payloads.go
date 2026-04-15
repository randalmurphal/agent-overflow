package store

import "fmt"

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
