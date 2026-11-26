package linear

import (
	"context"
	"time"
)

// PollCursor is where a poll resumes: a Position in the oldest-first
// update order, its Bound the floor, and the walk in progress. A sync
// loop stores the Next of each page it has applied and passes it back as
// From, in the same process or a later one. The zero value reads every
// issue.
type PollCursor struct {
	Position
	// Walk is the walk in progress; nil before its first page.
	Walk *PollWalk
}

// PollWalk is what a walk of polls knows across its pages.
type PollWalk struct {
	// Start is Linear's clock when it answered the walk's first page
	// (its Date header); zero when that answer carried none.
	Start time.Time
	// Delivered reports that a page of the walk delivered an issue.
	Delivered bool
}

// Restart is a cursor that starts a new walk at floor, inclusive,
// leaving nothing out.
func Restart(floor time.Time) PollCursor { return PollCursor{Position: Position{Bound: floor}} }

// Resume is c with its walk ended: the next walk starts where c's
// position is, leaving out what c leaves out.
func (c PollCursor) Resume() PollCursor {
	c.Walk = nil
	return c
}

// PollQuery selects the issues a sync loop polls.
type PollQuery struct {
	// TeamID limits the poll to one team; empty polls every team.
	TeamID          string
	IncludeArchived bool
	From            PollCursor
}

// PollPage is one page of a poll.
type PollPage struct {
	Page[Issue]
	// Next is the cursor to store once the page is applied. While
	// HasNextPage holds it continues the walk; when it does not, the walk
	// has ended and Next.Walk says what it saw, for the caller to choose
	// where the next one starts (Resume or Restart).
	Next PollCursor
}

// PollIssues returns the next page of the issues q.From selects, oldest
// update first, paged by Position (position.go). An issue updated during
// a walk moves to the end of the order and is delivered by a later page,
// again when it was delivered already. Issues can therefore repeat: the
// caller keeps an issue only when its UpdatedAt is newer
// than the copy it holds.
//
// Archived issues come only with IncludeArchived. Archiving and
// unarchiving update the issue, so a poll that includes archived issues
// sees both. A permanently deleted issue cannot be seen by polling; a
// sync loop finds one by reading the issue and getting ErrNotFound. A
// page that breaks the order the request asked for fails the call.
func (c *Client) PollIssues(ctx context.Context, q PollQuery) (PollPage, error) {
	var filter IssueFilter
	if q.TeamID != "" {
		filter.Teams = []string{q.TeamID}
	}
	p, at, err := readPosition(ctx, c, walkRows[wireIssue, Issue]{opIssues, wireIssue.typed}, oldestFirst, q.From.Position, IssueQuery{Filter: filter, IncludeArchived: q.IncludeArchived})
	if err != nil {
		return PollPage{}, err
	}
	next := PollCursor{Position: at, Walk: q.From.Walk}
	if next.Walk == nil {
		next.Walk = &PollWalk{Start: p.Served}
	} else {
		w := *next.Walk
		next.Walk = &w
	}
	next.Walk.Delivered = next.Walk.Delivered || len(p.Items) > 0
	return PollPage{Page: p, Next: next}, nil
}
