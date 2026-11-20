package linear

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// A walk in update order never pages by Linear's cursor. The cursor
// follows its issue: it means after where that issue is now (measured),
// so a walk in an order an update changes skips every issue between a
// page's last issue and its new place when it is updated between pages.
// Such a walk pages by position instead: a bound, the update time of the
// last issue delivered, and the IDs delivered at exactly it (a cohort).
// Each page asks again for the issues at or past the bound, less the
// cohort's still updated exactly at it (an or of updatedAt past the
// bound and id nin). Linear accepts that filter and sorts by it
// correctly under an oldest-update-first sort and, with lte and lt,
// under a newest-first one (both measured). An issue updated
// during a walk moves in the order and is delivered again where it now
// stands, or not at all when it moved behind the walk; no issue the
// update did not touch is skipped or repeated.

// maxCohort bounds a cohort, the issues sharing the bound's update time
// that the next page leaves out by ID. Every page request names them, at
// about 39 bytes each, so the bound keeps a request's variables under 40
// KB. Linear stores update times to the millisecond and gave the issues
// of one batch update distinct times (measured), so a cohort this large
// is a bulk change of another kind.
const maxCohort = 1000

// position is a walk's place in update order: the issues at or past
// bound (at or after it oldest first, at or before it newest first),
// less those of at still updated exactly at it. A zero bound is the
// start of the order.
type position struct {
	newest bool
	bound  time.Time
	at     []string
}

// filter is f narrowed to the issues p has not delivered.
func (p position) filter(f *issueFilterInput) *issueFilterInput {
	if p.bound.IsZero() {
		return f
	}
	out := &issueFilterInput{}
	if f != nil {
		out.And = slices.Clone(f.And)
	}
	b := formatTime(p.bound)
	atOrPast, past := &dateComparator{Gte: b}, &dateComparator{Gt: b}
	if p.newest {
		atOrPast, past = &dateComparator{Lte: b}, &dateComparator{Lt: b}
	}
	out.And = append(out.And, issueFilterInput{UpdatedAt: atOrPast})
	if len(p.at) > 0 {
		// An ID alone would also leave out a cohort issue updated again
		// during the walk, until a newer issue moved the bound.
		out.And = append(out.And, issueFilterInput{Or: []issueFilterInput{{UpdatedAt: past}, {ID: &idComparator{Nin: p.at}}}})
	}
	return out
}

// read is the page of the issues q selects past p, in p's order, and
// the position after it. A page that breaks the order, re-lists a cohort
// issue or holds none while saying more follow fails, as does a cohort
// past maxCohort (*CohortTooLargeError).
func (p position) read(ctx context.Context, c *Client, q IssueQuery) (Page[Issue], position, error) {
	// Linear's times are milliseconds and formatTime sends no finer, so
	// the bound the page answers is the bound truncated.
	p.bound = p.bound.Truncate(time.Millisecond)
	q.Order = orderUpdatedOldest
	if p.newest {
		q.Order = orderUpdatedNewest
	}
	vars, err := issueVars(q, "")
	if err == nil && len(p.at) > 0 {
		err = checkIDs("the cohort", p.at)
	}
	if err != nil {
		return Page[Issue]{}, position{}, invalid(opIssues.name, "%w", err)
	}
	vars.Filter = p.filter(vars.Filter)
	pg, err := fetchPage(ctx, c, opIssues, vars, wireIssue.typed)
	if err != nil {
		return Page[Issue]{}, position{}, err
	}
	if pg.HasNextPage && len(pg.Items) == 0 {
		// The position would not move, so the next page would ask again.
		return Page[Issue]{}, position{}, fmt.Errorf("linear %s: a page with no issues says more follow", opIssues.name)
	}
	next, err := p.advance(pg.Items)
	if err != nil {
		return Page[Issue]{}, position{}, err
	}
	return pg, next, nil
}

// advance is the position after items, read from p.
func (p position) advance(items []Issue) (position, error) {
	next := position{newest: p.newest, bound: p.bound, at: p.at}
	for _, is := range items {
		order := is.UpdatedAt.Compare(next.bound)
		if next.newest {
			order = -order
			if next.bound.IsZero() {
				order = 1
			}
		}
		switch {
		case order < 0:
			return position{}, fmt.Errorf("linear %s: issue %s, updated %s, comes after an issue updated %s",
				opIssues.name, is.ID, formatTime(is.UpdatedAt), formatTime(next.bound))
		case order > 0:
			next.bound, next.at = is.UpdatedAt, nil
		case slices.Contains(next.at, is.ID):
			return position{}, fmt.Errorf("linear %s: issue %s was left out by ID and came back", opIssues.name, is.ID)
		}
		next.at = append(slices.Clip(next.at), is.ID)
	}
	if len(next.at) > maxCohort {
		return position{}, &CohortTooLargeError{At: next.bound, Count: len(next.at)}
	}
	return next, nil
}

// CohortTooLargeError is a walk in update order that met more issues
// sharing one update time than its position can leave out by ID
// (maxCohort).
type CohortTooLargeError struct {
	At    time.Time
	Count int
}

func (e *CohortTooLargeError) Error() string {
	return fmt.Sprintf("linear Issues: %d issues share the update time %s, more than a walk's position holds (%d)",
		e.Count, formatTime(e.At), maxCohort)
}

// PollCursor is where a poll resumes, a position no update can move:
// the issues updated at or after Floor, less those of AtFloor still
// updated exactly at Floor. A sync loop stores the Next of each page it
// has applied and passes it back as From, in the same process or a
// later one. The zero value reads every issue.
type PollCursor struct {
	// Floor bounds the walk: issues updated at or after it.
	Floor time.Time
	// AtFloor are the IDs of the issues updated exactly at Floor that
	// were delivered, which the walk leaves out; at most maxCohort.
	AtFloor []string
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
func Restart(floor time.Time) PollCursor { return PollCursor{Floor: floor} }

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
// update first: those updated at or after its Floor, less its AtFloor,
// paged by position (see maxCohort). An issue updated during a walk
// moves to the end of the order and is delivered by a later page, again
// when it was delivered already. Issues can therefore repeat: the caller
// keeps an issue only when its UpdatedAt is newer than the copy it
// holds.
//
// Archived issues come only with IncludeArchived. Archiving and
// unarchiving update the issue, so a poll that includes archived issues
// sees both. A permanently deleted issue cannot be seen by polling; a
// sync loop finds one by reading the issue and getting ErrNotFound. A
// page that breaks the order the request asked for fails the call, as
// does a cohort past maxCohort (*CohortTooLargeError).
func (c *Client) PollIssues(ctx context.Context, q PollQuery) (PollPage, error) {
	var filter IssueFilter
	if q.TeamID != "" {
		filter.Teams = []string{q.TeamID}
	}
	from := position{bound: q.From.Floor, at: q.From.AtFloor}
	p, at, err := from.read(ctx, c, IssueQuery{Filter: filter, IncludeArchived: q.IncludeArchived})
	if err != nil {
		return PollPage{}, err
	}
	next := PollCursor{Floor: at.bound, AtFloor: at.at, Walk: q.From.Walk}
	if next.Walk == nil {
		next.Walk = &PollWalk{Start: p.Served}
	} else {
		w := *next.Walk
		next.Walk = &w
	}
	next.Walk.Delivered = next.Walk.Delivered || len(p.Items) > 0
	return PollPage{Page: p, Next: next}, nil
}
