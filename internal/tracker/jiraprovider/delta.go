package jiraprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// The delta pages a board's issues by update time with keyset paging: a
// cursor is the last row delivered, by update time in milliseconds and
// issue ID, and each page asks Jira for the rows after that pair, ordered
// by update time then ID (deltaJQL). JQL takes the time as a bare number
// of milliseconds since the epoch, compared at millisecond precision with
// no time zone involved, and orders by update time at that precision.
// That is measured on Jira Cloud's search/jql, not documented, and
// assumed of the board issue listing, which takes the same JQL. Jira's
// page tokens are never used, so nothing a cursor holds expires, and a
// result set that shifts between pages skips nothing.
//
// A walk is the pages from one with no position inside a walk to the
// first that says no more follow. Its first page records walkStart, the
// Date header of Jira's answer to it: Jira's own clock, so this
// computer's has no part. At the end of a walk that delivered a row, on
// its last page or an earlier one, tracker.DeltaResume decides where the
// next walk starts from the newest row delivered and walkStart: exactly
// after that row when every update it could have missed was visible to
// the whole walk, so a board nobody changed delivers nothing; otherwise at
// the time it returns, with no ID, so the next walk delivers again every
// row updated since. Jira documents that its search may not yet show a
// recent update: an issue whose update the index took only after the walk
// passed its time is read then. A walk that delivered no row keeps its
// cursor, so an idle cursor does not drift back.

const (
	// cursorVersion is the version of the cursor's encoding.
	cursorVersion = 3
	// deltaPage is the most issues one delta page asks for. The board
	// issue list is not clamped the way search is: 500 issues with ten
	// card fields came back in 0.99 s and 1.27 MB against 0.50 s and
	// 214 KB for 100 (docs/references/tracker-api-measurements.md), so a
	// first walk takes a fifth of the requests for about twice the time
	// per page.
	deltaPage = 500
)

// cursor is a delta's position on one board. AfterMS is the update time
// of the last row delivered, in milliseconds since the epoch, and AfterID
// its issue ID: the walk resumes with the rows after that pair. AfterID
// zero delivers every row updated at or after AfterMS. InWalk is set
// between the pages of a walk: StartMS is then walkStart in milliseconds,
// zero when Jira sent no Date, and Delivered reports a row delivered so
// far.
type cursor struct {
	V         int    `json:"v"`
	Board     string `json:"board"`
	AfterMS   int64  `json:"after_ms"`
	AfterID   int64  `json:"after_id,omitempty"`
	InWalk    bool   `json:"in_walk,omitempty"`
	StartMS   int64  `json:"start_ms,omitempty"`
	Delivered bool   `json:"delivered,omitempty"`
}

// readCursor decodes q's cursor, or starts a walk at q.Start's
// millisecond. A cursor that is not one this package wrote for q.Board is
// refused.
func readCursor(q tracker.DeltaQuery) (cursor, error) {
	if q.From == nil {
		if q.Start.IsZero() || q.Start.UnixMilli() <= 0 {
			return cursor{}, invalidf("a delta of board %s needs a cursor or a start after 1970", q.Board)
		}
		return cursor{V: cursorVersion, Board: string(q.Board), AfterMS: q.Start.UnixMilli()}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(q.From))
	dec.DisallowUnknownFields()
	var c cursor
	if err := dec.Decode(&c); err != nil || dec.More() {
		return cursor{}, invalidf("the cursor of board %s is not a Jira delta cursor", q.Board)
	}
	switch {
	case c.V != cursorVersion:
		return cursor{}, invalidf("the cursor of board %s has version %d, not %d", q.Board, c.V, cursorVersion)
	case c.Board != string(q.Board):
		return cursor{}, invalidf("the cursor is board %q's, not board %s's", c.Board, q.Board)
	case c.AfterMS <= 0 || c.AfterID < 0 || c.StartMS < 0:
		return cursor{}, invalidf("the cursor of board %s names no position", q.Board)
	case !c.InWalk && (c.StartMS != 0 || c.Delivered):
		return cursor{}, invalidf("the cursor of board %s holds a walk it is not in", q.Board)
	}
	return c, nil
}

