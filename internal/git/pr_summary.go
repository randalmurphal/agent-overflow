package git

import "context"

// UnresolvedThreads counts the threads that can be resolved and are not.
func UnresolvedThreads(threads []ReviewThread) int {
	n := 0
	for _, thread := range threads {
		if thread.IsResolvable && !thread.IsResolved {
			n++
		}
	}
	return n
}

// ReadPRSummary reads ref's detail with its readiness counts filled, for
// the Pull requests page's list row: approvals, the unresolved thread
// count (PRReadParts.Unresolved), which costs GitHub a request per 100
// threads past the first 100 and GitLab one per 100 discussions, and on
// GitLab the viewer's own review (PRReadParts.ViewerReview, one request
// per 100 reviewers until the viewer's).
func (c *Core) ReadPRSummary(ctx context.Context, ref PRReference) (PRDetail, error) {
	read, err := c.ReadPR(ctx, ref, PRReadParts{Detail: true, Unresolved: true, ViewerReview: true}, nil, nil)
	return read.Detail, err
}
