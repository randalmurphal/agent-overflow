package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// A ticket's Work lists the stored open pull requests whose title or head
// branch names its key, the pull requests its linked threads were last
// seen with (a merged one too), and the branches of the projects that list
// its connection's boards whose name names the key, with a branch a listed
// pull request names folded into that pull request; WEB-12 is not WEB-1.
func TestTicketWorkListsThePullRequestsAndBranchesThatNameItsKey(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") })

	project, err := f.a.store.GetProject(f.project)
	if err != nil {
		t.Fatal(err)
	}
	dir := project.Path
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, dir, ".gitkeep", "")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	for _, branch := range []string{"web-1-retry", "WEB-12-other", "feature/WEB-1-ui"} {
		runGit(t, dir, "branch", branch)
	}
	worktree := filepath.Join(t.TempDir(), "retry")
	runGit(t, dir, "worktree", "add", worktree, "web-1-retry")

	storePRList(t, f.a, f.project, prListRepoA, []gitops.PRListRow{
		prListRow(prListRepoA, 5, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.Author.Login, r.Author.Name, r.PipelineStatus = "Retry cursor", "feature/WEB-1-ui", "user1", "User 1", "success"
		}),
		prListRow(prListRepoA, 6, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "WEB-12 other", "other" }),
	}, time.Minute)
	storePRList(t, f.a, "p-b", prListRepoB, []gitops.PRListRow{prListRow(prListRepoB, 7, nil)}, time.Minute)

	if err := f.a.store.CreateThread(store.Thread{ID: "t1", ProjectID: f.project, Title: "Thread", Provider: "claude", Model: "test-model",
		WorkspacePath: dir, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.SetThreadTicket(ctx, "t1", ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "10101"}); err != nil {
		t.Fatal(err)
	}
	merged := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 9}
	if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: "t1", Forge: merged.Forge, Host: merged.Host,
		Project: merged.Project(), Number: merged.Number, URL: "https://github.com/acme/widgets/pull/9", Branch: "web-1-first",
		WorktreePath: dir, SeenOpenAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.store.MarkThreadPullRequestsMerged(merged.Forge, merged.Host, merged.Project(), merged.Number, 2); err != nil {
		t.Fatal(err)
	}

	work, err := f.a.GetTicketWork(ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if work.Key != "WEB-1" || len(work.Problems) != 0 {
		t.Fatalf("work = %+v; want WEB-1's, with no problem", work)
	}
	wantPRs := []TicketPullRequest{
		{Ref: prListRow(prListRepoA, 5, nil).Ref, Title: "Retry cursor", Author: "User 1", State: TicketPROpen, CI: "success",
			Branch: "feature/WEB-1-ui", ProjectID: f.project},
		{Ref: merged, URL: "https://github.com/acme/widgets/pull/9", State: TicketPRMerged, Branch: "web-1-first"},
	}
	if !slices.Equal(work.PullRequests, wantPRs) {
		t.Errorf("pull requests = %+v\nwant %+v", work.PullRequests, wantPRs)
	}
	var names []string
	for _, b := range work.Branches {
		names = append(names, b.Name)
		if b.ProjectID != f.project {
			t.Errorf("branch %s of project %q; want %q", b.Name, b.ProjectID, f.project)
		}
		if b.Name == "web-1-retry" && !strings.HasSuffix(b.WorktreePath, "retry") {
			t.Errorf("web-1-retry is checked out at %q; want the worktree", b.WorktreePath)
		}
	}
	if !slices.Equal(names, []string{"web-1-retry"}) {
		t.Errorf("branches = %v; want web-1-retry alone", names)
	}

	if _, err := f.a.GetTicketWork(ctx, f.conn.ID, "99999"); err == nil || !strings.Contains(err.Error(), "has not read the ticket") {
		t.Errorf("Work of a ticket the mirror does not hold = %v; want the ticket not read", err)
	}
}

// A stored list of a project that was removed, kept until the Pull
// requests page's next read prunes it, is not Work's: its pull requests
// are not listed and its project is not read.
func TestTicketWorkLeavesOutTheStoredListOfARemovedProject(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") })
	storePRList(t, f.a, "p-removed", prListRepoB, []gitops.PRListRow{
		prListRow(prListRepoB, 7, func(r *gitops.PRListRow) { r.Title = "WEB-1 Retry cursor" }),
	}, time.Minute)

	work, err := f.a.GetTicketWork(ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.PullRequests) != 0 || slices.ContainsFunc(work.Problems, func(p TicketWorkProblem) bool { return p.ProjectID == "p-removed" }) {
		t.Errorf("pull requests = %+v, problems %+v; want neither of the removed project", work.PullRequests, work.Problems)
	}
}
