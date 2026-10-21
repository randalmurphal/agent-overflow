package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// expectHandled fails on any invocation the fake did not implement and
// returns the routes the rig's Core sent.
func expectHandled(t *testing.T, r *rig) []string {
	t.Helper()
	var routes []string
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
		routes = append(routes, inv.Route)
	}
	return routes
}

// listsFixture is a GitHub repository and a GitLab project whose open
// pulls cover the list rows' cases: the viewer's draft, a fork's with a
// review requested from the viewer, one the viewer requested changes on,
// and a GitLab merge request with reviewers.
func listsFixture() forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/lists", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Mine", Author: "alice", AuthorName: "Alice Ng", Draft: true, HeadRef: "alice/mine", HeadSHA: strings.Repeat("1", 40),
				CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-02T00:00:00Z", CommittedAt: "2026-10-01T12:00:00Z", Commits: 4,
				Comments: []forgefake.Comment{{Body: "a"}, {Body: "b"}},
				CI:       &forgefake.Pipeline{Jobs: []forgefake.Job{{Name: "unit", Status: "running"}}}},
			{Number: 2, Title: "Fork", Author: "carol", HeadRepo: "carol/lists", Reviewers: []string{"alice"}, Mergeable: "conflicts",
				CI: &forgefake.Pipeline{Jobs: []forgefake.Job{{Name: "unit", Status: "failed"}}}},
			{Number: 3, Title: "Asked", Author: "carol", BaseRef: "release",
				Reviews: []forgefake.Review{{Author: "alice", State: "CHANGES_REQUESTED", Body: "no", SubmittedAt: "2026-10-03T00:00:00Z"}}},
			{Number: 4, Title: "Closed", State: "closed"},
		}},
		{Forge: "gitlab", Project: "grp/lists", ID: 5500, Merge: &forgefake.RepoMerge{DiscussionsMustResolve: true}, Pulls: []forgefake.Pull{
			{Number: 7, Title: "Draft: mine", Author: "alice", Reviewers: []string{"bob"}, HeadSHA: strings.Repeat("7", 40), UpdatedAt: "2026-10-05T00:00:00Z"},
			{Number: 8, Title: "Fork", Author: "carol", HeadRepo: "carol/lists", Reviewers: []string{"dave", "alice"}, Mergeable: "conflicts",
				Readiness: &forgefake.Readiness{RequiredApprovals: 1},
				Threads:   []forgefake.Thread{{Path: "a.go", Line: intPtr(1), Comments: []forgefake.Comment{{Body: "fix"}}}}},
			{Number: 9, Title: "Approval wanted", Author: "carol", Readiness: &forgefake.Readiness{RequiredApprovals: 1}},
		}},
	}}
}

