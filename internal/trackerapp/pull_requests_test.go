package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// A ticket's pull requests are read for a person, and no outcome of the
// read settles the connection's problem: a refusal, an answer not as
// measured and a success leave it as the connection's other reads made
// it. A tracker with no such data answers none.
func TestIssuePullRequestsNeverSettleTheProblem(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	merged := tracker.LinkedPullRequest{URL: "https://gitlab.com/example/web/-/merge_requests/7", Title: "Fix", State: tracker.PullRequestMerged, Status: "MERGED"}
	var answer error
	f.w.set(func(*scripted) {
		f.w.projects = func() ([]tracker.Project, error) {
			return nil, &trackerapi.TransientError{Err: errors.New("reset")}
		}
		f.w.pulls = func(issue tracker.IssueID) ([]tracker.LinkedPullRequest, error) {
			if issue != "10001" {
				t.Errorf("read the pull requests of %s", issue)
			}
			return []tracker.LinkedPullRequest{merged}, answer
		}
	})
	before := len(f.p.callsInOrder())
	// A problem the connection already has stays through every outcome.
	if _, err := f.svc.Projects(t.Context(), f.conn.ID); err == nil || f.problem() != FailureTransient {
		t.Fatalf("projects: %v, problem %q", err, f.problem())
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a refusal", &trackerapi.StatusError{Tracker: "jira", Status: 403, URL: "https://example.atlassian.net/rest/dev-status/latest/issue/summary"}},
		{"an answer not as measured", fmt.Errorf("jiraprovider: development summary of 10001: %w", &jira.AnswerError{Message: "rest/dev-status/latest/issue/summary answered in a shape this app does not know"})},
		{"errors in the answer", &jira.DevStatusError{Path: "rest/dev-status/latest/issue/detail", Errors: []string{`"down"`}}},
		{"a success", nil},
	} {
		answer = tc.err
		prs, err := f.svc.IssuePullRequests(t.Context(), f.conn.ID, "10001")
		if tc.err == nil {
			if err != nil || !slices.Equal(prs, []tracker.LinkedPullRequest{merged}) {
				t.Errorf("%s: %+v, %v", tc.name, prs, err)
			}
		} else if cf, ok := errors.AsType[*CallFailure](err); !ok || !errors.Is(err, tc.err) || cf.Failure != ClassifyFailure(f.conn.Kind, tc.err) || prs != nil {
			t.Errorf("%s: %+v, %v; want the failure as a CallFailure", tc.name, prs, err)
		}
		if p := f.problem(); p != FailureTransient {
			t.Errorf("%s: the problem is %q; want the projects read's", tc.name, p)
		}
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Projects", "PullRequests", "PullRequests", "PullRequests", "PullRequests"}) {
		t.Fatalf("calls %v", got)
	}
	if slices.Contains(f.p.interactiveCalls()[before:], false) {
		t.Fatal("a pull request read was not interactive")
	}

	f.w.set(func(*scripted) {
		f.w.pulls = func(tracker.IssueID) ([]tracker.LinkedPullRequest, error) {
			return nil, fmt.Errorf("linearprovider: pull requests: %w", tracker.ErrUnsupported)
		}
	})
	if prs, err := f.svc.IssuePullRequests(t.Context(), f.conn.ID, "10001"); err != nil || prs != nil {
		t.Fatalf("a tracker with no such data: %+v, %v", prs, err)
	}
	if _, err := f.svc.IssuePullRequests(t.Context(), "", "10001"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no connection: %v", err)
	}
}
