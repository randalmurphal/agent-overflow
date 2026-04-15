package store

import "fmt"

func (s *Store) CreateThread(t Thread) error {
	t.InteractionMode = normalizeInteractionMode(t.InteractionMode)
	_, err := s.db.Exec(
		`INSERT INTO threads (id, title, provider, session_ref, workspace_path, model,
		    project_path, worktree_path, branch, interaction_mode, discussion_id, parent_thread_id,
		    created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model,
		t.ProjectPath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch), t.InteractionMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID),
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived),
	)
	if err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model,
		    project_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
		    interaction_mode, COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
		    created_at, updated_at, archived
		 FROM threads WHERE id = ?`, id,
	)
	var t Thread
	var archived int
	err := row.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model,
		&t.ProjectPath, &t.WorktreePath, &t.Branch,
		&t.InteractionMode, &t.DiscussionID, &t.ParentThreadID,
		&t.CreatedAt, &t.UpdatedAt, &archived)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	t.Archived = archived != 0
	return t, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model,
		    project_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
		    interaction_mode, COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
		    created_at, updated_at, archived
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
		if err := rows.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model,
			&t.ProjectPath, &t.WorktreePath, &t.Branch,
			&t.InteractionMode, &t.DiscussionID, &t.ParentThreadID,
			&t.CreatedAt, &t.UpdatedAt, &archived); err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		t.Archived = archived != 0
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) UpdateThread(t Thread) error {
	t.InteractionMode = normalizeInteractionMode(t.InteractionMode)
	_, err := s.db.Exec(
		`UPDATE threads SET title=?, provider=?, session_ref=?, workspace_path=?, model=?,
		    project_path=?, worktree_path=?, branch=?, interaction_mode=?,
		    discussion_id=?, parent_thread_id=?,
		    updated_at=?, archived=?
		 WHERE id=?`,
		t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model,
		t.ProjectPath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch), t.InteractionMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID),
		t.UpdatedAt, boolToInt(t.Archived), t.ID,
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

func (s *Store) UpdateTitle(threadID, title string) error {
	_, err := s.db.Exec(`UPDATE threads SET title = ?, updated_at = ? WHERE id = ?`,
		title, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update title for %s: %w", threadID, err)
	}
	return nil
}

func (s *Store) UpdateModel(threadID, model string) error {
	_, err := s.db.Exec(`UPDATE threads SET model = ?, updated_at = ? WHERE id = ?`,
		model, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update model for %s: %w", threadID, err)
	}
	return nil
}

func normalizeInteractionMode(mode string) string {
	if mode == "" {
		return "default"
	}
	return mode
}
