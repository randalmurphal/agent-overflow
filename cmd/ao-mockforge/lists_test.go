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
			{Number: 7, Title: "Draft: mine", Author: "alice", Reviewers: []string{"bob"}, HeadSHA: strings.Repeat("7", 40), UpdatedAt: "2026-10-05T00:00:00Z",
				Readiness:   &forgefake.Readiness{RequiredApprovals: 2},
				CommittedAt: "2026-10-04T00:00:00Z", CI: &forgefake.Pipeline{Jobs: []forgefake.Job{{Name: "unit", Status: "failed"}}}},
			{Number: 8, Title: "Fork", Author: "carol", HeadRepo: "carol/lists", Reviewers: []string{"dave", "alice"}, Mergeable: "conflicts",
				Readiness: &forgefake.Readiness{RequiredApprovals: 1}, Reviews: []forgefake.Review{{Author: "dave", State: "APPROVED"}},
				Threads: []forgefake.Thread{
					{Path: "a.go", Line: intPtr(1), Comments: []forgefake.Comment{{Author: "alice", Body: "fix", CreatedAt: "2026-10-06T00:00:00Z"}}},
					{Path: "b.go", Line: intPtr(2), Resolved: true, Comments: []forgefake.Comment{{Author: "dave", Body: "ok"}}},
				}},
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
	if !fork.CrossRepository || !fork.ReviewRequested || !fork.ViewerIsReviewer || !fork.Conflicts || fork.ChecksState != "failure" || fork.MergeStatus != "dirty" ||
		fork.ViewerReviewState != "unreviewed" || fork.PipelineStatus != gitops.CIStatusFailed {
		t.Errorf("gh fork = %+v", fork)
	}
	if asked.ReviewRequested || !asked.ViewerIsReviewer || asked.ReviewDecision != "CHANGES_REQUESTED" || asked.BaseRefName != "release" ||
		asked.ViewerReview != (gitops.PRListReview{State: "CHANGES_REQUESTED", SubmittedAt: "2026-10-03T00:00:00Z"}) || asked.ChecksState != "" ||
		asked.ViewerReviewState != "requested_changes" || asked.ViewerActivityAt != "2026-10-03T00:00:00Z" ||
		*asked.ApprovalsGiven != 0 || *asked.ApprovalsRequired != 0 || *asked.Unresolved != 0 {
		t.Errorf("gh changes requested = %+v", asked)
	}
	if *mine.ApprovalsRequired != 0 || mine.PipelineStatus != gitops.CIStatusPending {
		t.Errorf("gh own draft detail = required %d ci %q", *mine.ApprovalsRequired, mine.PipelineStatus)
	}

	gl, err := r.core.OriginRepository(t.Context(), checkout("https://gitlab.com/grp/lists.git"))
	if err != nil {
		t.Fatalf("OriginRepository: %v", err)
	}
	mrs, err := r.core.ListProjectPRs(gitops.WithPRListRound(t.Context()), gl)
	if err != nil || len(mrs) != 3 {
		t.Fatalf("glab ListProjectPRs = %+v, %v", mrs, err)
	}
	wanted, mrFork, mrMine := mrs[0], mrs[1], mrs[2]
	if mrMine.Ref.Number != 7 || !mrMine.Draft || !mrMine.ViewerIsAuthor || mrMine.ViewerIsReviewer || mrMine.HeadSHA != strings.Repeat("7", 40) ||
		mrMine.UpdatedAt != "2026-10-05T00:00:00Z" || mrMine.MergeStatus != "draft_status" || mrMine.NodeID != "" || mrMine.Commits != 0 {
		t.Errorf("glab own draft = %+v", mrMine)
	}
	if mrMine.PipelineStatus != gitops.CIStatusFailed || mrMine.LastCommitAt != "2026-10-04T00:00:00Z" || *mrMine.ApprovalsGiven != 0 ||
		*mrMine.ApprovalsRequired != 2 || *mrMine.Unresolved != 0 || mrMine.ViewerReviewState != "" {
		t.Errorf("glab own draft detail = %+v", mrMine)
	}
	// The viewer commented in a thread: its reviewer state is reviewed and
	// the comment is its activity; one of two threads is open.
	if !mrFork.CrossRepository || !mrFork.ViewerIsReviewer || !mrFork.ReviewRequested || !mrFork.Conflicts || !mrFork.UnresolvedBlocking || mrFork.Comments != 2 ||
		mrFork.ViewerReviewState != "reviewed" || mrFork.ViewerActivityAt != "2026-10-06T00:00:00Z" ||
		*mrFork.ApprovalsGiven != 1 || *mrFork.ApprovalsRequired != 1 || *mrFork.Unresolved != 1 {
		t.Errorf("glab fork = %+v", mrFork)
	}
	if wanted.ReviewDecision != "REVIEW_REQUIRED" || wanted.MergeStatus != "not_approved" || wanted.ViewerIsReviewer ||
		wanted.ApprovalsGiven != nil || wanted.ApprovalsRequired != nil || wanted.Unresolved != nil {
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
	if count("gh graphql PullsSearch") != 1 || len(routes) != 10 {
		t.Fatalf("routes = %v, want one PullsSearch for GitHub and GitLab's nine reads", routes)
	}
	for _, route := range []string{"glab api user", "glab api merge request list", "glab graphql PullsReviewing", "glab graphql PullsAuthored",
		"glab graphql PullsReviewingActivity", "glab graphql PullsAuthoredActivity", "glab graphql PullsReviewingApprovals",
		"glab graphql PullsAuthoredApprovals", "glab api user events"} {
		if count(route) != 1 {
			t.Errorf("route %s read %d times, want once: %v", route, count(route), routes)
		}
	}
}

