package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ThreadAgentStopRead is one read of what a Stop of every Agents row of
// Root acts on, with ThreadAgentTreeRead's Root, Waiting, Live and
// SettledSince.
type ThreadAgentStopRead struct {
	Root         string
	Waiting      []string
	Live         []string
	SettledSince int64
	// Page bounds the rows each statement reads.
	Page int
}

// ListThreadAgentStopRows returns the requests a Stop of every Agents row
// of Root acts on: the rows ListThreadAgentTree lists at Root's own level,
// less a send or ask that already settled, which has nothing left to stop.
// A settled spawn stays, since its Stop reaches into the thread it made.
// Oldest first, each without its answer. The rows are read Page at a time,
// each page through the listing's arms from where the last one ended, all
// inside one read transaction: they are the ledger as the first page found
// it, and a request made meanwhile is in none of them. The tray's limit
// does not apply: every such row is returned.
func (s *Store) ListThreadAgentStopRows(read ThreadAgentStopRead) (out []ThreadRequest, err error) {
	action := "store: list what a stop of the agents of " + read.Root + " stops"
	if read.Page < 1 {
		return nil, fmt.Errorf("%s: page %d", action, read.Page)
	}
	held, err := agentHeldParam(read.Waiting, read.Live)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	query := threadAgentStopRowsSQL(read.Page)
	out = []ThreadRequest{}
	afterAt, afterToken := int64(math.MinInt64), ""
	for {
		page, err := s.threadAgentStopPage(tx, query, read, held, afterAt, afterToken)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		out = append(out, page...)
		if len(page) < read.Page {
			return out, nil
		}
		last := page[len(page)-1]
		afterAt, afterToken = last.CreatedAt, last.Token
		if hook := s.agentStopPageReadForTest; hook != nil {
			hook()
		}
	}
}

func (s *Store) threadAgentStopPage(tx *sql.Tx, query string, read ThreadAgentStopRead, held string, afterAt int64, afterToken string) (_ []ThreadRequest, err error) {
	rows, err := tx.Query(query, read.Root, held, read.SettledSince, nil, 0, afterAt, afterToken)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	page := []ThreadRequest{}
	for rows.Next() {
		var head []byte
		var late int
		var wake string
		row, err := scanThreadRequestWith(rows, &head, &late, &wake)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return page, nil
}

// threadAgentStopRowsSQL reads one page of ListThreadAgentStopRows: the
// first limit rows after the cursor (?6, ?7) in (created_at, token) order
// from each arm of agentRowArms, read through the arm's index, with the
// parameters of threadAgentTreeSQL (?4 unused, ?5 an answer head of 0).
// The open arm's index is in that order and seeks to the cursor. The other
// arms hold few rows, and their cursor term is kept off every index: on
// idx_thread_requests_caller it would read the caller's history from the
// cursor on (TestThreadAgentTreeSearchesItsArms).
func threadAgentStopRowsSQL(limit int) string {
	n := strconv.Itoa(limit)
	var arms []string
	for _, arm := range agentRowArms("a", agentRootSQL) {
		cursor := `(+a.created_at, +a.token) > (?6, ?7)`
		if arm.index == "idx_thread_requests_open_created" {
			cursor = `(a.created_at, a.token) > (?6, ?7)`
		}
		arms = append(arms, `SELECT token FROM (SELECT a.token FROM `+arm.from("a")+`
        WHERE `+arm.where+` AND `+agentRowKindSQL("a")+`
          AND (a.settled_at IS NULL OR (a.kind = 'spawn' AND `+ownRunOpenSQL("a")+`) OR (a.kind = 'send' AND `+runOpenSQL("a")+`))
          AND `+cursor+`
        ORDER BY a.created_at, a.token LIMIT `+n+`)`)
	}
	return `WITH RECURSIVE
  ` + agentHeldSQL + `
SELECT ` + threadAgentRequestColumns + `
  FROM thread_requests r
 WHERE r.token IN (` + strings.Join(arms, `
    UNION ALL
    `) + `)
 ORDER BY r.created_at, r.token LIMIT ` + n
}
