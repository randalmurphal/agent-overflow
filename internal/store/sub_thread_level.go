package store

import (
	"database/sql"
	"fmt"
)

// subThreadLevelWalk bounds the walk up a delegation chain. The setting
// caps a level at four, so any chain this long is past every limit; the
// bound only has to stop a cycle and keep a chain of asks finite.
const subThreadLevelWalk = 16

// delegatorOfSQL is the thread whose delegation the threads alias t
// continues on this computer: the spawner of an owned worker
// (spawnerOfSQL), and the asker of an agent's ask copy, by the ask request
// its scratch record names. NULL for every other thread: one the person
// talks to, one in the sidebar, an assistant's own thread and a person's
// side chat, whose scratch record names no request.
func delegatorOfSQL(t string) string {
	return `CASE WHEN ` + t + `.placement = 'owned' THEN ` + spawnerOfSQL(t+".id") + `
	        ELSE (SELECT ask.caller_thread_id FROM scratch_threads AS copy
	                JOIN thread_requests AS ask ON ask.token = copy.request_token
	               WHERE copy.thread_id = ` + t + `.id AND ask.kind = '` + ThreadRequestAsk + `'
	                 AND ask.target_computer_id = '') END`
}

// delegationWalkSQL walks from ?1 up its delegation chain, at most ?2
// links: each row a thread, its distance from ?1 and the level a paired
// computer's delegation carried to it (threads.spawn_level).
var delegationWalkSQL = `WITH RECURSIVE up(id, level, delegator, carried) AS (
	  SELECT id, 0, ` + delegatorOfSQL("threads") + `, spawn_level FROM threads WHERE id = ?1
	  UNION ALL
	  SELECT threads.id, up.level + 1, ` + delegatorOfSQL("threads") + `, threads.spawn_level
	    FROM up JOIN threads ON threads.id = up.delegator
	   WHERE up.level < ?2)`

// SubThreadLevel is threadID's sub-thread level: the number of delegation
// links between it and the top of its chain on this computer, plus the
// level a paired computer's spawn or ask carried to that top
// (threads.spawn_level, which ends where that delegation ends:
// endCarriedLevelTx). A link is an owned worker's spawn or an agent's ask
// copy's ask (delegatorOfSQL). A thread the person talks to, one in the
// sidebar, an assistant's own thread and a person's side chat are level 0;
// a helper is level 1 under its assistant, as any owned worker is under
// its spawner, and an ask copy one below the thread that asked
// (docs/specs/agent-thread-tools.md, "Sub-thread depth"). A thread that is
// not here is sql.ErrNoRows.
func (s *Store) SubThreadLevel(threadID string) (int, error) {
	var level int
	err := s.reader().QueryRow(delegationWalkSQL+`
		SELECT level + carried FROM up ORDER BY level DESC LIMIT 1`,
		threadID, subThreadLevelWalk).Scan(&level)
	if err != nil {
		return 0, fmt.Errorf("store: sub-thread level of %s: %w", threadID, err)
	}
	return level, nil
}

// DelegationChain is threadID and every thread above it on its delegation
// chain on this computer, nearest first (SubThreadLevel's walk). A thread
// that is not here has none.
func (s *Store) DelegationChain(threadID string) ([]string, error) {
	rows, err := s.reader().Query(delegationWalkSQL+`
		SELECT id FROM up ORDER BY level`, threadID, subThreadLevelWalk)
	if err != nil {
		return nil, fmt.Errorf("store: delegation chain of %s: %w", threadID, err)
	}
	// A read's close fails only with an error rows.Err reports.
	defer func() { _ = rows.Close() }()
	var chain []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: delegation chain of %s: %w", threadID, err)
		}
		chain = append(chain, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: delegation chain of %s: %w", threadID, err)
	}
	return chain, nil
}

// endCarriedLevelTx ends the delegation a paired computer's spawn or ask
// carried to threadID: placing the thread in the sidebar, in a group or
// standalone, and keeping an ask copy make it a thread of its own here,
// as the same moves do to a local worker, whose link stops counting when it
// is no longer owned. Nothing restores the carried level: putting a thread
// back under ownership needs a local spawn link, and that link is what
// counts then.
func endCarriedLevelTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(`UPDATE threads SET spawn_level = 0 WHERE id = ? AND spawn_level <> 0`, threadID); err != nil {
		return fmt.Errorf("store: end the carried level of %s: %w", threadID, err)
	}
	return nil
}