// The GitLab reviewer read names the viewer's own review state, so
// another reviewer's change request is not the viewer's, and a reviewer
// past the first page of reviewers still reads.
func TestGitLabListReadsTheViewersOwnReview(t *testing.T) {
	r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{
		{Forge: "gitlab", Project: "grp/reviews", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Bob asked for changes", Author: "carol", Reviewers: []string{"alice", "bob"}, ChangeRequests: []string{"bob"}},
			{Number: 2, Title: "Alice asked for changes", Author: "carol", Reviewers: []string{"alice"}, ChangeRequests: []string{"alice"}},
			{Number: 3, Title: "Alice approved", Author: "carol", Reviewers: []string{"alice", "bob"}, ChangeRequests: []string{"bob"},
				Reviews: []forgefake.Review{{Author: "alice", State: "APPROVED", SubmittedAt: "2026-10-07T00:00:00Z"}}},
			{Number: 4, Title: "Not asked", Author: "carol", Reviewers: []string{"bob"}},
			{Number: 5, Title: "Alice asked for changes, 101st", Author: "carol", Reviewers: append(otherReviewers(100), "alice"), ChangeRequests: []string{"alice"}},
		}},
	}})
	repo := gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "reviews"}
	rows, err := r.core.ListProjectPRs(t.Context(), repo)
	if err != nil {
		t.Fatalf("ListProjectPRs: %v", err)
	}
	want := map[int]string{1: "unreviewed", 2: "requested_changes", 3: "approved", 4: "", 5: "requested_changes"}
	for _, row := range rows {
		if row.ViewerReviewState != want[row.Ref.Number] {
			t.Errorf("!%d viewer review = %q, want %q", row.Ref.Number, row.ViewerReviewState, want[row.Ref.Number])
		}
	}
	if len(rows) != 5 || rows[2].ViewerActivityAt != "2026-10-07T00:00:00Z" {
		t.Errorf("rows = %d, !3 activity %q", len(rows), rows[2].ViewerActivityAt)
	}
	expectHandled(t, r)
}

// otherReviewers is n reviewers other than the viewer.
func otherReviewers(n int) []string {
	reviewers := make([]string, n)
	for i := range reviewers {
		reviewers[i] = fmt.Sprintf("reviewer-%03d", i)
	}
	return reviewers
}
