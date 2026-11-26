package jiraprovider

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/sync/errgroup"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// PullRequests reads the development panel (jira.Client.DevSummary,
// then DevPullRequests): one summary request, then, for each integration
// whose count is above zero, one detail request, those at once. An issue
// no integration links a pull request to costs the summary alone. The
// rows are in the integrations' name order, each integration's in the
// order it answered.
func (p *Provider) PullRequests(ctx context.Context, issue tracker.IssueID) ([]tracker.LinkedPullRequest, error) {
	if err := checkIssue(issue); err != nil {
		return nil, err
	}
	sum, err := p.jira.DevSummary(ctx, string(issue))
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: development summary of %s: %w", issue, err)
	}
	kinds := make([]string, 0, len(sum.PullRequests))
	for kind := range sum.PullRequests {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	found := make([][]jira.DevPullRequest, len(kinds))
	g, gctx := errgroup.WithContext(ctx)
	for i, kind := range kinds {
		g.Go(func() error {
			prs, err := p.jira.DevPullRequests(gctx, string(issue), kind)
			if err != nil {
				return fmt.Errorf("jiraprovider: %s pull requests of %s: %w", kind, issue, err)
			}
			found[i] = prs
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var out []tracker.LinkedPullRequest
	for _, prs := range found {
		for _, pr := range prs {
			out = append(out, tracker.LinkedPullRequest{
				URL: pr.URL, Title: pr.Name, Author: pr.Author, Branch: pr.Branch, State: devState(pr.Status), Status: pr.Status,
			})
		}
	}
	return out, nil
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
