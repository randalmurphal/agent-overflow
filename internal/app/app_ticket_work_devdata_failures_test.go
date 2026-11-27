package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A read that did not answer within the wait is said beside a refusal
// another integration already gave.
func TestSearchTicketWorkSaysARefusalBesideTheWait(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixtureOn(t, trackerBoardSetup{site: devSite(t,
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "a!4", Name: "A", URL: "https://gitlab.com/example/ops/-/merge_requests/4", Status: "MERGED"},
		trackerfake.JiraPullRequest{InstanceType: devGitHub, ID: "b#9", Name: "B", URL: "https://github.com/example/api/pull/9", Status: "OPEN"},
	), budget: 300 * time.Millisecond}, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	detail := "GET rest/dev-status/latest/issue/detail"
	for _, fault := range []trackerfake.Fault{
		{Origin: trackerBoardOrigin, Route: detail, Query: map[string]string{"applicationType": devGitHub}, Kind: trackerfake.FaultRefuse, Status: 403, Body: json.RawMessage(`{"errorMessages":["no"]}`)},
		{Origin: trackerBoardOrigin, Route: detail, Query: map[string]string{"applicationType": devInstance}, Kind: trackerfake.FaultHold},
	} {
		if err := f.e.AddFault(fault); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { f.e.ClearFaults(trackerBoardOrigin, "") })
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(work.DevelopmentProblem, "did not answer within 300ms") || !strings.Contains(work.DevelopmentProblem, "for "+devGitHub+"): Jira refused access (HTTP 403)") {
		t.Errorf("problem = %q; want the timeout and the refusal", work.DevelopmentProblem)
	}
}

// A 401 of the sign-in check a summary's 404 ran is said as a refused
// token, not as the summary's status.
func TestSearchTicketWorkSaysASignInRefusalAsNoStatusOfTheSummary(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	for _, fault := range []trackerfake.Fault{
		{Origin: trackerBoardOrigin, Route: "GET rest/dev-status/latest/issue/summary", Kind: trackerfake.FaultRefuse, Status: 404, Body: json.RawMessage(`{"errorMessages":["no"]}`), Times: 1},
		{Origin: trackerBoardOrigin, Route: "GET rest/api/3/myself", Kind: trackerfake.FaultRefuse, Status: 401, Body: json.RawMessage(`{}`), Times: 2},
	} {
		if err := f.e.AddFault(fault); err != nil {
			t.Fatal(err)
		}
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if work.DevelopmentProblem == "" || strings.Contains(work.DevelopmentProblem, "summary, HTTP 401") {
		t.Errorf("problem = %q; want the refusal said without the check's status on the summary's path", work.DevelopmentProblem)
	}
}

// A row Jira gives with no status keeps the state a linked thread
// recorded, and takes Jira's title.
func TestAddDevelopmentKeepsARecordedStateJiraLeavesOut(t *testing.T) {
	ref, err := gitops.ParsePRURL("https://github.com/example/web/pull/20")
	if err != nil {
		t.Fatal(err)
	}
	r := ticketWorkRead{work: TicketWork{PullRequests: []TicketPullRequest{{Ref: ref, URL: "https://github.com/example/web/pull/20", State: TicketPRMerged, Branch: "notes"}}},
		recorded: map[string]bool{ref.Identity(): true}}
	r.addDevelopment(ticketDevelopment{prs: []tracker.LinkedPullRequest{{URL: "https://github.com/example/web/pull/20", Title: "Release notes"}}})
	if got := r.work.PullRequests[0]; got.State != TicketPRMerged || got.Title != "Release notes" {
		t.Fatalf("row = %+v; want Jira's title with the recorded merged state", got)
	}
}
