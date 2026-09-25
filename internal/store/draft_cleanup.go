package store

import (
	"fmt"
)

// emptyDraftThreadSQL is true for a materialized chat/plan draft that has
// not gained durable conversation state. A worktree counts as durable
// state: deleting the thread row would orphan a checkout the user
// deliberately created.
const emptyDraftThreadSQL = `SELECT EXISTS (SELECT 1
		   FROM threads
		  WHERE id = ?
		    AND mode IN ('chat', 'plan')
		    AND COALESCE(worktree_path, '') = ''
		    AND NOT EXISTS (
		    	SELECT 1 FROM threads child WHERE child.parent_thread_id = threads.id
		    )
		    AND NOT EXISTS (
		      SELECT 1 FROM timeline_items WHERE timeline_items.thread_id = threads.id
		    )
		    AND NOT EXISTS (
		    	SELECT 1 FROM turns WHERE turns.thread_id = threads.id
		    )
		    AND NOT EXISTS (SELECT 1 FROM thread_draft_recoveries WHERE thread_id = threads.id)
		    AND NOT EXISTS (
		    	SELECT 1 FROM thread_drafts
		    	 WHERE thread_drafts.thread_id = threads.id
		    	   AND thread_drafts.has_content = 1
		    ))`

// IsEmptyDraftThread reports whether threadID is still a materialized chat/plan
// draft that has not gained durable conversation state (emptyDraftThreadSQL).
func (s *Store) IsEmptyDraftThread(threadID string) (bool, error) {
	if threadID == "" {
		return false, fmt.Errorf("store: check empty draft thread: thread id is required")
	}
	var empty bool
	if err := s.reader().QueryRow(emptyDraftThreadSQL, threadID).Scan(&empty); err != nil {
		return false, fmt.Errorf("store: check empty draft thread %s: %w", threadID, err)
	}
	return empty, nil
}

// DeleteEmptyDraftThread removes a materialized draft row that never gained
// durable conversation state. It is intentionally narrow: terminal, design,
// discussion, sent, and in-flight threads are outside this cleanup path.
// A thread whose history was reverted away can still be read by its forks,
// through the rows it hides from its own ancestors; it becomes a holder
// (retireToHolderTx) rather than going.
func (s *Store) DeleteEmptyDraftThread(threadID string) (bool, error) {
	if threadID == "" {
		return false, fmt.Errorf("store: delete empty draft thread: thread id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin delete empty draft thread %s: %w", threadID, err)
	}
	defer tx.Rollback()
	var empty bool
	if err := tx.QueryRow(emptyDraftThreadSQL, threadID).Scan(&empty); err != nil {
		return false, fmt.Errorf("store: check empty draft thread %s: %w", threadID, err)
	}
	if !empty {
		return false, nil
	}
	reads, err := readsThroughLineageTx(tx, threadID)
	if err != nil {
		return false, err
	}
	retired, err := retireToHolderTx(tx, threadID)
	if err != nil {
		return false, err
	}
	if !retired {
		// The thread had no items, but it had a title, and the contentless
		// index rows do not cascade with the mapping row that names them.
		if err := deleteThreadSearchThreadTx(tx, threadID); err != nil {
			return false, err
		}
		result, err := tx.Exec(`DELETE FROM threads WHERE id = ?`, threadID)
		if err != nil {
			return false, fmt.Errorf("store: delete empty draft thread %s: %w", threadID, err)
		}
		if err := requireRowsAffected(result, "store: delete empty draft thread "+threadID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit delete empty draft thread %s: %w", threadID, err)
	}
	if reads {
		s.holdersMayBeReleased()
	}
	return true, nil
}
