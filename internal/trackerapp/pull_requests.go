package trackerapp

import (
	"context"
	"errors"
	"log"
	"net/url"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// DevelopmentFailure is one read of IssuePullRequests that failed.
type DevelopmentFailure struct {
	// Where names the read: its endpoint, and the integration a detail
	// read asked for ("rest/dev-status/latest/issue/detail for <type>");
	// empty when the failure names no read.
	Where string
	// Status is the HTTP status the read's own endpoint answered with a
	// refused credential or a rate limit, which Failure's message does not
	// say; zero otherwise, as for a refusal of the sign-in check a 404
	// ran.
	Status  int
	Failure Failure
}

// IssuePullRequests are the pull requests the tracker's forge
// integrations link to issue (tracker.Provider.PullRequests, Jira's
// development panel), read on a hold of connection id for a person, and
// beside them each read of them that failed: one integration's failure
// leaves the rows the others answered. A tracker that has no such data
// (Linear) answers none with no request, and a site that keeps none
// (tracker.ErrNoDevelopmentData) none with its cause logged. The error is
// the hold's alone. No failure settles the connection's problem: the
// endpoints are undocumented, so a refusal or an answer not as measured
// says nothing of the connection, and the person's other reads of it go
// on as they were.
func (s *Service) IssuePullRequests(ctx context.Context, id, issue string) ([]tracker.LinkedPullRequest, []DevelopmentFailure, error) {
	r, err := s.interact(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	defer r.finish()
	prs, err := r.p.PullRequests(r.ctx, tracker.IssueID(issue))
	switch {
	case errors.Is(err, tracker.ErrUnsupported):
		return nil, nil, nil
	case errors.Is(err, tracker.ErrNoDevelopmentData):
		log.Printf("trackerapp: development data of %s on connection %s: %v", issue, id, err)
		return nil, nil, nil
	case err == nil:
		return prs, nil, nil
	}
	parts := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts = joined.Unwrap()
	}
	failures := make([]DevelopmentFailure, 0, len(parts))
	for _, part := range parts {
		failures = append(failures, developmentFailure(r.row.Kind, part))
	}
	return prs, failures, nil
}

// developmentFailure is err, a failed read of a connection of kind, as a
// DevelopmentFailure.
func developmentFailure(kind string, err error) DevelopmentFailure {
	f := DevelopmentFailure{Failure: ClassifyFailure(kind, err)}
	call, ok := errors.AsType[*jira.DevCallError](err)
	if !ok {
		return f
	}
	f.Where = call.Path
	if call.InstanceType != "" {
		f.Where += " for " + call.InstanceType
	}
	if auth, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		if u, perr := url.Parse(auth.URL); perr == nil && strings.HasSuffix(u.Path, "/"+call.Path) {
			f.Status = auth.Status
		}
	} else if limited, ok := errors.AsType[*trackerapi.RateLimitedError](err); ok {
		f.Status = limited.Status
	}
	return f
}
