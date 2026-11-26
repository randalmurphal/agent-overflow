package jira

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// gitlabType is the GitLab.com integration's instance type, as measured.
const gitlabType = "oAuth-gitlab-jira-connect-gitlab.com"

const (
	devSummaryURL = "/rest/dev-status/latest/issue/summary"
	devDetailURL  = "/rest/dev-status/latest/issue/detail"
)

// The summary as measured for an issue with merge requests: every key of
// the measured answer, the ones DevSummary does not read included.
const devSummaryMeasured = `{"errors":[],"configErrors":[],"summary":{
 "pullrequest":{"overall":{"count":3,"lastUpdated":"2026-11-20T10:00:00.000+0000","stateCount":1,"state":"OPEN","dataType":"pullrequest","open":true},
  "byInstanceType":{"oAuth-gitlab-jira-connect-gitlab.com":{"count":3,"name":"GitLab"},"other-type":{"count":0,"name":"Other"}}},
 "build":{},"review":{},"deployment-environment":{},
 "repository":{"overall":{"count":1},"byInstanceType":{}},
 "branch":{"overall":{"count":1,"lastUpdated":"2026-11-20T10:00:00.000+0000","dataType":"branch"},"byInstanceType":{"oAuth-gitlab-jira-connect-gitlab.com":{"count":1,"name":"GitLab"}}}}}`

// The detail as measured: branches and pull requests together, and the
// instance's own record.
const devDetailMeasured = `{"errors":[],"detail":[{
 "branches":[{"name":"WEB-1-fix","url":"https://gitlab.com/example/web/-/tree/WEB-1-fix","createPullRequestUrl":"https://gitlab.com/example/web/-/merge_requests/new","repository":{"id":"1","name":"web","url":"https://gitlab.com/example/web"},
  "lastCommit":{"id":"abc","displayId":"abc","authorTimestamp":"2026-11-20T10:00:00.000+0000","url":"https://gitlab.com/example/web/-/commit/abc","author":{"name":"User 1","avatar":"https://example.com/a.png"},"fileCount":1,"merge":false,"message":"WEB-1 fix","files":[{"path":"a.go","url":"https://example.com/a","changeType":"MODIFIED","linesAdded":1,"linesRemoved":0}]}}],
 "pullRequests":[
  {"author":{"name":"User 1","avatar":"https://example.com/a.png"},"id":"example/web!7","name":"Fix the cache","commentCount":0,"source":{"branch":"WEB-1-fix","url":"https://gitlab.com/example/web/-/tree/WEB-1-fix"},"destination":{"branch":"main"},"reviewers":[{"name":"User 2","avatar":"https://example.com/b.png","approved":true}],"status":"MERGED","url":"https://gitlab.com/example/web/-/merge_requests/7","lastUpdate":"2026-11-20T10:00:00.000+0000","repositoryId":"1","repositoryName":"web","repositoryUrl":"https://gitlab.com/example/web","ari":"ari:example"},
  {"author":{"name":"User 2","avatar":"https://example.com/b.png"},"id":"example/sub/api!12","name":"Draft the index","commentCount":2,"source":{"branch":"index","url":"https://gitlab.com/example/sub/api/-/tree/index"},"destination":{"branch":"main"},"reviewers":[],"status":"OPEN","url":"https://gitlab.com/example/sub/api/-/merge_requests/12","lastUpdate":"2026-11-21T10:00:00.000+0000","repositoryId":"2","repositoryName":"api","repositoryUrl":"https://gitlab.com/example/sub/api","ari":"ari:example2"}],
 "repositories":[],
 "_instance":{"name":"GitLab","baseUrl":"https://gitlab.com","type":"oAuth-gitlab-jira-connect-gitlab.com","id":"1","typeName":"GitLab","singleInstance":true}}]}`

func TestDevSummaryCountsEachIntegrationThatLinksAPullRequest(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", devSummaryURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "issueId", "10101")
		jiratest.Reply(200, devSummaryMeasured)(w, r, b)
	})
	sum, err := c.DevSummary(t.Context(), "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.PullRequests) != 1 || sum.PullRequests[gitlabType] != 3 {
		t.Fatalf("summary = %+v; want the GitLab integration's 3 and no integration that links none", sum)
	}
}

