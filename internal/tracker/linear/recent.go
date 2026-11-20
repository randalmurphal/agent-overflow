package linear

import (
	"context"
	"time"
)

// RecentCursor is where a walk of issues most recently updated first
// resumes, a position no update can move: the issues updated at or
// before Ceiling, less those of AtCeiling still updated exactly at it.
// The zero value starts at the newest issue.
type RecentCursor struct {
	Ceiling time.Time
	// AtCeiling are the IDs of the issues updated exactly at Ceiling that
	// were delivered; at most maxCohort.
	AtCeiling []string
}

// RecentQuery selects issues for a walk most recently updated first.
type RecentQuery struct {
	Filter          IssueFilter
	IncludeArchived bool
	// Limit is the page size, 1 to 250; zero means 250.
	Limit int
	From  RecentCursor
}

// RecentPage is one page of a walk most recently updated first.
type RecentPage struct {
	Page[Issue]
	// Next continues the walk while HasNextPage holds.
	Next RecentCursor
}

// RecentIssues returns the next page of the issues q selects, most
// recently updated first, from q.From, paged by position (see
// maxCohort). An issue updated during the walk moves to the front,
// behind the walk, and is not delivered again; no issue the update did
// not touch is skipped or repeated. A page that breaks the order fails
// the call, as does a cohort past maxCohort (*CohortTooLargeError).
func (c *Client) RecentIssues(ctx context.Context, q RecentQuery) (RecentPage, error) {
	from := position{newest: true, bound: q.From.Ceiling, at: q.From.AtCeiling}
	p, at, err := from.read(ctx, c, IssueQuery{Filter: q.Filter, IncludeArchived: q.IncludeArchived, Limit: q.Limit})
	if err != nil {
		return RecentPage{}, err
	}
	return RecentPage{Page: p, Next: RecentCursor{Ceiling: at.bound, AtCeiling: at.at}}, nil
}
