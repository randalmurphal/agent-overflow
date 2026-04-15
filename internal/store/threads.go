package store

import "fmt"

func (s *Store) CreateThread(t Thread) error {
	_, err := s.db.Exec(
		`INSERT INTO threads (id, title, provider, session_ref, workspace_path, model, created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model, t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived),
	)
	if err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model, created_at, updated_at, archived
		 FROM threads WHERE id = ?`, id,
	)
	var t Thread
	var archived int
	err := row.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model, &t.CreatedAt, &t.UpdatedAt, &archived)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	t.Archived = archived != 0
	return t, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model, created_at, updated_at, archived
		 FROM threads WHERE archived = 0 ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var archived int
		if err := rows.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model, &t.CreatedAt, &t.UpdatedAt, &archived); err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		t.Archived = archived != 0
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) UpdateThread(t Thread) error {
	_, err := s.db.Exec(
		`UPDATE threads SET title=?, provider=?, session_ref=?, workspace_path=?, model=?, updated_at=?, archived=?
		 WHERE id=?`,
		t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model, t.UpdatedAt, boolToInt(t.Archived), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	return nil
}

func (s *Store) DeleteThread(id string) error {
	_, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread %s: %w", id, err)
	}
	return nil
}

func (s *Store) ArchiveThread(id string) error {
	_, err := s.db.Exec(`UPDATE threads SET archived = 1, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: archive thread %s: %w", id, err)
	}
	return nil
}

func (s *Store) UpdateSessionRef(threadID, ref string) error {
	_, err := s.db.Exec(`UPDATE threads SET session_ref = ?, updated_at = ? WHERE id = ?`,
		ref, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	return nil
}
