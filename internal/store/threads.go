package store

import "fmt"

const threadColumns = `id, title, provider, COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''), workspace_path, model,
    project_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    interaction_mode, COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''), COALESCE(forked_from_thread_id, ''),
    created_at, updated_at, archived`

func scanThread(scanner interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var archived int
	if err := scanner.Scan(
		&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.PendingForkRef, &t.WorkspacePath, &t.Model,
		&t.ProjectPath, &t.WorktreePath, &t.Branch,
		&t.InteractionMode, &t.DiscussionID, &t.ParentThreadID, &t.ForkedFromThreadID,
		&t.CreatedAt, &t.UpdatedAt, &archived,
	); err != nil {
		return Thread{}, err
	}
	t.Archived = archived != 0
	return t, nil
}

func (s *Store) CreateThread(t Thread) error {
	t.InteractionMode = normalizeInteractionMode(t.InteractionMode)
	_, err := s.db.Exec(
		`INSERT INTO threads (id, title, provider, session_ref, pending_fork_session_ref, workspace_path, model,
		    project_path, worktree_path, branch, interaction_mode, discussion_id, parent_thread_id,
		    forked_from_thread_id, created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Provider, nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef), t.WorkspacePath, t.Model,
		t.ProjectPath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch), t.InteractionMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID),
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived),
	)
	if err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT `+threadColumns+` FROM threads WHERE id = ?`, id,
	)
	t, err := scanThread(row)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	return t, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads WHERE archived = 0 ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) ListChildThreads(parentID string) ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT `+threadColumns+` FROM threads WHERE parent_thread_id = ? ORDER BY created_at ASC`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list child threads for %s: %w", parentID, err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan child thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) UpdateThread(t Thread) error {
	t.InteractionMode = normalizeInteractionMode(t.InteractionMode)
	result, err := s.db.Exec(
		`UPDATE threads SET title=?, provider=?, session_ref=?, pending_fork_session_ref=?, workspace_path=?, model=?,
		    project_path=?, worktree_path=?, branch=?, interaction_mode=?,
		    discussion_id=?, parent_thread_id=?, forked_from_thread_id=?,
		    updated_at=?, archived=?
		 WHERE id=?`,
		t.Title, t.Provider, nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef), t.WorkspacePath, t.Model,
		t.ProjectPath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch), t.InteractionMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID),
		t.UpdatedAt, boolToInt(t.Archived), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update thread %s", t.ID))
}

func (s *Store) DeleteThread(id string) error {
	result, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete thread %s", id))
}

func (s *Store) ArchiveThread(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET archived = 1, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: archive thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: archive thread %s", id))
}

// UnarchiveThread flips the archived column back to 0 for a thread and bumps
// updated_at so the sidebar reshuffles it toward the top of the active list.
// Returns an error if no row matches the id.
func (s *Store) UnarchiveThread(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET archived = 0, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: unarchive thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: unarchive thread %s", id))
}

func (s *Store) UpdateSessionRef(threadID, ref string) error {
	result, err := s.db.Exec(
		`UPDATE threads
		 SET session_ref = ?, pending_fork_session_ref = NULL, updated_at = ?
		 WHERE id = ?`,
		ref, nowMillis(), threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update session ref for %s", threadID))
}

func (s *Store) UpdateTitle(threadID, title string) error {
	result, err := s.db.Exec(`UPDATE threads SET title = ?, updated_at = ? WHERE id = ?`,
		title, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update title for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update title for %s", threadID))
}

func (s *Store) UpdateTitleIfCurrent(threadID, currentTitle, newTitle string) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE threads SET title = ?, updated_at = ? WHERE id = ? AND title = ?`,
		newTitle, nowMillis(), threadID, currentTitle,
	)
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title for %s: %w", threadID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title rows affected for %s: %w", threadID, err)
	}
	return rows > 0, nil
}

func (s *Store) UpdateModel(threadID, model string) error {
	result, err := s.db.Exec(`UPDATE threads SET model = ?, updated_at = ? WHERE id = ?`,
		model, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update model for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update model for %s", threadID))
}

func normalizeInteractionMode(mode string) string {
	if mode == "" {
		return "default"
	}
	return mode
}
