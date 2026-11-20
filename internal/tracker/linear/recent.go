package linear

import "context"

// RecentQuery selects issues for a walk most recently updated first.
type RecentQuery struct {
	Filter          IssueFilter
	IncludeArchived bool
	// Limit is the page size, 1 to 250; zero means 250.
	Limit int
	// From is where the walk resumes: a Position in the newest-first
	// update order, its Bound the ceiling; the zero value starts at the
	// newest issue.
	From Position
}

// RecentPage is one page of a walk most recently updated first.
type RecentPage struct {
	Page[Issue]
	// Next continues the walk while HasNextPage holds.
	Next Position
}

// RecentIssues returns the next page of the issues q selects, most
// recently updated first, from q.From, paged by Position (position.go).
// An issue updated during the walk moves to the front, behind the walk,
// and is not delivered again; no issue the update did not touch is
// skipped or repeated. A page that breaks the order fails the call.
func (c *Client) RecentIssues(ctx context.Context, q RecentQuery) (RecentPage, error) {
	p, next, err := readPosition(ctx, c, newestFirst, q.From, IssueQuery{Filter: q.Filter, IncludeArchived: q.IncludeArchived, Limit: q.Limit})
	if err != nil {
		return RecentPage{}, err
	}
	return RecentPage{Page: p, Next: next}, nil
}
