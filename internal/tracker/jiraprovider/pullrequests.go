package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// PullRequests reads the development panel (jira.Client.DevSummary,
// then DevPullRequests): one summary request, then, for each integration
// whose count is above zero, one detail request, those at once. An issue
// no integration links a pull request to costs the summary alone, and a
// site whose summary says it keeps no development data
// (jira.DevAbsentError) fails wrapping tracker.ErrNoDevelopmentData. The
// rows are in the integrations' name order, each integration's in the
// order it answered. One integration's failure leaves the others' rows:
// they are returned beside errors.Join of each failed read's error.
func (p *Provider) PullRequests(ctx context.Context, issue tracker.IssueID) ([]tracker.LinkedPullRequest, error) {
	if err := checkIssue(issue); err != nil {
		return nil, err
	}
	sum, err := p.jira.DevSummary(ctx, string(issue))
	if _, absent := errors.AsType[*jira.DevAbsentError](err); absent {
		return nil, fmt.Errorf("jiraprovider: development summary of %s: %w: %w", issue, tracker.ErrNoDevelopmentData, err)
	}
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: development summary of %s: %w", issue, err)
	}
	kinds := make([]string, 0, len(sum.PullRequests))
	for kind := range sum.PullRequests {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	found := make([][]jira.DevPullRequest, len(kinds))
	failed := make([]error, len(kinds))
	var wg sync.WaitGroup
	for i, kind := range kinds {
		wg.Go(func() {
			found[i], failed[i] = p.jira.DevPullRequests(ctx, string(issue), kind)
		})
	}
	wg.Wait()
	var out []tracker.LinkedPullRequest
	for _, prs := range found {
		for _, pr := range prs {
			out = append(out, tracker.LinkedPullRequest{
				URL: pr.URL, Title: pr.Name, Author: pr.Author, Branch: pr.Branch, State: devState(pr.Status), Status: pr.Status,
			})
		}
	}
	return out, errors.Join(failed...)
}

// devState is the state of a development panel status. Measured on the
// GitLab integration: OPEN, MERGED and DECLINED (a merge request closed
// without a merge). Any other word is not known: empty, and the caller
// shows Status.
func devState(status string) tracker.PullRequestState {
	switch status {
	case "OPEN":
		return tracker.PullRequestOpen
	case "MERGED":
		return tracker.PullRequestMerged
	case "DECLINED":
		return tracker.PullRequestClosed
	}
	return ""
}
