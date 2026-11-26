package app

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// devInstance is the GitLab.com integration's instance type, as measured.
const devInstance = "oAuth-gitlab-jira-connect-gitlab.com"

// devSite is trackerBoardSite with prs linked to WEB-1 (10101) in Jira's
// development data.
func devSite(t *testing.T, prs ...trackerfake.JiraPullRequest) trackerfake.JiraSite {
	t.Helper()
	site := trackerBoardSite()
	at := slices.IndexFunc(site.Issues, func(is trackerfake.JiraIssue) bool { return is.ID == "10101" })
	if at < 0 {
		t.Fatal("the board site has no issue 10101")
	}
	site.Issues[at].PullRequests = prs
	return site
}

// devRoutes are the development data requests the fake served after
// since.
func devRoutes(f workSearchFixture, since int) []string {
	var out []string
	for _, r := range f.e.Requests(since).Requests {
		if r.Route == "GET rest/dev-status/latest/issue/summary" || r.Route == "GET rest/dev-status/latest/issue/detail" {
			out = append(out, r.Route)
		}
	}
	return out
}

func workSearchPRURL(n int) string { return "https://github.com/example/web/pull/" + strconv.Itoa(n) }

func lastTrackerSeq(f workSearchFixture) int {
	reqs := f.e.Requests(0).Requests
	if len(reqs) == 0 {
		return 0
	}
	return reqs[len(reqs)-1].Seq
}

// SearchTicketWork lists the pull requests Jira's development data links
// to the ticket after the stored list and the forge's search: one no other
// source holds is listed with the data's title, author, state and branch,
// whether or not it names the key (a merged one whose branch is gone); one
// the forge found stays as the forge answered it, a linked thread's
// record of it too; one only a linked thread recorded takes the data's
// title and author; a status that was not
// measured shows as Jira's word; and a link the app cannot open as a pull
// request is counted, not listed. It costs one summary and one detail
// request; GetTicketWork asks Jira nothing.
func TestSearchTicketWorkListsJirasDevelopmentData(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixtureOn(t, devSite(t,
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/ops!4", Name: "Rework the cache", URL: "https://gitlab.com/example/ops/-/merge_requests/4",
			Status: "MERGED", Author: "User 1", Branch: "web-1-cache"},
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/web!1", Name: "Jira's title", URL: "https://github.com/example/web/pull/1", Status: "OPEN"},
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/web!20", Name: "Release notes", URL: "https://github.com/example/web/pull/20",
			Status: "MERGED", Author: "User 1"},
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/sub/api!5", Name: "Spike", URL: "https://gitlab.com/example/sub/api/-/merge_requests/5", Status: "UNKNOWN"},
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "x", Name: "Elsewhere", URL: "https://bitbucket.org/example/web/pull-requests/3", Status: "OPEN"},
	), forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User 1"},
	}}}})
	// One linked thread was last seen with 1, which the forge finds, and
	// another with 20, whose branch names no key.
	for _, pr := range []struct {
		thread string
		n      int
		branch string
	}{{"t1", 1, "fix-login"}, {"t2", 20, "release-notes"}} {
		f.linkThread(t, pr.thread, f.project, f.dir)
		if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: pr.thread, Forge: "github", Host: "github.com",
			Project: "example/web", Number: pr.n, URL: workSearchPRURL(pr.n), Branch: pr.branch, WorktreePath: f.dir, SeenOpenAt: 1}); err != nil {
			t.Fatal(err)
		}
	}

	since := lastTrackerSeq(f)
	if _, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101"); err != nil {
		t.Fatal(err)
	}
	if got := devRoutes(f, since); len(got) != 0 {
		t.Fatalf("GetTicketWork asked Jira %v", got)
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	gitlab := func(project string, n int) gitops.PRReference {
		ns, repo, _ := gitops.SplitProjectForForge("gitlab", project)
		return gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: ns, Repo: repo, Number: n}
	}
	want := []TicketPullRequest{
		{Ref: workSearchPR(1), URL: workSearchPRURL(1), Title: "WEB-1 Fix the login", Author: "User 1", State: TicketPROpen, Branch: "fix-login", ProjectID: f.project},
		{Ref: workSearchPR(20), URL: workSearchPRURL(20), Title: "Release notes", Author: "User 1", State: TicketPRMerged, Branch: "release-notes"},
		{Ref: gitlab("example/ops", 4), URL: "https://gitlab.com/example/ops/-/merge_requests/4", Title: "Rework the cache", Author: "User 1", State: TicketPRMerged, Branch: "web-1-cache"},
		{Ref: gitlab("example/sub/api", 5), URL: "https://gitlab.com/example/sub/api/-/merge_requests/5", Title: "Spike", State: "UNKNOWN"},
	}
	if !slices.Equal(work.PullRequests, want) {
		got, _ := json.MarshalIndent(work.PullRequests, "", " ")
		t.Errorf("pull requests = %s\nwant %+v", got, want)
	}
	if work.DevelopmentUnlisted != 1 || work.DevelopmentProblem != "" {
		t.Errorf("unlisted %d, problem %q; want the Bitbucket link counted and no problem", work.DevelopmentUnlisted, work.DevelopmentProblem)
	}
	if got := devRoutes(f, since); !slices.Equal(got, []string{"GET rest/dev-status/latest/issue/summary", "GET rest/dev-status/latest/issue/detail"}) {
		t.Errorf("development requests = %v; want one summary and one detail", got)
	}
}

