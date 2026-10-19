package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// codex_session_start_pending accessors (migration v160): the SessionStart
// hook delivery a thread's Codex thread is still expected to make, "start"
// or "compaction". The provider records it as an expectation opens and
// clears it as one settles (internal/provider/codex/session_start_hooks.go).
//
// A lost row costs a missing delivery the next turn reports as a warning; a
// stale one costs one repeated delivery. The row names the Codex thread it
// describes, so a thread repointed at another Codex thread reads as owing
// nothing. A fork child starts with none: its history is the source's at
// the cut.

// CodexSessionStartPending reads the delivery recorded as owed by
// threadID's Codex thread sessionRef, "" when none is.
func (s *Store) CodexSessionStartPending(threadID, sessionRef string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" {
		return "", nil
	}
	var after string
	err := s.reader().QueryRow(
		`SELECT after FROM codex_session_start_pending WHERE thread_id = ? AND session_ref = ?`,
		threadID, sessionRef,
	).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read codex session start pending %s: %w", threadID, err)
	}
	return after, nil
}

// SetCodexSessionStartPending records that threadID's Codex thread
// sessionRef owes the delivery after its "start" or "compaction"; "" clears
// what the thread has recorded. A thread deleted meanwhile writes nothing.
func (s *Store) SetCodexSessionStartPending(threadID, sessionRef, after string) error {
	threadID = strings.TrimSpace(threadID)
	sessionRef = strings.TrimSpace(sessionRef)
	if threadID == "" || sessionRef == "" {
		return fmt.Errorf("store: set codex session start pending: thread %q and session ref %q are both required", threadID, sessionRef)
	}
	if after == "" {
		if _, err := s.db.Exec(`DELETE FROM codex_session_start_pending WHERE thread_id = ?`, threadID); err != nil {
			return fmt.Errorf("store: clear codex session start pending %s: %w", threadID, err)
		}
		return nil
	}
	if _, err := s.db.Exec(
		`INSERT INTO codex_session_start_pending (thread_id, session_ref, after)
		 SELECT ?, ?, ? WHERE EXISTS(SELECT 1 FROM threads WHERE id = ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
		     session_ref = excluded.session_ref,
		     after       = excluded.after`,
		threadID, sessionRef, after, threadID,
	); err != nil {
		return fmt.Errorf("store: set codex session start pending %s: %w", threadID, err)
	}
	return nil
}
