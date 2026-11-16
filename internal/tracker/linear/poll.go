package linear

import (
	"context"
	"errors"
	"time"
)

// PollCursor is where a poll resumes. A sync loop stores the Next of
// each page it has applied and passes it back as From, in the same
// process or a later one. The zero value reads every issue.
type PollCursor struct {
	// Since bounds the walk: issues updated at or after it.
	Since time.Time
	// After is Linear's cursor within the walk Since began; empty starts
	// the walk at Since.
	After string
	// Floor is a time before which every issue updated has been
	// delivered. The walk restarts from it when Linear refuses After.
	Floor time.Time
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
	// Next is the cursor to store once the page is applied.
	Next PollCursor
	// Restarted reports that Linear refused From.After, so the page
	// starts a new walk at From.Floor and repeats issues already
	// delivered.
	Restarted bool
}

// PollIssues returns the next page of the issues updated at or after
// q.From.Since, oldest update first, continuing the walk at q.From.After.
//
// Resume rule: the caller applies a page, then stores its Next; whenever
// it stops, it resumes by passing the stored Next as From. While a walk
// has pages left, Next keeps the walk's Since and carries Linear's cursor,
// so a run of issues sharing one update time larger than a page is
// drained page by page, across restarts. Floor is the update time of the
// last issue delivered: every issue updated before it has been delivered,
// none at it is skipped. When Linear refuses the cursor (an input error),
// the walk restarts at Floor, inclusive, and the page says Restarted.
// When a walk ends, Next starts the next poll at Floor, inclusive, so an
// issue updated at the same moment as the last one delivered is not
// lost. The time never moves past an update time until every issue at it
// has been delivered. Issues can therefore repeat: the caller keeps an
// issue only when its UpdatedAt is newer than the copy it holds. An issue
// updated during a walk moves to the end of the order and is delivered by
// a later page or the next poll.
//
// Archived issues come only with IncludeArchived. Archiving and
// unarchiving update the issue, so a poll that includes archived issues
// sees both. A permanently deleted issue cannot be seen by polling; a
// sync loop finds one by reading the issue and getting ErrNotFound.
func (c *Client) PollIssues(ctx context.Context, q PollQuery) (PollPage, error) {
	from := q.From
	if from.Floor.Before(from.Since) {
		from.Floor = from.Since
	}
	p, err := c.pollPage(ctx, q, from.Since, from.After)
	restarted := false
	if err != nil && from.After != "" && cursorRefused(err) {
		restarted = true
		from.Since, from.After = from.Floor, ""
		p, err = c.pollPage(ctx, q, from.Since, "")
	}
	if err != nil {
		return PollPage{}, err
	}
	floor := from.Floor
	for _, issue := range p.Items {
		if issue.UpdatedAt.After(floor) {
			floor = issue.UpdatedAt
		}
	}
	next := PollCursor{Since: floor, Floor: floor}
	if p.HasNextPage {
		next = PollCursor{Since: from.Since, After: p.EndCursor, Floor: floor}
	}
	return PollPage{Page: p, Next: next, Restarted: restarted}, nil
}

func (c *Client) pollPage(ctx context.Context, q PollQuery, since time.Time, after string) (Page[Issue], error) {
	var filter IssueFilter
	if q.TeamID != "" {
		filter.Teams = []string{q.TeamID}
	}
	filter.UpdatedSince = since
	vars, err := issueVars(IssueQuery{Filter: filter, Order: orderUpdatedOldest, IncludeArchived: q.IncludeArchived}, after)
	if err != nil {
		return Page[Issue]{}, invalid(opIssues.name, "%w", err)
	}
	return fetchPage(ctx, c, opIssues, vars, wireIssue.typed)
}

// cursorRefused reports an answer refusing a request's input as a whole
// (no path, or the page itself), which for a request that only differs
// from a valid one by its cursor is the cursor. Linear documents no code
// of its own for a refused cursor. An error deeper in the page is about
// an issue, not the cursor.
func cursorRefused(err error) bool {
	e, ok := errors.AsType[*Error](err)
	if !ok || len(e.Errors) == 0 {
		return false
	}
	for _, g := range e.Errors {
		atPage := len(g.Path) == 0 || (len(g.Path) == 1 && g.Path[0] == opIssues.root)
		if g.Code != CodeInput || !atPage {
			return false
		}
	}
	return true
}
