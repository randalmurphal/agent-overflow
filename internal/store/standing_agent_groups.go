package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// An assistant's thread and the threads it spawned are never in a sidebar
// group: the agent's row holds its spawned threads the way a group holds
// members (docs/specs/standing-agents.md, "Sidebar"). An assistant's threads
// are created ungrouped, and every group move refuses them, so the sidebar
// never has to pick between the two nestings.

// ErrStandingAgentGrouped is what a move INTO a group reports for a
// standing agent.
var ErrStandingAgentGrouped = errors.New("store: a standing agent is not grouped; its row holds the threads it spawned")

// ErrSpawnedChildGrouped is what a move INTO a group reports for a thread a
// standing agent spawned.
var ErrSpawnedChildGrouped = errors.New("store: a thread a standing agent spawned nests under that agent, not in a group")

// spawnerSQL is the local thread whose spawn request made the named thread,
// the same read threadColumns makes for spawnedByThreadId.
const spawnerSQL = `(SELECT caller_thread_id FROM thread_requests
   WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = ?
   ORDER BY created_at DESC LIMIT 1)`

// refuseAgentGroupingTx fails a move of threadID into a group when the
// thread is a standing agent or a standing agent spawned it.
func refuseAgentGroupingTx(tx *sql.Tx, threadID string) error {
	var agent, child bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?),
		EXISTS(SELECT 1 FROM agents WHERE thread_id = `+spawnerSQL+`)`, threadID, threadID).Scan(&agent, &child)
	switch {
	case err != nil:
		return fmt.Errorf("probe standing agent: %w", err)
	case agent:
		return ErrStandingAgentGrouped
	case child:
		return ErrSpawnedChildGrouped
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

// ListArchivedSpawnedThreads lists the archived threads a thread spawned on
// this computer and, for an assistant's current thread, the assistant's
// archived past threads, newest activity first: the agent row's finished
// work, read when the user opens it. An assistant it created is a row of
// its own, never a child.
func (s *Store) ListArchivedSpawnedThreads(spawnerID string) (threads []Thread, err error) {
	action := fmt.Sprintf("store: list archived threads spawned by %s", spawnerID)
	rows, err := s.reader().Query(`SELECT `+threadColumns+` FROM owned_threads AS threads
		WHERE archived = 1 AND (id IN (
			SELECT target_thread_id FROM thread_requests
			 WHERE caller_thread_id = ?1 AND kind = 'spawn' AND target_computer_id = '')
		OR id IN (
			SELECT agent_past_threads.thread_id FROM agent_past_threads
			  JOIN agents ON agents.id = agent_past_threads.agent_id
			 WHERE agents.thread_id = ?1))
		AND id NOT IN (SELECT thread_id FROM agents)
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
