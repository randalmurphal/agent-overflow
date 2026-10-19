package store

import (
	"fmt"
	"strings"
)

// session_start_receipts accessors (migration v160): the digest of the
// context each SessionStart hook last delivered to a thread's model, and
// the provider session that received it. A resume compares it with the
// context the hook would deliver now and runs only the hooks that differ
// (internal/app/app_standing_agents.go).
//
// The rows are cache content: a lost row costs one repeated delivery. A row
// names the provider session it describes, so a thread repointed at another
// session (a rollback that forks or slices) reads as having received
// nothing, and a fork child starts with none. A compaction drops every
// earlier delivery from the model, so it forgets the thread's rows before
// its own delivery is kept.

// SessionStartReceipts reads, by hook, the digest of what threadID's
// provider session sessionRef received; empty when nothing is recorded for
// that session.
func (s *Store) SessionStartReceipts(threadID, sessionRef string) (map[string]string, error) {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	receipts := map[string]string{}
	if threadID == "" || sessionRef == "" {
		return receipts, nil
	}
	rows, err := s.reader().Query(
		`SELECT hook, digest FROM session_start_receipts WHERE thread_id = ? AND session_ref = ?`,
		threadID, sessionRef,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read session start receipts %s: %w", threadID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var hook, digest string
		if err := rows.Scan(&hook, &digest); err != nil {
			return nil, fmt.Errorf("store: read session start receipts %s: %w", threadID, err)
		}
		receipts[hook] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read session start receipts %s: %w", threadID, err)
	}
	return receipts, nil
}

// PutSessionStartReceipt records that threadID's provider session
// sessionRef received context with digest from hook. A thread deleted
// meanwhile writes nothing.
func (s *Store) PutSessionStartReceipt(threadID, sessionRef, hook, digest string) error {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" || hook == "" || digest == "" {
		return fmt.Errorf("store: put session start receipt: thread %q, session ref %q, hook %q and digest %q are all required", threadID, sessionRef, hook, digest)
	}
	if _, err := s.db.Exec(
		`INSERT INTO session_start_receipts (thread_id, hook, session_ref, digest)
		 SELECT ?, ?, ?, ? WHERE EXISTS(SELECT 1 FROM threads WHERE id = ?)
		 ON CONFLICT(thread_id, hook) DO UPDATE SET
		     session_ref = excluded.session_ref,
		     digest      = excluded.digest`,
		threadID, hook, sessionRef, digest, threadID,
	); err != nil {
		return fmt.Errorf("store: put session start receipt %s %s: %w", threadID, hook, err)
	}
	return nil
}

// ForgetSessionStartReceipts forgets what threadID's model received from
// its SessionStart hooks, so its next resume runs every hook. Nothing to
// forget is not an error.
func (s *Store) ForgetSessionStartReceipts(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM session_start_receipts WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: forget session start receipts %s: %w", threadID, err)
	}
	return nil
}