// The project list of each forge reads through the app's own forge code
// in one list read, the checkout's origin naming the repository.
func TestProjectPRListsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, listsFixture())
	checkout := func(origin string) string {
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return dir
	}
	gh, err := r.core.OriginRepository(t.Context(), checkout("git@github.com:acme/lists.git"))
	if err != nil {
		t.Fatalf("OriginRepository: %v", err)
	}
	rows, err := r.core.ListProjectPRs(t.Context(), gh)
	if err != nil || len(rows) != 3 {
		t.Fatalf("gh ListProjectPRs = %+v, %v", rows, err)
	}
	asked, fork, mine := rows[0], rows[1], rows[2]
	if mine.Ref != (gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "lists", Number: 1}) || mine.NodeID == "" ||
		!mine.Draft || !mine.ViewerIsAuthor || mine.Author != (gitops.PRListAuthor{Login: "alice", Name: "Alice Ng"}) ||
		mine.HeadSHA != strings.Repeat("1", 40) || mine.HeadRefName != "alice/mine" || mine.Comments != 2 || mine.Commits != 4 ||
		mine.LastCommitAt != "2026-10-01T12:00:00Z" || mine.CreatedAt != "2026-10-01T00:00:00Z" || mine.ChecksState != "pending" ||
		mine.MergeStatus != "blocked" || mine.URL != "https://github.com/acme/lists/pull/1" {
		t.Errorf("gh own draft = %+v", mine)
	}
	if !fork.CrossRepository || !fork.ReviewRequested || !fork.ViewerIsReviewer || !fork.Conflicts || fork.ChecksState != "failure" || fork.MergeStatus != "dirty" {
		t.Errorf("gh fork = %+v", fork)
	}
	if asked.ReviewRequested || !asked.ViewerIsReviewer || asked.ReviewDecision != "CHANGES_REQUESTED" || asked.BaseRefName != "release" ||
		asked.ViewerReview != (gitops.PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-03T00:00:00Z"}) || asked.ChecksState != "" {
		t.Errorf("gh changes requested = %+v", asked)
	}

	gl, err := r.core.OriginRepository(t.Context(), checkout("https://gitlab.com/grp/lists.git"))
	if err != nil {
		t.Fatalf("OriginRepository: %v", err)
	}
	mrs, err := r.core.ListProjectPRs(t.Context(), gl)
	if err != nil || len(mrs) != 3 {
		t.Fatalf("glab ListProjectPRs = %+v, %v", mrs, err)
	}
	wanted, mrFork, mrMine := mrs[0], mrs[1], mrs[2]
	if mrMine.Ref.Number != 7 || !mrMine.Draft || !mrMine.ViewerIsAuthor || mrMine.ViewerIsReviewer || mrMine.HeadSHA != strings.Repeat("7", 40) ||
		mrMine.UpdatedAt != "2026-10-05T00:00:00Z" || mrMine.MergeStatus != "draft_status" || mrMine.NodeID != "" || mrMine.Commits != 0 {
		t.Errorf("glab own draft = %+v", mrMine)
	}
	if !mrFork.CrossRepository || !mrFork.ViewerIsReviewer || !mrFork.ReviewRequested || !mrFork.Conflicts || !mrFork.UnresolvedBlocking || mrFork.Comments != 1 {
		t.Errorf("glab fork = %+v", mrFork)
	}
	if wanted.ReviewDecision != "REVIEW_REQUIRED" || wanted.MergeStatus != "not_approved" || wanted.ViewerIsReviewer {
		t.Errorf("glab approval wanted = %+v", wanted)
	}

	routes := expectHandled(t, r)
	count := func(route string) int {
		n := 0
		for _, got := range routes {
			if got == route {
				n++
			}
		}
		return n
	}
	if count("gh graphql PullsSearch") != 1 || count("glab api merge request list") != 1 {
		t.Fatalf("routes = %v, want one list read per project", routes)
	}
}

// The list row's GitLab summary reads the viewer's own review state from
// the merge request's reviewers, so another reviewer's change request is
// not the viewer's.
func TestGitLabSummaryReadsTheViewersOwnReview(t *testing.T) {
	r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{
		{Forge: "gitlab", Project: "grp/reviews", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Bob asked for changes", Author: "carol", Reviewers: []string{"alice", "bob"}, ChangeRequests: []string{"bob"}},
			{Number: 2, Title: "Alice asked for changes", Author: "carol", Reviewers: []string{"alice"}, ChangeRequests: []string{"alice"}},
			{Number: 3, Title: "Alice approved", Author: "carol", Reviewers: []string{"alice", "bob"}, ChangeRequests: []string{"bob"},
				Reviews: []forgefake.Review{{Author: "alice", State: "APPROVED"}}},
			{Number: 4, Title: "Not asked", Author: "carol", Reviewers: []string{"bob"}},
			// Alice is past the reviewers' first page of 100.
			{Number: 5, Title: "Alice asked for changes, 101st", Author: "carol", Reviewers: append(otherReviewers(100), "alice"), ChangeRequests: []string{"alice"}},
			{Number: 6, Title: "Not asked, two pages", Author: "carol", Reviewers: otherReviewers(101)},
		}},
	}})
	for number, want := range map[int]string{1: "unreviewed", 2: "requested_changes", 3: "approved", 4: "", 5: "requested_changes", 6: ""} {
		ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "reviews", Number: number}
		detail, err := r.core.ReadPRSummary(t.Context(), ref)
		if err != nil {
			t.Fatalf("!%d ReadPRSummary: %v", number, err)
		}
		if detail.ViewerReviewState != want {
			t.Errorf("!%d viewer review = %q, want %q", number, detail.ViewerReviewState, want)
		}
	}
	reviewerPages := 0
	for _, route := range expectHandled(t, r) {
		if route == "glab api reviewers" {
			reviewerPages++
		}
	}
	// One page for each of !1..!4, two for !5 and !6.
	if reviewerPages != 8 {
		t.Errorf("reviewers pages read = %d, want 8", reviewerPages)
	}
}

// otherReviewers is n reviewers other than the viewer.
func otherReviewers(n int) []string {
	reviewers := make([]string, n)
	for i := range reviewers {
		reviewers[i] = fmt.Sprintf("reviewer-%03d", i)
	}
	return reviewers
}
