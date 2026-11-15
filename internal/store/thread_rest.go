package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ThreadRestCursor continues ListThreadRestRequests after the last row of
// the page before. The zero value starts at the oldest row.
type ThreadRestCursor struct {
	CreatedAt int64
	Token     string
}

// After is the cursor that continues past row.
func (c ThreadRestCursor) After(row ThreadRequest) ThreadRestCursor {
	return ThreadRestCursor{CreatedAt: row.CreatedAt, Token: row.Token}
}

// ListThreadRestRequests returns, oldest first and at most limit at a time,
// the requests callerThreadID made that bringing it to rest acts on: every
// one still open, of any kind, and every spawn it made whatever its state,
// a lineage-only row included, since the thread a spawn made may still be
// running long after the spawn settled. A local spawn another spawn of the
// same thread superseded is not this caller's link to it.
func (s *Store) ListThreadRestRequests(callerThreadID string, after ThreadRestCursor, limit int) ([]ThreadRequest, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: list the requests a rest of %s stops: limit %d", callerThreadID, limit)
	}
	afterAt := after.CreatedAt
	if after == (ThreadRestCursor{}) {
		afterAt = math.MinInt64
	}
	rows, err := s.reader().Query(
		`SELECT `+threadRequestColumns+` FROM thread_requests
		  WHERE caller_thread_id = ?
		    AND ((lineage_only = 0 AND settled_at IS NULL)
		      OR (kind = 'spawn' AND target_thread_id <> '' AND (target_computer_id <> '' OR superseded = 0)))
		    AND (created_at > ? OR (created_at = ? AND token > ?))
		  ORDER BY created_at, token LIMIT `+strconv.Itoa(limit),
		callerThreadID, afterAt, afterAt, after.Token)
	if err != nil {
		return nil, fmt.Errorf("store: list the requests a rest of %s stops: %w", callerThreadID, err)
	}
	return collectThreadRequests(rows, "store: list the requests a rest of "+callerThreadID+" stops")
}

// CallerSpawnsOf returns callerThreadID's spawns of threadID on computerID,
// "" for this computer, which a thread_cancel by thread id brings to rest.
// Here that is the spawn link (agentSpawnLinkSQL), a link the retention
// floor reduced to lineage included and a superseded spawn excluded; on a
// paired computer it is every spawn row the caller still holds for it.
func (s *Store) CallerSpawnsOf(callerThreadID, computerID, threadID string) ([]ThreadRequest, error) {
	link := agentSpawnLinkSQL("thread_requests")
	args := []any{callerThreadID, threadID}
	if computerID != "" {
		link = `kind = 'spawn' AND target_computer_id = ` + boundText
		args = append(args, computerID)
	}
	rows, err := s.reader().Query(`SELECT `+threadRequestColumns+` FROM thread_requests
		WHERE caller_thread_id = ? AND target_thread_id = ? AND `+link, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read the spawns of %s by %s: %w", threadID, callerThreadID, err)
	}
	return collectThreadRequests(rows, "store: read the spawns of "+threadID+" by "+callerThreadID)
}

// CallerRequestedOf reports whether callerThreadID ever sent to, asked or
// spawned threadID, settled or not: a request naming it as its target or,
// for an ask, as the thread its copy was made from.
func (s *Store) CallerRequestedOf(callerThreadID, threadID string) (bool, error) {
	var linked bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_requests
		WHERE caller_thread_id = ?1 AND (target_thread_id = ?2 OR origin_thread_id = ?2))`,
		callerThreadID, threadID).Scan(&linked)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s made a request of %s: %w", callerThreadID, threadID, err)
	}
	return linked, nil
}

// ListThreadStartedWorkItems returns, oldest first, the runs threadID
// started that have not ended: bound to it, which only a root run can be,
// and recorded with source and no source ref, as a run an interactive thread
// asks for is. A run a person bound to the thread carries another source and
// is not listed.
func (s *Store) ListThreadStartedWorkItems(threadID, source string) ([]WorkItem, error) {
	return s.queryWorkItems(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE origin_thread_id = `+boundText+` AND origin_thread_id <> ''
		   AND source = `+boundText+` AND source_ref = ''
		   AND state NOT IN ('done','failed','cancelled')
		 ORDER BY created_at ASC, id ASC`,
		"list the runs thread "+threadID+" started", threadID, source,
	)
}

// ThreadRunningWorkflowRun returns the id of a running run threadID works in
// as one of its phases or fan-out units, or "" when it works in none.
func (s *Store) ThreadRunningWorkflowRun(threadID string) (string, error) {
	var itemID string
	err := s.reader().QueryRow(`SELECT id FROM work_items
		WHERE state = 'running'
		  AND (id IN (SELECT item_id FROM work_item_phases WHERE thread_id = ?1)
		       OR id IN (SELECT item_id FROM work_item_units WHERE thread_id = ?1))
		ORDER BY created_at ASC, id ASC LIMIT 1`, threadID).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read the running run thread %s works in: %w", threadID, err)
	}
	return itemID, nil
}
