package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// A ticket's pull requests are read for a person, and no outcome of the
// read settles the connection's problem: a refusal, an answer not as
// measured and a success leave it as the connection's other reads made
// it. A failure comes with the rows the read answered beside it, each
// failed read on its own with where it was and the status its message
// does not say. A tracker or a site with no such data answers none.
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
		{"a refusal", &jira.DevCallError{Path: "rest/dev-status/latest/issue/summary", Err: &trackerapi.StatusError{Tracker: "jira", Status: 403}}},
		{"an answer not as measured", fmt.Errorf("jiraprovider: development summary of 10001: %w", &jira.DevCallError{Path: "rest/dev-status/latest/issue/summary", Err: &jira.DevShapeError{Why: "no count"}})},
		{"errors in the answer", errors.Join(&jira.DevCallError{Path: "rest/dev-status/latest/issue/detail", InstanceType: "a", Err: &jira.DevStatusError{Errors: []string{`"down"`}}})},
		{"a refused token", &jira.DevCallError{Path: "rest/dev-status/latest/issue/summary", Err: &trackerapi.AuthError{Tracker: "jira", Status: 401}}},
		{"a success", nil},
	} {
		answer = tc.err
		prs, failures, err := f.svc.IssuePullRequests(t.Context(), f.conn.ID, "10001")
		if err != nil || !slices.Equal(prs, []tracker.LinkedPullRequest{merged}) {
			t.Errorf("%s: %+v, %v; want the rows beside the failure", tc.name, prs, err)
		}
		if tc.err == nil {
			if len(failures) != 0 {
				t.Errorf("%s: failures %+v", tc.name, failures)
			}
		} else if len(failures) != 1 || !errors.Is(failures[0].Err, tc.err) && !errors.Is(tc.err, failures[0].Err) || failures[0].Failure != ClassifyFailure(f.conn.Kind, tc.err) ||
			!strings.HasPrefix(failures[0].Where, "rest/dev-status/latest/issue/") {
			t.Errorf("%s: failures %+v; want the one failure with where it was", tc.name, failures)
		}
		if p := f.problem(); p != FailureTransient {
			t.Errorf("%s: the problem is %q; want the projects read's", tc.name, p)
		}
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Projects", "PullRequests", "PullRequests", "PullRequests", "PullRequests", "PullRequests"}) {
		t.Fatalf("calls %v", got)
	}
	if slices.Contains(f.p.interactiveCalls()[before:], false) {
		t.Fatal("a pull request read was not interactive")
	}

	// Two integrations that failed are two failures; the detail of a
	// refused credential and of a rate limit carries its status.
	answer = errors.Join(
		&jira.DevCallError{Path: "rest/dev-status/latest/issue/detail", InstanceType: "a", Err: &trackerapi.RateLimitedError{Tracker: "jira", Status: 429}},
		&jira.DevCallError{Path: "rest/dev-status/latest/issue/detail", InstanceType: "b", Err: &trackerapi.StatusError{Tracker: "jira", Status: 403}},
	)
	_, failures, err := f.svc.IssuePullRequests(t.Context(), f.conn.ID, "10001")
	if err != nil || len(failures) != 2 || failures[0].Where != "rest/dev-status/latest/issue/detail for a" || failures[0].Status != 429 ||
		failures[1].Where != "rest/dev-status/latest/issue/detail for b" || failures[1].Status != 0 || failures[1].Failure.Kind != FailureForbidden {
		t.Fatalf("two failed integrations: %+v, %v", failures, err)
	}

	for name, none := range map[string]error{
		"a tracker with no such data": fmt.Errorf("linearprovider: pull requests: %w", tracker.ErrUnsupported),
		"a site that keeps none":      fmt.Errorf("jiraprovider: development summary of 10001: %w: %w", tracker.ErrNoDevelopmentData, &jira.DevAbsentError{Why: "HTTP 404"}),
	} {
		f.w.set(func(*scripted) {
			f.w.pulls = func(tracker.IssueID) ([]tracker.LinkedPullRequest, error) { return nil, none }
		})
		if prs, failures, err := f.svc.IssuePullRequests(t.Context(), f.conn.ID, "10001"); err != nil || prs != nil || failures != nil {
			t.Fatalf("%s: %+v, %+v, %v", name, prs, failures, err)
		}
	}
	if _, _, err := f.svc.IssuePullRequests(t.Context(), "", "10001"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no connection: %v", err)
	}
}
