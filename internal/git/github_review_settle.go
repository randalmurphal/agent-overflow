package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// A GitHub review is one POST; an answer that does not say whether GitHub
// applied it (a reset or timeout after sending, a 5xx) is settled by
// reading the PR's reviews, as GitLab's lost publish is settled by its
// draft notes. The submit reads the viewer's reviews before the POST, so
// after a lost answer a review of the viewer's that was not there before
// is this submit's.

// githubViewerReviewsQuery reads the PR's newest reviews with whether the
// viewer wrote each: a review this submit posted is the newest, so it is
// in the window whatever the PR's count.
const githubViewerReviewsQuery = `query ViewerReviews($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { reviews(last: 100) { nodes { databaseId viewerDidAuthor state body commit { oid } } } }
  }
}`

// githubReviewSettleTimeout bounds the read that settles a lost answer.
const githubReviewSettleTimeout = gitlabDraftCleanupTimeout

type githubViewerReview struct {
	DatabaseID      int64  `json:"databaseId"`
	ViewerDidAuthor bool   `json:"viewerDidAuthor"`
	State           string `json:"state"`
	Body            string `json:"body"`
	Commit          *struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

// readGitHubViewerReviews returns the viewer's reviews among the PR's
// newest, by database id.
func readGitHubViewerReviews(ctx context.Context, client *forgeapi.Client, ref PRReference) (map[int64]githubViewerReview, error) {
	vars, err := githubPullVars(ref)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Repository *struct {
			PullRequest *struct {
				Reviews struct {
					Nodes []githubViewerReview `json:"nodes"`
				} `json:"reviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if _, err := client.GraphQL(ctx, "ViewerReviews", githubViewerReviewsQuery, vars, &raw); err != nil {
		return nil, err
	}
	if raw.Repository == nil || raw.Repository.PullRequest == nil {
		return nil, fmt.Errorf("GitHub answered no pull request %s#%d on %s", ref.Project(), ref.Number, ref.Host)
	}
	out := map[int64]githubViewerReview{}
	for _, review := range raw.Repository.PullRequest.Reviews.Nodes {
		if review.ViewerDidAuthor {
			out[review.DatabaseID] = review
		}
	}
	return out, nil
}

// githubReviewStates maps a REST review event to the state it submits.
var githubReviewStates = map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}

// settleGitHubReview decides a review POST whose answer was lost
// (postErr) by the viewer's reviews: none new means GitHub did not apply
// it, and postErr is returned as the submit's not-posted error; one new
// review with the request's state, body and commit means it did, and the
// answer is nil. Anything else, or a failed read, is an
// UnconfirmedSubmitError around postErr.
func settleGitHubReview(ctx context.Context, client *forgeapi.Client, ref PRReference, review githubReviewIn, before map[int64]githubViewerReview, postErr error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubReviewSettleTimeout)
	defer cancel()
	after, err := readGitHubViewerReviews(ctx, client, ref)
	if err != nil {
		return &UnconfirmedSubmitError{Err: errors.Join(postErr, fmt.Errorf("read the reviews: %w", err))}
	}
	var fresh []githubViewerReview
	for id, candidate := range after {
		if _, known := before[id]; !known && candidate.State != "PENDING" {
			fresh = append(fresh, candidate)
		}
	}
	switch {
	case len(fresh) == 0:
		return postErr
	case len(fresh) == 1 && fresh[0].State == githubReviewStates[review.Event] && strings.TrimSpace(fresh[0].Body) == strings.TrimSpace(review.Body) &&
		fresh[0].Commit != nil && strings.EqualFold(fresh[0].Commit.OID, review.CommitID):
		return nil
	}
	return &UnconfirmedSubmitError{Err: fmt.Errorf("%w; %d reviews of the viewer's are new and do not settle which is this one", postErr, len(fresh))}
}
