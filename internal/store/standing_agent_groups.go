package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// An assistant's own thread is never in a sidebar group: its row holds the
// threads it spawned the way a group holds members
// (docs/specs/standing-agents.md, "Sidebar"). A thread it spawned may be
// grouped like any other (ruling 36, thread_placement.go).

// ErrStandingAgentGrouped is what a move INTO a group reports for a
// standing agent.
var ErrStandingAgentGrouped = errors.New("store: a standing agent is not grouped; its row holds the threads it spawned")

// refuseAgentGroupingTx fails a move of threadID into a group when the
// thread is a standing agent.
func refuseAgentGroupingTx(tx *sql.Tx, threadID string) error {
	var agent bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?)`, threadID).Scan(&agent); err != nil {
		return fmt.Errorf("probe standing agent: %w", err)
	}
	if agent {
		return ErrStandingAgentGrouped
	}
	return nil
}

// ListSpawnedThreadIDs lists the live threads a thread spawned on this
// computer. Their rows' spawnedByAgent follows whether the spawner is an
// assistant's current thread, so the app re-emits them when that changes
// (creation, a handoff, a delete, a move).
func (s *Store) ListSpawnedThreadIDs(spawnerID string) ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM owned_threads WHERE archived = 0 AND id IN (
		SELECT target_thread_id FROM thread_requests
		 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = '')
		ORDER BY id`, spawnerID)
	if err != nil {
		return nil, fmt.Errorf("store: list threads spawned by %s: %w", spawnerID, err)
	}
	return ids, nil
}

// ListArchivedSpawnedThreads lists the archived threads the done footers of
// an assistant's rows hold (archivedAgentChildrenSQL), newest activity
// first, read when the user opens one; each row's project says which. An
// assistant it created is a row of its own, never a child.
func (s *Store) ListArchivedSpawnedThreads(spawnerID string) (threads []Thread, err error) {
	action := fmt.Sprintf("store: list archived threads spawned by %s", spawnerID)
	rows, err := s.reader().Query(`SELECT `+threadColumns+` FROM owned_threads AS threads
		WHERE id IN (SELECT id FROM (`+archivedAgentChildrenSQL("?1")+`))
		ORDER BY updated_at DESC`, spawnerID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	threads = []Thread{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return threads, nil
}
