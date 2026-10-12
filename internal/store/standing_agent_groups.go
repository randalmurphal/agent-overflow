package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A standing agent and the threads it spawned are never in a sidebar group:
// the agent's row holds its spawned threads the way a group holds members
// (docs/specs/standing-agents.md, "Sidebar agent row"). Promotion takes the
// agent and its spawned threads out of their groups, and every later group
// move refuses them, so the sidebar never has to pick between the two
// nestings.

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
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM standing_agents WHERE thread_id = ?),
		EXISTS(SELECT 1 FROM standing_agents WHERE thread_id = `+spawnerSQL+`)`, threadID, threadID).Scan(&agent, &child)
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

// StandingAgentPromotion is what promoting a thread changed: its agent row,
// whether this call made it, the threads the promotion took out of their
// groups (the agent and the threads it spawned) and the groups that left
// empty, which are deleted.
type StandingAgentPromotion struct {
	Agent    StandingAgent
	Promoted bool
	// Ungrouped holds every row the promotion moved, as it now stands.
	Ungrouped []Thread
	// DeletedGroups are the groups the ungrouping emptied, in no order.
	DeletedGroups []ThreadGroup
}

// PromoteStandingAgent records a promotion and takes the thread and the
// threads it spawned out of their sidebar groups, deleting a group that is
// left empty, in one transaction. Promoting a standing agent again changes
// nothing and returns its row with Promoted false.
func (s *Store) PromoteStandingAgent(threadID, home string, now int64) (promotion StandingAgentPromotion, err error) {
	action := fmt.Sprintf("store: promote %s", threadID)
	tx, err := s.db.Begin()
	if err != nil {
		return StandingAgentPromotion{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	result, err := tx.Exec(`INSERT INTO standing_agents (thread_id, home, promoted_at) VALUES (?, ?, ?)
		ON CONFLICT(thread_id) DO NOTHING`, threadID, home, now)
	if err != nil {
		return StandingAgentPromotion{}, fmt.Errorf("%s: %w", action, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return StandingAgentPromotion{}, fmt.Errorf("%s: rows affected: %w", action, err)
	}
	promotion = StandingAgentPromotion{Promoted: inserted == 1}
	if promotion.Promoted {
		if promotion.Ungrouped, promotion.DeletedGroups, err = ungroupStandingAgentTx(tx, threadID); err != nil {
			return StandingAgentPromotion{}, fmt.Errorf("%s: %w", action, err)
		}
	}
	var notify int
	if err := tx.QueryRow(`SELECT thread_id, home, notify_on_tick, promoted_at FROM standing_agents WHERE thread_id = ?`,
		threadID).Scan(&promotion.Agent.ThreadID, &promotion.Agent.Home, &notify, &promotion.Agent.PromotedAt); err != nil {
		return StandingAgentPromotion{}, fmt.Errorf("%s: read back: %w", action, err)
	}
	promotion.Agent.NotifyOnTick = notify == 1
	if err := tx.Commit(); err != nil {
		return StandingAgentPromotion{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	return promotion, nil
}

// ungroupStandingAgentTx takes the agent and every thread it spawned out of
// their groups, pins included, and deletes each group that left empty.
func ungroupStandingAgentTx(tx *sql.Tx, agentID string) ([]Thread, []ThreadGroup, error) {
	groupIDs, err := queryIDs(tx, `SELECT DISTINCT group_id FROM threads
		WHERE group_id IS NOT NULL AND (id = ? OR id IN (
			SELECT target_thread_id FROM thread_requests
			 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = ''))`, agentID, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("find the groups of the agent and its spawned threads: %w", err)
	}
	moved, err := queryIDs(tx, `UPDATE threads SET group_id = NULL, pinned_at = NULL, pin_group = NULL
		WHERE group_id IS NOT NULL AND (id = ? OR id IN (
			SELECT target_thread_id FROM thread_requests
			 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = ''))
		RETURNING id`, agentID, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("ungroup the agent and its spawned threads: %w", err)
	}
	rows := []Thread{}
	if len(moved) > 0 {
		if rows, err = listThreadsByIDTx(tx, moved); err != nil {
			return nil, nil, fmt.Errorf("read back the ungrouped threads: %w", err)
		}
	}
	deleted := []ThreadGroup{}
	for _, groupID := range groupIDs {
		group, err := scanThreadGroup(tx.QueryRow(`DELETE FROM thread_groups WHERE id = ?
			AND NOT EXISTS (SELECT 1 FROM threads WHERE group_id = thread_groups.id)
			RETURNING `+threadGroupColumns, groupID))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("delete the emptied group %s: %w", groupID, err)
		}
		deleted = append(deleted, group)
	}
	return rows, deleted, nil
}

// ListArchivedSpawnedThreads lists the archived threads a thread spawned on
// this computer, newest activity first: the standing agent row's finished
// work, read when the user opens it.
func (s *Store) ListArchivedSpawnedThreads(spawnerID string) (threads []Thread, err error) {
	action := fmt.Sprintf("store: list archived threads spawned by %s", spawnerID)
	rows, err := s.reader().Query(`SELECT `+threadColumns+` FROM owned_threads AS threads
		WHERE archived = 1 AND id IN (
			SELECT target_thread_id FROM thread_requests
			 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = '')
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