// A ticket Jira links no pull request to costs the summary alone and
// says nothing; a refusal of the development data is said with its
// endpoint and status beside the forge's rows, and the connection's other
// reads are not marked by it.
func TestSearchTicketWorkSaysAFailedDevelopmentRead(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login"},
	}}}})
	since := lastTrackerSeq(f)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if work.DevelopmentProblem != "" || work.DevelopmentUnlisted != 0 || len(work.PullRequests) != 1 {
		t.Fatalf("a ticket with no development data: %+v", work)
	}
	if got := devRoutes(f, since); !slices.Equal(got, []string{"GET rest/dev-status/latest/issue/summary"}) {
		t.Fatalf("development requests = %v; want the summary alone", got)
	}

	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: "GET rest/dev-status/latest/issue/summary", Kind: trackerfake.FaultRefuse,
		Status: 403, Body: json.RawMessage(`{"errorMessages":["no"]}`), Times: 1}); err != nil {
		t.Fatal(err)
	}
	work, err = f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Jira's development data could not be read (rest/dev-status/latest/issue/summary): Jira refused access (HTTP 403): the token may not make this request"; work.DevelopmentProblem != want {
		t.Errorf("problem = %q\nwant %q", work.DevelopmentProblem, want)
	}
	if len(work.PullRequests) != 1 || work.PullRequests[0].Ref != workSearchPR(1) {
		t.Errorf("the forge's rows beside the failure: %+v", work.PullRequests)
	}
	conns, err := f.a.TrackerConnections(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.ID == f.conn.ID && c.Problem != nil {
			t.Errorf("the connection was marked by the development read: %+v", c.Problem)
		}
	}
}

// A Linear ticket's Work asks Linear nothing for development data, which
// Linear has none of, and says nothing of it.
func TestSearchTicketWorkOfALinearTicketSaysNoDevelopmentData(t *testing.T) {
	t.Parallel()
	f, _, _ := costFixture(t, true, 3)
	conns, err := f.a.TrackerConnections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(conns, func(c TrackerConnection) bool { return c.Kind == store.TrackerKindLinear })
	if at < 0 {
		t.Fatal("no Linear connection")
	}
	linear := conns[at]
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	useForgefake(t, f.a, engine)
	issue := trackerCostLinear().Issues[0].ID
	since := lastTrackerSeq(workSearchFixture{trackerBoardFixture: f})
	work, err := f.a.SearchTicketWork(t.Context(), linear.ID, issue)
	if err != nil {
		t.Fatal(err)
	}
	if work.DevelopmentProblem != "" || work.DevelopmentUnlisted != 0 {
		t.Errorf("a Linear ticket: problem %q, unlisted %d", work.DevelopmentProblem, work.DevelopmentUnlisted)
	}
	if got := f.e.Requests(since).Requests; len(got) != 0 {
		t.Errorf("a Linear ticket's Work asked the trackers %d times: %+v", len(got), got)
	}

}
