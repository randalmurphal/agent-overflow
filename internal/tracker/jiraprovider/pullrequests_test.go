package jiraprovider

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// gitlabType is the GitLab.com integration's instance type, as measured.
const gitlabType = "oAuth-gitlab-jira-connect-gitlab.com"

// A ticket's pull requests cost the summary, then one detail read for
// the one integration that links any; a ticket with none costs the
// summary alone. Each measured status has its state.
func TestPullRequestsReadTheSummaryThenEachIntegrationThatLinksAny(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var routes []string
	s := newEditedScenario(t, func(r trackerfake.Request) {
		mu.Lock()
		defer mu.Unlock()
		routes = append(routes, r.Route)
	}, func(site *trackerfake.JiraSite) {
		site.Tempo = nil
		site.Issues[0].PullRequests = []trackerfake.JiraPullRequest{
			{InstanceType: gitlabType, ID: "example/web!7", Name: "Fix the cache", URL: "https://gitlab.com/example/web/-/merge_requests/7", Status: "MERGED", Author: "User 1", Branch: "WEB-1-fix"},
			{InstanceType: gitlabType, ID: "example/sub/web!8", Name: "Cache again", URL: "https://gitlab.com/example/sub/web/-/merge_requests/8", Status: "OPEN", Branch: "WEB-1-again"},
			{InstanceType: gitlabType, ID: "example/web!9", Name: "Undo the cache", URL: "https://gitlab.com/example/web/-/merge_requests/9", Status: "DECLINED", Author: "User 2", Branch: "WEB-1-undo"},
		}
	})
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := routes
		routes = nil
		return out
	}
	prs, err := s.p.PullRequests(s.ctx, "10101")
	if err != nil {
		t.Fatal(err)
	}
	want := []tracker.LinkedPullRequest{
		{URL: "https://gitlab.com/example/web/-/merge_requests/7", Title: "Fix the cache", Author: "User 1", Branch: "WEB-1-fix", State: tracker.PullRequestMerged, Status: "MERGED"},
		{URL: "https://gitlab.com/example/sub/web/-/merge_requests/8", Title: "Cache again", Branch: "WEB-1-again", State: tracker.PullRequestOpen, Status: "OPEN"},
		{URL: "https://gitlab.com/example/web/-/merge_requests/9", Title: "Undo the cache", Author: "User 2", Branch: "WEB-1-undo", State: tracker.PullRequestClosed, Status: "DECLINED"},
	}
	if !slices.Equal(prs, want) {
		t.Fatalf("pull requests = %+v", prs)
	}
	if got := taken(); !slices.Equal(got, []string{"GET rest/dev-status/latest/issue/summary", "GET rest/dev-status/latest/issue/detail"}) {
		t.Fatalf("requests %v; want the summary and one detail", got)
	}
	prs, err = s.p.PullRequests(s.ctx, "10102")
	if err != nil || len(prs) != 0 {
		t.Fatalf("pull requests of a ticket with none = %+v, %v", prs, err)
	}
	if got := taken(); !slices.Equal(got, []string{"GET rest/dev-status/latest/issue/summary"}) {
		t.Fatalf("requests %v; want the summary alone", got)
	}
}

// A key is refused before anything is sent: the endpoints take an
// issue's number.
func TestPullRequestsRefuseAKey(t *testing.T) {
	t.Parallel()
	s := newScenario(t, false)
	if _, err := s.p.PullRequests(s.ctx, "WEB-1"); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if n := len(s.e.Requests(0).Requests); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// A status that was not measured keeps its pull request, with no state
// and the tracker's word in Status for the caller to show.
func TestDevStateKnowsOnlyTheMeasuredStatuses(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]tracker.PullRequestState{
		"OPEN": tracker.PullRequestOpen, "MERGED": tracker.PullRequestMerged, "DECLINED": tracker.PullRequestClosed,
		"DRAFT": "", "UNKNOWN": "", "open": "",
	} {
		if got := devState(status); got != want {
			t.Errorf("devState(%q) = %q, want %q", status, got, want)
		}
	}
}
