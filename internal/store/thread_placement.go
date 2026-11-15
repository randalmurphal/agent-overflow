package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Where the sidebar shows a thread (migration v150,
// docs/specs/standing-agents.md, "Owned workers"). The schema makes a row
// grouped exactly when it has a group, so a group move is the one writer of
// grouped (SetThreadGroup) and this file writes the other two. An owned
// thread belongs to the thread that spawned it: it has no sidebar row
// (catalogThreadClause) and is otherwise an ordinary thread. Owned needs a
// local spawn row naming the thread. A thread outside every project (an
// assistant's own, past or helper thread) takes no placement: a helper is
// owned from its spawn and stays so. A row is inserted standalone or
// grouped, the write that names a local spawn's thread makes it owned, and
// the trigger of v151 returns an owned row of a project to standalone when
// its last link is deleted.

const (
	PlacementOwned      = "owned"
	PlacementGrouped    = "grouped"
	PlacementStandalone = "standalone"
)

// ErrInvalidPlacement is a placement outside the three values.
var ErrInvalidPlacement = errors.New("store: invalid thread placement")

// ErrPlacementNeedsGroup is grouped asked of SetThreadPlacement: a grouped
// row needs its group, which only SetThreadGroup writes.
var ErrPlacementNeedsGroup = errors.New("store: a grouped placement needs a group; move the thread into one")

// ErrThreadNotSpawned is owned asked for a thread nothing would own: no
// local spawn row names it.
var ErrThreadNotSpawned = errors.New("store: no thread on this computer spawned that thread, so no thread can own it")

// ErrStandingAgentPlaced is a placement asked for an assistant's current
// thread, whose row is an assistant row of its own.
var ErrStandingAgentPlaced = errors.New("store: an assistant's own thread is a row of its own and takes no placement")

// ErrPlacementOutsideProjects is a placement asked for a thread outside
// every project, an assistant's helper or past thread: a helper is always
// owned by its assistant, and a past thread is reached from its assistant.
var ErrPlacementOutsideProjects = errors.New("store: a thread outside every project belongs to its assistant and takes no placement")

// insertedPlacement is the placement a new row is written with: grouped
// with its group, else standalone. Nothing is inserted owned, because the
// link ownership needs is written after the row exists.
func insertedPlacement(groupID string) string {
	if groupID != "" {
		return PlacementGrouped
	}
	return PlacementStandalone
}

// spawnLinkSQL is true when ?1 names a thread that a local spawn row
// names.
const spawnLinkSQL = `EXISTS (SELECT 1 FROM thread_requests
       WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = ?1)`

// SetThreadPlacement makes each named thread owned or standalone, out of
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

// setThreadPlacementTx writes one thread's owned or standalone placement
// in the caller's transaction and reports whether the row changed.
func setThreadPlacementTx(tx *sql.Tx, threadID, placement string) (bool, error) {
	switch placement {
	case PlacementOwned, PlacementStandalone:
	case PlacementGrouped:
		return false, ErrPlacementNeedsGroup
	default:
		return false, fmt.Errorf("%w: %q", ErrInvalidPlacement, placement)
	}
	if err := checkThreadTransferAccess(tx, threadID); err != nil {
		return false, err
	}
	var agent, outside, linked bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?1), project_id IS NULL, `+spawnLinkSQL+`
		FROM threads WHERE id = ?1`, threadID).Scan(&agent, &outside, &linked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrThreadGone
	}
	if err != nil {
		return false, fmt.Errorf("store: probe placement of %s: %w", threadID, err)
	}
	switch {
	case agent:
		return false, ErrStandingAgentPlaced
	case outside:
		return false, ErrPlacementOutsideProjects
	case placement == PlacementOwned && !linked:
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

// ownSpawnedThreadTx makes the thread a local spawn just named owned, in
// the naming write's transaction. A thread already grouped by the spawn's
// group stays grouped.
func ownSpawnedThreadTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`UPDATE threads SET placement = 'owned'
	 WHERE id = ? AND placement = 'standalone'`, threadID); err != nil {
		return fmt.Errorf("store: own spawned thread %s: %w", threadID, err)
	}
	return nil
}