// end is the cursor the next walk starts from, once the walk c is in
// ends.
func (c cursor) end() cursor {
	next := cursor{V: c.V, Board: c.Board, AfterMS: c.AfterMS, AfterID: c.AfterID}
	if !c.Delivered {
		return next
	}
	var walkStart time.Time
	if c.StartMS > 0 {
		walkStart = time.UnixMilli(c.StartMS)
	}
	restart, exact := tracker.DeltaResume(time.UnixMilli(c.AfterMS), walkStart)
	if !exact {
		next.AfterMS, next.AfterID = max(restart.UnixMilli(), 1), 0
	}
	return next
}

func (c cursor) bytes() tracker.Cursor {
	raw, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("jiraprovider: encode a cursor: %v", err))
	}
	return raw
}

// deltaJQL is the JQL of the page after c. A walk's first page is
//
//	updated >= <ms> ORDER BY updated ASC, id ASC
//
// and every later one
//
//	(updated > <ms> OR (updated >= <ms> AND id > <id>)) ORDER BY updated ASC, id ASC
//
// which is the rows updated after the last one delivered, or at the same
// millisecond with a greater ID: none of a run of same-millisecond rows
// is skipped or repeated, however many pages the run spans. It uses only
// the operators measured at millisecond precision (> and >=) and needs no
// equality on a time.
func (c cursor) deltaJQL() string {
	ms := strconv.FormatInt(c.AfterMS, 10)
	where := "updated >= " + ms
	if c.AfterID > 0 {
		where = "(updated > " + ms + " OR (updated >= " + ms + " AND id > " + decimal(c.AfterID) + "))"
	}
	return where + " ORDER BY updated ASC, id ASC"
}

// ChangedRows is the next page of board's rows updated since q's cursor,
// oldest update first.
func (p *Provider) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	cur, err := readCursor(q)
	if err != nil {
		return tracker.RowPage{}, err
	}
	b, err := p.board(ctx, q.Board)
	if err != nil {
		return tracker.RowPage{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.RowPage{}, err
	}
	page, err := p.jira.ListIssues(ctx, jira.ListQuery{
		List: jira.BoardIssues(b.cfg.BoardID), JQL: cur.deltaJQL(), Fields: b.rowFields(s), MaxResults: deltaPage,
	})
	if err != nil {
		return tracker.RowPage{}, fmt.Errorf("jiraprovider: changes of board %s: %w", q.Board, err)
	}
	if len(page.Warnings) > 0 {
		return tracker.RowPage{}, fmt.Errorf("jiraprovider: changes of board %s: Jira warned %q, so rows may be missing", q.Board, page.Warnings[0].Message)
	}
	cat := newCatalog()
	var out tracker.RowPage
	next := cur
	if !next.InWalk {
		next.InWalk, next.StartMS = true, 0
		if !page.Date.IsZero() {
			next.StartMS = max(page.Date.UnixMilli(), 0)
		}
	}
	for _, is := range page.Issues {
		row, err := b.rowOf(s, is, cat)
		if err != nil {
			return tracker.RowPage{}, err
		}
		id, err := numericID("issue", is.ID)
		if err != nil {
			return tracker.RowPage{}, fmt.Errorf("jiraprovider: changes of board %s: %w", q.Board, err)
		}
		at := row.Card.Updated.UnixMilli()
		if at < next.AfterMS || at == next.AfterMS && id <= next.AfterID {
			return tracker.RowPage{}, fmt.Errorf("jiraprovider: changes of board %s: Jira returned issue %s out of update order", q.Board, is.Key)
		}
		next.AfterMS, next.AfterID, next.Delivered = at, id, true
		out.Rows = append(out.Rows, row)
	}
	// A page with no rows ends the walk whatever its token says: the next
	// page would ask the same.
	out.More = page.NextPageToken != "" && len(out.Rows) > 0
	if !out.More {
		next = next.end()
	}
	out.Catalog = cat.out
	out.Next = next.bytes()
	return out, nil
}
