package store

import (
	"database/sql"
	"encoding/json"
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
// and a delete of the spawner takes the links of those it did not delete,
// leaving an owned one standalone, so the app re-emits them after either.
func (s *Store) ListSpawnedThreadIDs(spawnerID string) ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM owned_threads WHERE archived = 0 AND id IN (
		SELECT r.target_thread_id FROM thread_requests r
		 WHERE r.caller_thread_id = ? AND `+agentSpawnLinkSQL("r")+`)
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

// ownedStepSQL is one step down the owned lineage from the thread the
// column expression owner names: the owned_threads row, as threads, of each
// owned thread whose spawn link that thread made. A thread has one spawn
// link (idx_thread_requests_spawn_link), so its caller is the owner
// (spawnerOfSQL). It is the one owned edge: an archive's walk
// (ListOwnedWorkersToArchive) and a delete's (ListDeleteFamily) take every
// step with it, searching the owner's requests by caller and each worker
// by id. Callers append their own conditions with AND.
func ownedStepSQL(owner string) string {
	return `thread_requests AS link
	    CROSS JOIN owned_threads AS threads ON threads.id = link.target_thread_id
	   WHERE link.caller_thread_id = ` + owner + ` AND ` + agentSpawnLinkSQL("link") + ` AND threads.placement = 'owned'`
}

// ListOwnedWorkersToArchive lists the unarchived threads ownerID owns, at
// any depth through workers already archived (ownedStepSQL). The walk stops
// at each unarchived worker, whose own archive goes on to the threads it
// owns, so an archive of the owner archives each of them, and a repeated
// one reaches a worker an earlier one left open beneath a worker it did
// archive (docs/specs/standing-agents.md, "Owned workers"). One recursive
// read; it visits the archived workers under ownerID.
func (s *Store) ListOwnedWorkersToArchive(ownerID string) (workers []OwnedWorker, err error) {
	action := fmt.Sprintf("store: list the workers %s owns", ownerID)
	rows, err := s.reader().Query(ownedWorkersToArchiveSQL, ownerID)
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

var ownedWorkersToArchiveSQL = `WITH RECURSIVE under(id) AS (
		  SELECT ?
		  UNION
		  SELECT threads.id FROM under CROSS JOIN ` + ownedStepSQL("under.id") + ` AND threads.archived = 1
		)
		SELECT DISTINCT threads.id, under.id FROM under CROSS JOIN ` + ownedStepSQL("under.id") + ` AND threads.archived = 0
		 ORDER BY threads.id`

// DeleteMemberKind is why a delete takes a thread (ListDeleteFamily), in
// the order a thread reached more than one way is counted.
type DeleteMemberKind int

const (
	// DeleteNamed is a thread the delete names.
	DeleteNamed DeleteMemberKind = iota
	// DeletePastThread is a past thread of an assistant whose current
	// thread the delete names.
	DeletePastThread
	// DeleteHelper is a helper of such an assistant.
	DeleteHelper
	// DeleteWorker is an owned thread under any of them.
	DeleteWorker
)

// DeleteMember is one thread a delete takes.
type DeleteMember struct {
	ThreadID string
	// Under is the thread it was reached from: a worker's owner, whose
	// ownership fences the worker's claim (ClaimThreadDelete), or the
	// assistant's current thread of a past thread or helper. Empty for a
	// named thread.
	Under        string
	Kind         DeleteMemberKind
	Depth        int
	ProjectID    string
	WorktreePath string
}

// ListDeleteFamily lists what deleting the named threads takes, each thread
// once: the named threads, for one that is an assistant's current thread its
// past threads and helpers (ruling 39), and the owned workers under all of
// them at any depth, archived or not (ownedStepSQL), each by the shallowest
// way it is reached, in claim order: by depth, then kind, then id, so an
// owner comes before its workers. A thread whose delete began is not reached
// as a past thread, helper or worker: the delete that marked it walks what
// is under it. One recursive read; the delete, its confirmation and boot's
// resume of a delete all read it (docs/specs/standing-agents.md, "Owned
// workers").
func (s *Store) ListDeleteFamily(threadIDs []string) (members []DeleteMember, err error) {
	ids := uniqueNonEmptyStrings(threadIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	named, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("store: encode the threads a delete names: %w", err)
	}
	action := "store: list what a delete takes"
	rows, err := s.reader().Query(deleteFamilySQL, string(named))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	seen := make(map[string]bool)
	for rows.Next() {
		var member DeleteMember
		if err := rows.Scan(&member.ThreadID, &member.Under, &member.Depth, &member.Kind, &member.ProjectID, &member.WorktreePath); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		if seen[member.ThreadID] {
			continue
		}
		seen[member.ThreadID] = true
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return members, nil
}

// deleteFamilySQL is ListDeleteFamily's walk. A thread reached two ways (a
// helper its assistant also spawned) has a row for each, the shallowest
// first; the reader keeps that one.
var deleteFamilySQL = `WITH RECURSIVE
		named(id) AS (SELECT value FROM json_each(?1)),
		family(id, under, depth, kind) AS (
		  SELECT id, '', 0, ` + strconv.Itoa(int(DeleteNamed)) + ` FROM named
		  UNION
		  SELECT past.thread_id, named.id, 0, ` + strconv.Itoa(int(DeletePastThread)) + ` FROM named
		    CROSS JOIN agents ON agents.thread_id = named.id
		    CROSS JOIN agent_past_threads AS past ON past.agent_id = agents.id
		   WHERE EXISTS (SELECT 1 FROM owned_threads WHERE owned_threads.id = past.thread_id)
		  UNION
		  SELECT helper.thread_id, named.id, 0, ` + strconv.Itoa(int(DeleteHelper)) + ` FROM named
		    CROSS JOIN agents ON agents.thread_id = named.id
		    CROSS JOIN agent_helpers AS helper ON helper.agent_id = agents.id
		   WHERE EXISTS (SELECT 1 FROM owned_threads WHERE owned_threads.id = helper.thread_id)
		  UNION
		  SELECT threads.id, family.id, family.depth + 1, ` + strconv.Itoa(int(DeleteWorker)) + `
		    FROM family CROSS JOIN ` + ownedStepSQL("family.id") + `
		)
		SELECT family.id, family.under, family.depth, family.kind,
		       COALESCE(threads.project_id, ''), COALESCE(threads.worktree_path, '')
		  FROM family CROSS JOIN threads ON threads.id = family.id
		 ORDER BY family.depth, family.kind, family.id`

// ErrWorkerNotOwned is a worker's claim, made for its owner's delete
// (ClaimThreadDelete), finding the thread no longer that owner's worker: it
// was given a sidebar row or another owner since the owner's delete read
// it. The thread is left as it is, with everything under it.
var ErrWorkerNotOwned = errors.New("store: the thread is no longer a worker of the thread being deleted")

// ownedBySQL holds for the threads row that owner, an SQL expression, owns:
// owned, with owner its spawner (spawnerOfSQL).
func ownedBySQL(owner string) string {
	return `threads.placement = 'owned' AND ` + spawnerOfSQL("threads.id") + ` = ` + owner
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
