package store

import (
	"fmt"
	"strconv"
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

// carriedLevelSQL is the level a paired computer's delegation carried to
// the threads alias t that still counts: the stored level
// (threads.spawn_level) while t is owned, through that computer's link,
// or is an ask copy, and none once it has a sidebar row or a group. The
// level is the link's, so the placement that ends a delegation ends it,
// and the placement that restores one restores it.
func carriedLevelSQL(t string) string {
	return `CASE WHEN ` + t + `.placement = '` + PlacementOwned + `' OR ` + t + `.mode = 'scratch' THEN ` + t + `.spawn_level ELSE 0 END`
}

// delegationWalkSQL walks from ?1 up its delegation chain, at most ?2
// links: each row a thread, its distance from ?1 and the level a paired
// computer's delegation carried to it that counts (carriedLevelSQL).
var delegationWalkSQL = `WITH RECURSIVE up(id, level, delegator, carried) AS (
	  SELECT id, 0, ` + delegatorOfSQL("threads") + `, ` + carriedLevelSQL("threads") + ` FROM threads WHERE id = ?1
	  UNION ALL
	  SELECT threads.id, up.level + 1, ` + delegatorOfSQL("threads") + `, ` + carriedLevelSQL("threads") + `
	    FROM up JOIN threads ON threads.id = up.delegator
	   WHERE up.level < ?2)`

// SubThreadLevel is threadID's sub-thread level: the number of delegation
// links between it and the top of its chain on this computer, plus the
// level a paired computer's spawn or ask carried to that top while it
// still counts (carriedLevelSQL: while the thread is owned through that
// computer's link, or is an ask copy). A link is an owned worker's spawn or an agent's ask
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

// lineageWalk bounds the walk up a thread's lineage. A thread with a
// sidebar row may spawn again at level 0, so lineage has no depth limit; the
// bound only keeps the walk finite.
const lineageWalk = 64

// worksForAssistantSQL walks from ?1 up its lineage on this computer: who
// spawned each thread (spawnerOfSQL's link, whatever the thread's
// placement now, and kept as lineage past the retention floor) and who
// asked an agent's ask copy (delegatorOfSQL's ask). It reports whether the
// walk reaches an assistant's current thread or helper.
var worksForAssistantSQL = `WITH RECURSIVE up(id, level) AS (
	  SELECT ?1, 0
	  UNION ALL
	  SELECT link.caller_thread_id, up.level + 1
	    FROM up CROSS JOIN thread_requests AS link
	   WHERE up.level < ` + strconv.Itoa(lineageWalk) + ` AND link.target_thread_id = up.id AND ` + agentSpawnLinkSQL("link") + `
	  UNION ALL
	  SELECT ask.caller_thread_id, up.level + 1
	    FROM up CROSS JOIN scratch_threads AS copy CROSS JOIN thread_requests AS ask
	   WHERE up.level < ` + strconv.Itoa(lineageWalk) + ` AND copy.thread_id = up.id AND ask.token = copy.request_token
	     AND ask.kind = '` + ThreadRequestAsk + `' AND ask.target_computer_id = '')
	SELECT EXISTS (SELECT 1 FROM up
	                WHERE up.id IN (SELECT thread_id FROM agents)
	                   OR up.id IN (SELECT thread_id FROM agent_helpers))`

// WorksForAssistant reports whether threadID works for an assistant on this
// computer: it is an assistant's current thread or helper, or it descends
// from one (worksForAssistantSQL). A thread an assistant spawned, directly
// or through its own workers at any depth, works for it wherever its row is
// drawn: owned, or shown in the sidebar at its spawn or later.
func (s *Store) WorksForAssistant(threadID string) (bool, error) {
	var works bool
	err := s.reader().QueryRow(worksForAssistantSQL, threadID).Scan(&works)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s works for an assistant: %w", threadID, err)
	}
	return works, nil
}