// Measured (M1): an issue with no pull requests answers the pull request
// part with count 0 and no integration.
func TestDevSummaryOfAnIssueWithNoPullRequestIsEmpty(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", devSummaryURL, jiratest.Reply(200, `{"errors":[],"configErrors":[],"summary":{"pullrequest":{"overall":{"count":0,"lastUpdated":null,"state":"OPEN"},"byInstanceType":{}},"build":{},"review":{},"deployment-environment":{},"repository":{},"branch":{}}}`))
	sum, err := c.DevSummary(t.Context(), "10101")
	if err != nil || len(sum.PullRequests) != 0 {
		t.Fatalf("summary = %+v, %v; want none", sum, err)
	}
}

// An answer not in the measured shape fails, naming the endpoint and
// that it is undocumented, rather than reading as no pull request.
func TestDevStatusAnswersNotAsMeasuredFail(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ path, body string }{
		"summary without a summary":                   {devSummaryURL, `{"errors":[],"configErrors":[]}`},
		"summary without its pull request part":       {devSummaryURL, `{"errors":[],"configErrors":[],"summary":{"branch":{}}}`},
		"summary without its integrations":            {devSummaryURL, `{"errors":[],"configErrors":[],"summary":{"pullrequest":{"overall":{"count":0}}}}`},
		"summary with an integration and no count":    {devSummaryURL, `{"errors":[],"configErrors":[],"summary":{"pullrequest":{"overall":{"count":1},"byInstanceType":{"x":{"name":"X"}}}}}`},
		"detail without a detail list":                {devDetailURL, `{"errors":[]}`},
		"detail without a pull request list":          {devDetailURL, `{"errors":[],"detail":[{"branches":[]}]}`},
		"detail with a pull request that has no link": {devDetailURL, `{"errors":[],"detail":[{"pullRequests":[{"id":"a!1","status":"OPEN"}]}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := jiratest.NewServer(t)
			c := newTestClient(t, f)
			f.On("GET", tc.path, jiratest.Reply(200, tc.body))
			var err error
			if tc.path == devSummaryURL {
				_, err = c.DevSummary(t.Context(), "10101")
			} else {
				_, err = c.DevPullRequests(t.Context(), "10101", gitlabType)
			}
			var answer *AnswerError
			if !errors.As(err, &answer) || !strings.Contains(err.Error(), "rest/dev-status/latest/issue/") || !strings.Contains(err.Error(), "undocumented and may have changed") {
				t.Fatalf("err = %v; want an AnswerError naming the endpoint and that it is undocumented", err)
			}
		})
	}
}

// An element in errors or configErrors fails the read with the element
// as given, cut short, since no element's fields have been measured.
func TestDevStatusErrorsFailWithTheElementCutShort(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	long := strings.Repeat("x", 400)
	f.On("GET", devSummaryURL, jiratest.Reply(200, `{"errors":[],"configErrors":[{"message":"`+long+`"}],"summary":{"pullrequest":{"overall":{"count":0},"byInstanceType":{}}}}`))
	f.On("GET", devDetailURL, jiratest.Reply(200, `{"errors":["the integration is down"],"detail":[]}`))
	_, err := c.DevSummary(t.Context(), "10101")
	var dev *DevStatusError
	if !errors.As(err, &dev) || dev.Path != "rest/dev-status/latest/issue/summary" || len(dev.Errors) != 1 ||
		dev.Errors[0] != (`{"message":"`+long)[:devStatusErrorBytes]+"…" {
		t.Fatalf("summary err = %#v", err)
	}
	_, err = c.DevPullRequests(t.Context(), "10101", gitlabType)
	if !errors.As(err, &dev) || !slices.Equal(dev.Errors, []string{`"the integration is down"`}) ||
		err.Error() != `jira: rest/dev-status/latest/issue/detail answered errors: "the integration is down"` {
		t.Fatalf("detail err = %v", err)
	}
}

// A refusal is said with its HTTP status and the endpoint, as every
// other read's is; what a refusal of this endpoint means is unmeasured.
func TestDevStatusRefusalSaysItsStatusAndEndpoint(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", devSummaryURL, jiratest.Reply(403, `{"errorMessages":["You do not have the permission to see the specified issue."]}`))
	_, err := c.DevSummary(t.Context(), "10101")
	var status *trackerapi.StatusError
	if !errors.As(err, &status) || status.Status != 403 || !strings.Contains(status.URL, "rest/dev-status/latest/issue/summary") {
		t.Fatalf("err = %#v", err)
	}
}

func TestDevPullRequestsReadsEachPullRequestAndNoBranch(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", devDetailURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "issueId", "10101")
		jiratest.WantQuery(t, r, "applicationType", gitlabType)
		jiratest.WantQuery(t, r, "dataType", "pullrequest")
		jiratest.Reply(200, devDetailMeasured)(w, r, b)
	})
	prs, err := c.DevPullRequests(t.Context(), "10101", gitlabType)
	if err != nil {
		t.Fatal(err)
	}
	want := []DevPullRequest{
		{ID: "example/web!7", Name: "Fix the cache", URL: "https://gitlab.com/example/web/-/merge_requests/7", Status: "MERGED", Author: "User 1", Branch: "WEB-1-fix"},
		{ID: "example/sub/api!12", Name: "Draft the index", URL: "https://gitlab.com/example/sub/api/-/merge_requests/12", Status: "OPEN", Author: "User 2", Branch: "index"},
	}
	if !slices.Equal(prs, want) {
		t.Fatalf("pull requests = %+v", prs)
	}
}

