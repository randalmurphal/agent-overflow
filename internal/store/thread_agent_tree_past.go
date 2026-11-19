package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// MaxPastAgentRowsPage bounds one page of ListThreadPastAgentRequests.
const MaxPastAgentRowsPage = 50

// ErrPastAgentRowsPage is a page size outside 1 to MaxPastAgentRowsPage.
var ErrPastAgentRowsPage = fmt.Errorf("store: a page of past agent rows holds 1 to %d rows", MaxPastAgentRowsPage)

// PastAgentRowsCursor is where a page of past agent rows starts: after the
// row it names, in page order. The zero cursor starts at the newest.
type PastAgentRowsCursor struct {
	SettledAt int64  `json:"settledAt"`
	Token     string `json:"token"`
}

// PastAgentRequests is one page of a thread's settled agent rows.
type PastAgentRequests struct {
	Requests []ThreadAgentRequest
	// Total is how many settled rows the thread has, this page's included.
	Total int
	// Next starts the following page; nil on the last.
	Next *PastAgentRowsCursor
}

// pastAgentRowSQL is the request kinds a thread's past agent rows hold:
// every spawn and ask, and a send whose wake was armed, less the spawns
// whose thread is not at rest (agentTargetOpenSQL, with ?2 the held work).
// A lineage row the retention sweep kept is one of them, with what the
// sweep cleared gone.
var pastAgentRowSQL = `r.caller_thread_id = ?1 AND r.settled_at IS NOT NULL
	   AND (r.kind IN ('spawn','ask') OR (r.kind = 'send' AND (r.notify = 1 OR r.run_base_tool_calls IS NOT NULL)))
	   AND NOT ` + agentTargetOpenSQL("r")

// ListThreadPastAgentRequests lists one page of the requests callerThreadID
// made that its Agents segment lists as past: settled, newest settled
// first, ties by token descending, from after the cursor, at most limit (1
// to MaxPastAgentRowsPage), with the first answerHead bytes of each answer
// and the count of them all. A settled row whose run is open on a thread
// not at rest is still open, and left out: waiting and live are the held work, as
// ThreadAgentTreeRead's. The page and the count are one read, through
// idx_thread_requests_caller_settled, so the page's cost follows its rows;
// the count's follows the caller's settled history, which the retention
// floor bounds to a month of requests and the lineage of the spawns past
// it.
func (s *Store) ListThreadPastAgentRequests(callerThreadID string, after PastAgentRowsCursor, limit, answerHead int, waiting, live []string) (page PastAgentRequests, err error) {
	if limit < 1 || limit > MaxPastAgentRowsPage {
		return PastAgentRequests{}, ErrPastAgentRowsPage
	}
	action := "store: list the past agents of " + callerThreadID
	if answerHead < 0 {
		return PastAgentRequests{}, fmt.Errorf("%s: answer head %d", action, answerHead)
	}
	settledBefore, tokenBefore := after.SettledAt, after.Token
	if after == (PastAgentRowsCursor{}) {
		settledBefore = math.MaxInt64
	}
	held, err := agentHeldParam(waiting, live)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: %w", action, err)
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	if err := tx.QueryRow(`WITH RECURSIVE `+agentHeldSQL+` SELECT count(*) FROM thread_requests r WHERE `+pastAgentRowSQL, callerThreadID, held).Scan(&page.Total); err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: count: %w", action, err)
	}
	page.Requests = []ThreadAgentRequest{}
	if page.Total == 0 {
		return page, nil
	}
	rows, err := tx.Query(pastAgentRowsSQL(limit+1), callerThreadID, held, settledBefore, tokenBefore, answerHead)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var row ThreadAgentRequest
		var late int
		row.ThreadRequest, err = scanThreadRequestWith(rows, &row.AnswerHead, &late, &row.WakeItemID)
		if err != nil {
			return PastAgentRequests{}, fmt.Errorf("%s: scan: %w", action, err)
		}
		row.HasLateReply = late != 0
		page.Requests = append(page.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: iterate: %w", action, err)
	}
	if len(page.Requests) > limit {
		page.Requests = page.Requests[:limit]
		last := page.Requests[limit-1]
		page.Next = &PastAgentRowsCursor{SettledAt: last.SettledAt, Token: last.Token}
	}
	return page, nil
}

// pastAgentRowsSQL reads a page of past agent rows: ?1 is the caller, ?2
// the held work, ?3 and ?4 the cursor's settled time and token, ?5 the
// answer head (where the tree reads put it).
func pastAgentRowsSQL(limit int) string {
	return `WITH RECURSIVE ` + agentHeldSQL + `
SELECT ` + threadAgentRequestColumns + `
  FROM thread_requests r INDEXED BY idx_thread_requests_caller_settled
 WHERE ` + pastAgentRowSQL + ` AND (r.settled_at, r.token) < (?3, ?4)
 ORDER BY r.settled_at DESC, r.token DESC LIMIT ` + strconv.Itoa(limit)
}
