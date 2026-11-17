package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Where the sidebar shows a thread (migration v153,
// docs/specs/standing-agents.md, "Owned workers"). The schema makes a row
// grouped exactly when it has a group, so a group move is the one writer of
// grouped (SetThreadGroup) and this file writes the other two. An owned
// thread belongs to the thread that spawned it: it has no sidebar row
// (catalogThreadClause) and is otherwise an ordinary thread. Owned needs the
// thread's spawn link (spawnLinkSQL): a local spawn, or a paired computer's
// link receipt. A thread outside every project (an assistant's own, past or
// helper thread) takes no placement: a helper is owned from its spawn and
// stays so. A row is inserted standalone or grouped, the write that names a
// spawn's thread makes it owned, and the triggers of v154 and v157 return
// an owned row of a project to standalone when its last link ends.

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
// spawn link reaches it.
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

// spawnLinkSQL is true when ?1 names a thread with a spawn link, the one
// relation that owns it: a local spawn (spawnerOfSQL) or a paired
// computer's link receipt (remoteSpawnLinkSQL).
var spawnLinkSQL = `(` + spawnerOfSQL("?1") + ` IS NOT NULL OR ` + remoteSpawnLinkSQL("?1") + `)`

// ThreadMove is one row a group or placement write moved, as it stands
// after the write, and how the write moved it across the sidebar listing
// (ListingChange), read inside the write's transaction.
type ThreadMove struct {
	Thread  Thread
	Listing ListingChange
}

// ThreadMoveRows returns the rows of moves, in order.
func ThreadMoveRows(moves []ThreadMove) []Thread {
	rows := make([]Thread, 0, len(moves))
	for _, move := range moves {
		rows = append(rows, move.Thread)
	}
	return rows
}

// threadsListedTx reads, inside tx, whether the sidebar lists each id
// (threadListed).
func threadsListedTx(tx *sql.Tx, ids []string) (map[string]bool, error) {
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		yes, err := threadListed(tx, id)
		if err != nil {
			return nil, err
		}
		listed[id] = yes
	}
	return listed, nil
}

// threadMovesTx reads back the rows ids name after a move and pairs each
// with its listing change against before.
func threadMovesTx(tx *sql.Tx, ids []string, before map[string]bool) ([]ThreadMove, error) {
	rows, err := listThreadsByIDTx(tx, ids)
	if err != nil {
		return nil, err
	}
	after, err := threadsListedTx(tx, ids)
	if err != nil {
		return nil, err
	}
	moves := make([]ThreadMove, 0, len(rows))
	for _, row := range rows {
		moves = append(moves, ThreadMove{Thread: row, Listing: listingChange(before[row.ID], after[row.ID])})
	}
	return moves, nil
}

// SetThreadPlacement makes each named thread owned or standalone, out of
// any group it was in, and returns the rows as they now stand with how
// each moved across the sidebar listing. A row whose placement changes
// loses its pin, as a row that joins or leaves a group does: a thread
// starts unpinned in the place it moves to. One transaction; any refusal
// rolls the whole call back.
func (s *Store) SetThreadPlacement(threadIDs []string, placement string) ([]ThreadMove, error) {
	tx, err := s.beginListingWrite()
	if err != nil {
		return nil, fmt.Errorf("store: begin thread placement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := uniqueNonEmptyStrings(threadIDs)
	before, err := threadsListedTx(tx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := setThreadPlacementTx(tx, id, placement); err != nil {
			return nil, err
		}
	}
	moves := []ThreadMove{}
	if len(ids) > 0 {
		if moves, err = threadMovesTx(tx, ids, before); err != nil {
			return nil, fmt.Errorf("store: read back placed threads: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit thread placement: %w", err)
	}
	return moves, nil
}

// standingAgentThreadSQL and outsideProjectsSQL are the threads alias a
// that take no placement: a standing agent's own thread, and one outside
// every project (an assistant's helper). setThreadPlacementTx refuses
// both, and ThreadNames reads no placement for them.
func standingAgentThreadSQL(a string) string {
	return `EXISTS(SELECT 1 FROM agents WHERE thread_id = ` + a + `.id)`
}

func outsideProjectsSQL(a string) string { return a + `.project_id IS NULL` }

func unplaceableThreadSQL(a string) string {
	return `(` + standingAgentThreadSQL(a) + ` OR ` + outsideProjectsSQL(a) + `)`
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
	// A thread a delete claimed goes with that delete, and a sidebar row can
	// no longer keep it.
	if err := checkThreadNotDeleting(tx, threadID); err != nil {
		return false, err
	}
	var agent, outside, linked bool
	var spawner string
	err := tx.QueryRow(`SELECT `+standingAgentThreadSQL("threads")+`, `+outsideProjectsSQL("threads")+`, `+spawnLinkSQL+`,
		COALESCE(`+spawnerOfSQL("?1")+`, '')
		FROM threads WHERE id = ?1`, threadID).Scan(&agent, &outside, &linked, &spawner)
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
	// A thread given back to an owner a delete claimed would join a family
	// that delete already read. A paired computer's owner names no spawner
	// here: its delete reaches the thread through the link.
	if placement == PlacementOwned {
		switch err := checkThreadNotDeleting(tx, spawner); {
		case errors.Is(err, ErrThreadDeleting):
			return false, fmt.Errorf("store: the thread that spawned %s is being deleted: %w", threadID, ErrThreadDeleting)
		case err != nil:
			return false, err
		}
	}
	result, err := tx.Exec(`UPDATE threads
	   SET placement = ?2, group_id = NULL, pinned_at = NULL, pin_group = NULL
	 WHERE id = ?1 AND placement IS NOT ?2`, threadID, placement)
	if err != nil {
		return false, fmt.Errorf("store: set placement of %s: %w", threadID, err)
	}
	return rowsChanged(result, "store: set placement of "+threadID)
}

// ownSpawnedThreadTx makes the thread a spawn just named owned, in the
// naming write's transaction: a local spawn's, or a paired computer's
// through its link. A thread already grouped by the spawn's group stays
// grouped.
func ownSpawnedThreadTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`UPDATE threads SET placement = 'owned'
	 WHERE id = ? AND placement = 'standalone'`, threadID); err != nil {
		return fmt.Errorf("store: own spawned thread %s: %w", threadID, err)
	}
	return nil
}
