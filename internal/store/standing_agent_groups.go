package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// An assistant's own thread is one sidebar row of its own and never in a
// sidebar group (docs/specs/standing-agents.md, "Sidebar"). A thread it
// spawned may be grouped like any other (thread_placement.go).

// ErrStandingAgentGrouped is what a move INTO a group reports for a
// standing agent.
var ErrStandingAgentGrouped = errors.New("store: a standing agent is not grouped; its row is the assistant's own")

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
// computer. Their rows change with the spawn rows that name them: a
// handoff moves those to the assistant's next thread (SpawnedByThreadID),
// and a delete of the spawner leaves an owned one standalone, so the app
// re-emits them after either.
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

// OwnedWorker is an unarchived thread an owner's archive goes on to
// archive, and the thread that owns it, which fences its flip
// (ArchiveOwnedWorker).
type OwnedWorker struct {
	ThreadID string
	OwnerID  string
}

// ListOwnedWorkersToArchive lists the unarchived threads ownerID owns, at
// any depth through workers already archived: a thread owns the owned
// threads whose newest local spawn row names it as caller, the owner
// SpawnedByThreadID reads. The walk stops at each unarchived worker, whose
// own archive goes on to the threads it owns, so an archive of the owner
// archives each of them, and a repeated one reaches a worker an earlier
// one left open beneath a worker it did archive
// (docs/specs/standing-agents.md, "Owned workers"). One recursive read;
// it visits the archived workers under ownerID.
func (s *Store) ListOwnedWorkersToArchive(ownerID string) (workers []OwnedWorker, err error) {
	action := fmt.Sprintf("store: list the workers %s owns", ownerID)
	rows, err := s.reader().Query(`WITH RECURSIVE under(id) AS (
		  SELECT ?
		  UNION
		  SELECT threads.id FROM under
		    JOIN thread_requests AS link ON link.caller_thread_id = under.id
		         AND link.kind = 'spawn' AND link.target_computer_id = ''
		    JOIN owned_threads AS threads ON threads.id = link.target_thread_id
		   WHERE threads.archived = 1 AND `+ownedBySQL("under.id")+`
		)
		SELECT DISTINCT threads.id, under.id FROM under
		  JOIN thread_requests AS link ON link.caller_thread_id = under.id
		       AND link.kind = 'spawn' AND link.target_computer_id = ''
		  JOIN owned_threads AS threads ON threads.id = link.target_thread_id
		 WHERE threads.archived = 0 AND `+ownedBySQL("under.id")+`
		 ORDER BY threads.id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	for rows.Next() {
		var worker OwnedWorker
		if err := rows.Scan(&worker.ThreadID, &worker.OwnerID); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return workers, nil
}

// ownedBySQL holds for the threads row that owner, an SQL expression, owns:
// owned, with its newest local spawn naming owner as caller.
func ownedBySQL(owner string) string {
	return `threads.placement = 'owned'
		  AND (SELECT caller_thread_id FROM thread_requests
		        WHERE kind = 'spawn' AND target_computer_id = '' AND target_thread_id = threads.id
		        ORDER BY created_at DESC LIMIT 1) = ` + owner
}

// ArchiveOwnedWorker archives workerID for its owner's archive, and only
// while that archive stands: ownerID still archived and still the worker's
// owner (ListOwnedWorkersToArchive). A worker the fence excludes, or one already
// archived, is left as it is and reported unchanged; a missing one is
// sql.ErrNoRows.
func (s *Store) ArchiveOwnedWorker(workerID, ownerID string) (Thread, bool, error) {
	write := threadArchivedWrite(workerID, true)
	write.Change += ` AND ` + ownedBySQL("?") + `
		  AND EXISTS (SELECT 1 FROM threads AS owner WHERE owner.id = ? AND owner.archived = 1)`
	write.ChangeArgs = []any{ownerID, ownerID}
	return s.applyThreadRowWrite(write)
}

// MaxPastThreadsPage bounds one page of an assistant's past threads.
const MaxPastThreadsPage = 50

// ErrPastThreadsPage is a past threads page that asks for no rows or too
// many.
var ErrPastThreadsPage = errors.New("store: a past threads page asks for no rows or too many")

// PastThreadsCursor is where a page of past threads starts: after the
// thread it names, in page order. The zero cursor starts at the newest.
type PastThreadsCursor struct {
	SupersededAt int64  `json:"supersededAt"`
	ThreadID     string `json:"threadId"`
}

// PastThreadsPage is one page of an assistant's past threads.
type PastThreadsPage struct {
	// Threads are newest superseded first, ties by thread id descending.
	// Each carries AgentSupersededAt.
	Threads []Thread `json:"threads"`
	// Next continues after the last thread of this page; nil when no
	// thread follows.
	Next *PastThreadsCursor `json:"next,omitempty"`
}

// ListAssistantPastThreads lists one page of the threads a handoff
// superseded for agentID (agent_past_threads), newest superseded first,
// from after the cursor, at most limit (1 to MaxPastThreadsPage) rows. The
// page reads idx_agent_past_threads_agent in its order.
func (s *Store) ListAssistantPastThreads(agentID string, after PastThreadsCursor, limit int) (page PastThreadsPage, err error) {
	if limit < 1 || limit > MaxPastThreadsPage {
		return PastThreadsPage{}, ErrPastThreadsPage
	}
	action := fmt.Sprintf("store: list the past threads of assistant %s", agentID)
	query, args := pastThreadsPageQuery(agentID, after, limit)
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return PastThreadsPage{}, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	page.Threads = []Thread{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return PastThreadsPage{}, fmt.Errorf("%s: scan: %w", action, err)
		}
		page.Threads = append(page.Threads, thread)
	}
	if err := rows.Err(); err != nil {
		return PastThreadsPage{}, fmt.Errorf("%s: %w", action, err)
	}
	if len(page.Threads) > limit {
		page.Threads = page.Threads[:limit]
		last := page.Threads[limit-1]
		page.Next = &PastThreadsCursor{ThreadID: last.ID}
		if last.AgentSupersededAt != nil {
			page.Next.SupersededAt = *last.AgentSupersededAt
		}
	}
	return page, nil
}

// pastThreadsPageQuery is ListAssistantPastThreads' statement, one more row
// than the page to learn whether more follow. A function so the plan test
// explains the production text.
func pastThreadsPageQuery(agentID string, after PastThreadsCursor, limit int) (string, []any) {
	query := `SELECT ` + threadColumns + ` FROM agent_past_threads AS past
		JOIN owned_threads AS threads ON threads.id = past.thread_id
		WHERE past.agent_id = ?1`
	args := []any{agentID}
	if after.ThreadID != "" {
		query += ` AND (past.superseded_at, past.thread_id) < (?2, ?3)`
		args = append(args, after.SupersededAt, after.ThreadID)
	}
	return query + ` ORDER BY past.superseded_at DESC, past.thread_id DESC LIMIT ` + strconv.Itoa(limit+1), args
}
