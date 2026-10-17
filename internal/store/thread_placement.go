package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Where the sidebar shows a thread (migration v159,
// docs/specs/standing-agents.md ruling 36). The schema makes a row grouped
// exactly when it has a group, so a group move is the one writer of
// grouped (SetThreadGroup) and this file writes the other two. Nested needs
// a link that would nest the thread: a local spawn row naming it, or an
// assistant past-thread row. A row created later starts standalone or
// grouped, and the triggers of v159 return a nested row to standalone when
// its last link is deleted.

const (
	PlacementNested     = "nested"
	PlacementGrouped    = "grouped"
	PlacementStandalone = "standalone"
)

// ErrInvalidPlacement is a placement outside the three values.
var ErrInvalidPlacement = errors.New("store: invalid thread placement")

// ErrPlacementNeedsGroup is grouped asked of SetThreadPlacement: a grouped
// row needs its group, which only SetThreadGroup writes.
var ErrPlacementNeedsGroup = errors.New("store: a grouped placement needs a group; move the thread into one")

// ErrThreadNotSpawned is nested asked for a thread nothing would nest under:
// no local spawn row names it and it is no assistant's past thread.
var ErrThreadNotSpawned = errors.New("store: no thread on this computer spawned that thread, so it cannot nest")

// ErrStandingAgentPlaced is a placement asked for an assistant's current
// thread, whose row is an agent row of its own wherever it sits.
var ErrStandingAgentPlaced = errors.New("store: an assistant's own thread is a row of its own and takes no placement")

// insertedPlacement is the placement a new row is written with: grouped
// with its group, else standalone. Nothing is inserted nested, because the
// link nesting needs is written after the row exists.
func insertedPlacement(groupID string) string {
	if groupID != "" {
		return PlacementGrouped
	}
	return PlacementStandalone
}

// nestingLinkSQL is true when ?1 names a thread that a local spawn row
// names or that is an assistant's past thread.
const nestingLinkSQL = `(EXISTS (SELECT 1 FROM thread_requests
       WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = ?1)
   OR EXISTS (SELECT 1 FROM agent_past_threads WHERE agent_past_threads.thread_id = ?1))`

// SetThreadPlacement makes each named thread nested or standalone, out of
// any group it was in, and returns the rows as they now stand. A row whose
// placement changes loses its pin, as a row that joins or leaves a group
// does: a thread starts unpinned in the place it moves to. One transaction;
// any refusal rolls the whole call back.
func (s *Store) SetThreadPlacement(threadIDs []string, placement string) ([]Thread, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin thread placement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := uniqueNonEmptyStrings(threadIDs)
	for _, id := range ids {
		if _, err := setThreadPlacementTx(tx, id, placement); err != nil {
			return nil, err
		}
	}
	rows := []Thread{}
	if len(ids) > 0 {
		if rows, err = listThreadsByIDTx(tx, ids); err != nil {
			return nil, fmt.Errorf("store: read back placed threads: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit thread placement: %w", err)
	}
	return rows, nil
}

// setThreadPlacementTx writes one thread's nested or standalone placement
// in the caller's transaction and reports whether the row changed.
func setThreadPlacementTx(tx *sql.Tx, threadID, placement string) (bool, error) {
	switch placement {
	case PlacementNested, PlacementStandalone:
	case PlacementGrouped:
		return false, ErrPlacementNeedsGroup
	default:
		return false, fmt.Errorf("%w: %q", ErrInvalidPlacement, placement)
	}
	if err := checkThreadTransferAccess(tx, threadID); err != nil {
		return false, err
	}
	var exists, agent, linked bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM threads WHERE id = ?1),
		EXISTS(SELECT 1 FROM agents WHERE thread_id = ?1), `+nestingLinkSQL, threadID).Scan(&exists, &agent, &linked); err != nil {
		return false, fmt.Errorf("store: probe placement of %s: %w", threadID, err)
	}
	switch {
	case !exists:
		return false, ErrThreadGone
	case agent:
		return false, ErrStandingAgentPlaced
	case placement == PlacementNested && !linked:
		return false, ErrThreadNotSpawned
	}
	result, err := tx.Exec(`UPDATE threads
	   SET placement = ?2, group_id = NULL, pinned_at = NULL, pin_group = NULL
	 WHERE id = ?1 AND placement IS NOT ?2`, threadID, placement)
	if err != nil {
		return false, fmt.Errorf("store: set placement of %s: %w", threadID, err)
	}
	return rowsChanged(result, "store: set placement of "+threadID)
}

// nestSpawnedThreadTx makes the thread a local spawn just named nested, in
// the naming write's transaction. A thread already grouped by the spawn's
// group stays grouped.
func nestSpawnedThreadTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`UPDATE threads SET placement = 'nested'
	 WHERE id = ? AND placement = 'standalone'`, threadID); err != nil {
		return fmt.Errorf("store: nest spawned thread %s: %w", threadID, err)
	}
	return nil
}
