package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// session_start_receipts accessors (migration v160): the digest of the
// context the memory SessionStart hook last delivered to a thread's model,
// and the provider session that received it. A resume compares it with the
// context the hook would deliver now and runs the hook only when they
// differ (internal/app/app_memory.go).
//
// The row is cache content: a lost row costs one repeated delivery. It
// names the provider session it describes, so a thread repointed at
// another session (a rollback that forks or slices) reads as having
// received nothing, and a fork child starts with none. A compaction drops
// the earlier delivery from the model, so it forgets the row before its own
// delivery is kept.

// SessionStartReceipt reads the digest of what threadID's provider session
// sessionRef received from the hook; "" when nothing is recorded for that
// session.
func (s *Store) SessionStartReceipt(threadID, sessionRef string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" {
		return "", nil
	}
	var digest string
	err := s.reader().QueryRow(
		`SELECT digest FROM session_start_receipts WHERE thread_id = ? AND session_ref = ?`,
		threadID, sessionRef,
	).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read session start receipt %s: %w", threadID, err)
	}
	return digest, nil
}

// PutSessionStartReceipt records that threadID's provider session
// sessionRef received context with digest from the hook. A thread deleted
// meanwhile writes nothing.
func (s *Store) PutSessionStartReceipt(threadID, sessionRef, digest string) error {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" || digest == "" {
		return fmt.Errorf("store: put session start receipt: thread %q, session ref %q and digest %q are all required", threadID, sessionRef, digest)
	}
	if _, err := s.db.Exec(
		`INSERT INTO session_start_receipts (thread_id, session_ref, digest)
		 SELECT ?, ?, ? WHERE EXISTS(SELECT 1 FROM threads WHERE id = ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
		     session_ref = excluded.session_ref,
		     digest      = excluded.digest`,
		threadID, sessionRef, digest, threadID,
	); err != nil {
		return fmt.Errorf("store: put session start receipt %s: %w", threadID, err)
	}
	return nil
}

// ForgetSessionStartReceipt forgets what threadID's model received from the
// hook, so its next resume runs it. Nothing to forget is not an error.
func (s *Store) ForgetSessionStartReceipt(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM session_start_receipts WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: forget session start receipt %s: %w", threadID, err)
	}
	return nil
}
