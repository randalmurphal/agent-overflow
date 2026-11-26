package trackerapp

import (
	"context"
	"errors"

	"agent-overflow/internal/tracker"
)

// IssuePullRequests are the pull requests the tracker's forge
// integrations link to issue (tracker.Provider.PullRequests, Jira's
// development panel), read on a hold of connection id for a person; none
// on a tracker that has no such data (Linear), with no request. Its
// failure is returned as a *CallFailure and never settles the
// connection's problem: the endpoints are undocumented, so a refusal or
// an answer not as measured says nothing of the connection, and the
// person's other reads of it go on as they were.
func (s *Service) IssuePullRequests(ctx context.Context, id, issue string) ([]tracker.LinkedPullRequest, error) {
	r, err := s.interact(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.finish()
	prs, err := r.p.PullRequests(r.ctx, tracker.IssueID(issue))
	switch {
	case errors.Is(err, tracker.ErrUnsupported):
		return nil, nil
	case err != nil:
		return nil, callFailure(r.row.Kind, err)
	}
	return prs, nil
}
