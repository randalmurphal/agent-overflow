package store

import (
	"database/sql"
	"fmt"
)

// IsEmptyDraftThread reports whether threadID is still a materialized chat/plan
// draft that has not gained durable conversation state. A worktree counts as
// durable state: deleting the thread row would orphan a checkout the user
// deliberately created.
func (s *Store) IsEmptyDraftThread(threadID string) (bool, error) {
	if threadID == "" {
		return false, fmt.Errorf("store: check empty draft thread: thread id is required")
	}
	var exists int
	err := s.reader().QueryRow(
		`SELECT 1
		   FROM threads
		  WHERE id = ?
		    AND mode IN ('chat', 'plan')
		    AND COALESCE(worktree_path, '') = ''
		    AND NOT EXISTS (
		    	SELECT 1 FROM threads child WHERE child.parent_thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM items WHERE items.thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM turns WHERE turns.thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM thread_drafts
		    	 WHERE thread_drafts.thread_id = threads.id
		    	   AND thread_drafts.has_content = 1
		    )`,
		threadID,
	).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("store: check empty draft thread %s: %w", threadID, err)
}

// DeleteEmptyDraftThread removes a materialized draft row that never gained
// durable conversation state. It is intentionally narrow: terminal, design,
// discussion, sent, and in-flight threads are outside this cleanup path.
func (s *Store) DeleteEmptyDraftThread(threadID string) (bool, error) {
	if threadID == "" {
		return false, fmt.Errorf("store: delete empty draft thread: thread id is required")
	}
	result, err := s.db.Exec(
		`DELETE FROM threads
		  WHERE id = ?
		    AND mode IN ('chat', 'plan')
		    AND COALESCE(worktree_path, '') = ''
		    AND NOT EXISTS (
		    	SELECT 1 FROM threads child WHERE child.parent_thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM items WHERE items.thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM turns WHERE turns.thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM thread_drafts
		    	 WHERE thread_drafts.thread_id = threads.id
		    	   AND thread_drafts.has_content = 1
		    )`,
		threadID,
	)
	if err != nil {
		return false, fmt.Errorf("store: delete empty draft thread %s: %w", threadID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete empty draft thread %s rows affected: %w", threadID, err)
	}
	return affected > 0, nil
}
