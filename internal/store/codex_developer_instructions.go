package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// codex_developer_instructions accessors (migration v143): the digest of the
// developer instructions a thread's Codex thread last received. The provider
// compares it with the composed text at a session start and injects the
// text when they differ (internal/provider/codex/developer_instructions.go).
//
// The row is cache content: a lost row costs one repeated injection, never
// a wrong instruction. It names the Codex thread it describes, so a thread
// repointed at another Codex thread (a rollback that forks) reads as having
// no record without any writer having to clear it. A fork child starts with
// none: a cut can drop a message the source received after it.

// CodexDeveloperInstructionsDigest reads the digest recorded for threadID's
// Codex thread sessionRef. The bool is false when nothing is recorded for
// that Codex thread.
func (s *Store) CodexDeveloperInstructionsDigest(threadID, sessionRef string) (string, bool, error) {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" {
		return "", false, nil
	}
	var digest string
	err := s.reader().QueryRow(
		`SELECT digest FROM codex_developer_instructions WHERE thread_id = ? AND session_ref = ?`,
		threadID, sessionRef,
	).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read codex developer instructions digest %s: %w", threadID, err)
	}
	return digest, true, nil
}

// PutCodexDeveloperInstructionsDigest records that threadID's Codex thread
// sessionRef has received the developer instructions whose SHA-256 hex
// digest is digest. A thread deleted meanwhile writes nothing.
func (s *Store) PutCodexDeveloperInstructionsDigest(threadID, sessionRef, digest string) error {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" {
		return fmt.Errorf("store: put codex developer instructions digest: thread %q and session ref %q are both required", threadID, sessionRef)
	}
	if _, err := s.db.Exec(
		`INSERT INTO codex_developer_instructions (thread_id, session_ref, digest)
		 SELECT ?, ?, ? WHERE EXISTS(SELECT 1 FROM threads WHERE id = ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
		     session_ref = excluded.session_ref,
		     digest      = excluded.digest`,
		threadID, sessionRef, digest, threadID,
	); err != nil {
		return fmt.Errorf("store: put codex developer instructions digest %s: %w", threadID, err)
	}
	return nil
}

// DeleteCodexDeveloperInstructionsDigest forgets what threadID's Codex
// thread received, so its next session start injects the composed text. A
// missing row is not an error.
func (s *Store) DeleteCodexDeveloperInstructionsDigest(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM codex_developer_instructions WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: delete codex developer instructions digest %s: %w", threadID, err)
	}
	return nil
}