// The endpoints take an issue's number: a key, or no issue, is refused
// before anything is sent, as is a detail read with no instance type.
func TestDevStatusRefusesAKeyBeforeSending(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var notSent *trackerapi.NotSentError
	for _, id := range []string{"WEB-1", "", "10101 "} {
		if _, err := c.DevSummary(t.Context(), id); !errors.As(err, &notSent) {
			t.Errorf("summary of %q: %v", id, err)
		}
		if _, err := c.DevPullRequests(t.Context(), id, gitlabType); !errors.As(err, &notSent) {
			t.Errorf("detail of %q: %v", id, err)
		}
	}
	if _, err := c.DevPullRequests(t.Context(), "10101", ""); !errors.As(err, &notSent) {
		t.Errorf("detail with no instance type: %v", err)
	}
	if reqs := f.Requests(); len(reqs) != 0 {
		t.Fatalf("sent %v", reqs)
	}
}

// devCases are the conformance cases of the development panel.
var devCases = map[string]func(t *testing.T, f *fakeEnv){
	"DevSummary": func(t *testing.T, f *fakeEnv) {
		seedPullRequests(t, f)
		if sum := got(f.c.DevSummary(f.ctx, "10101")).must(t); len(sum.PullRequests) != 1 || sum.PullRequests[gitlabType] != 2 {
			t.Fatalf("summary of WEB-1 = %+v", sum)
		}
		if sum := got(f.c.DevSummary(f.ctx, "10102")).must(t); len(sum.PullRequests) != 0 {
			t.Fatalf("summary of WEB-2, which has none = %+v", sum)
		}
	},
	"DevPullRequests": func(t *testing.T, f *fakeEnv) {
		seedPullRequests(t, f)
		prs := got(f.c.DevPullRequests(f.ctx, "10101", gitlabType)).must(t)
		want := []DevPullRequest{
			{ID: "example/web!7", Name: "Fix the cache", URL: "https://gitlab.com/example/web/-/merge_requests/7", Status: "MERGED", Author: "User 1", Branch: "WEB-1-fix"},
			{ID: "example/web!9", Name: "Undo the cache", URL: "https://gitlab.com/example/web/-/merge_requests/9", Status: "DECLINED", Branch: "WEB-1-undo"},
		}
		if !slices.Equal(prs, want) {
			t.Fatalf("pull requests of WEB-1 = %+v", prs)
		}
	},
}

// seedPullRequests links two GitLab merge requests to WEB-1.
func seedPullRequests(t *testing.T, f *fakeEnv) {
	t.Helper()
	site := conformanceSite(f.clock.Now())
	site.Issues[0].PullRequests = []trackerfake.JiraPullRequest{
		{InstanceType: gitlabType, ID: "example/web!7", Name: "Fix the cache", URL: "https://gitlab.com/example/web/-/merge_requests/7", Status: "MERGED", Author: "User 1", Branch: "WEB-1-fix"},
		{InstanceType: gitlabType, ID: "example/web!9", Name: "Undo the cache", URL: "https://gitlab.com/example/web/-/merge_requests/9", Status: "DECLINED", Branch: "WEB-1-undo"},
	}
	check(t, f.e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}))
}
