package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

var workSearchRepo = gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}

// workSearchFixture is a ticket (WEB-1, issue 10101) whose board the
// fixture's project lists, that project a checkout of example/web on the
// fake forge, and the fake forge serving it.
type workSearchFixture struct {
	*trackerBoardFixture
	ctx    context.Context
	engine *forgefake.Engine
	dir    string
}

func newWorkSearchFixture(t *testing.T, fixture forgefake.Fixture) workSearchFixture {
	t.Helper()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") })
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, f.a, engine)
	t.Cleanup(func() {
		for _, inv := range engine.Invocations(0).Invocations {
			if inv.Unhandled {
				t.Errorf("unhandled forge call %s %s: %s", inv.Method, inv.Path, inv.Detail)
			}
		}
	})
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
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example/web.git")
	return workSearchFixture{trackerBoardFixture: f, ctx: ctx, engine: engine, dir: dir}
}

// forgeOps are the forge requests the engine served after the one
// numbered from, by GraphQL operation or REST route, and the number of
// the last.
func (f workSearchFixture) forgeOps(from int) ([]string, int) {
	var ops []string
	last := from
	for _, inv := range f.engine.Invocations(from).Invocations {
		ops = append(ops, inv.Route)
		last = inv.Seq
	}
	return ops, last
}

func (f workSearchFixture) linkThread(t *testing.T, id, projectID, dir string) {
	t.Helper()
	if err := f.a.store.CreateThread(store.Thread{ID: id, ProjectID: projectID, Title: "Thread", Provider: "claude", Model: "test-model",
		WorkspacePath: dir, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.SetThreadTicket(f.ctx, id, ThreadTicketRef{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", IssueID: "10101"}); err != nil {
		t.Fatal(err)
	}
}

func workSearchPR(number int) gitops.PRReference {
	return gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web", Number: number}
}

// SearchTicketWork lists, beside what GetTicketWork lists, the pull
// requests the forge finds for the key: by title, by a branch that begins
// with it, and by a branch of the ticket's that names it later in its
// name. A pull request listed already is listed once: the stored row
// stays while the forge answers its state, and a linked thread's record,
// or a stored row whose state the forge answers otherwise, gives way to
// the forge's answer. A found pull request that does not name the key
// (WEB-12, "web 1", the key in its body) is not listed, and a branch a
// listed pull request names is folded into it. GetTicketWork asks the
// forge nothing.
func TestSearchTicketWorkListsWhatTheForgeFindsByTheKey(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User 1"},
		{Number: 2, Title: "Cache the session", HeadRef: "WEB-1-cache", State: "merged", Author: "user1"},
		{Number: 3, Title: "Move the host", HeadRef: "feature/WEB-1-host", State: "closed"},
		{Number: 4, Title: "WEB-12 Other", HeadRef: "other"},
		{Number: 5, Title: "Move web 1 to the new host", HeadRef: "move"},
		{Number: 6, Title: "Unrelated", Body: "Follows WEB-1.", HeadRef: "unrelated"},
		{Number: 7, Title: "WEB-1 Old cursor", HeadRef: "old-cursor", State: "merged", Author: "user1", AuthorName: "User 1"},
		{Number: 8, Title: "WEB-1 Docs", HeadRef: "docs", State: "merged", Author: "user1", AuthorName: "User 1"},
	}}}})
	for _, branch := range []string{"WEB-1-cache", "feature/WEB-1-host", "web-1-new"} {
		runGit(t, f.dir, "branch", branch)
	}
	// The stored list holds 1 open, as the forge does, and 7 open, which
	// the forge says merged since.
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.Author.Login, r.Author.Name, r.PipelineStatus = "WEB-1 Fix the login", "fix-login", "user1", "User 1", "success"
		}),
		prListRow(workSearchRepo, 7, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.PipelineStatus = "WEB-1 Old cursor", "old-cursor", "failed"
		}),
	}, time.Minute)
	// A linked thread was last seen with 8, open.
	f.linkThread(t, "t1", f.project, f.dir)
	if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: "t1", Forge: "github", Host: "github.com",
		Project: "example/web", Number: 8, URL: "https://github.com/example/web/pull/8", Branch: "docs", WorktreePath: f.dir, SeenOpenAt: 1}); err != nil {
		t.Fatal(err)
	}

	stored, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.forgeOps(0); len(ops) != 0 {
		t.Fatalf("GetTicketWork asked the forge %v", ops)
	}
	if len(stored.PullRequests) != 3 || len(stored.Branches) != 3 || len(stored.ForgeProblems) != 0 || len(stored.SearchFull) != 0 {
		t.Fatalf("GetTicketWork = %+v; want the 3 stored and recorded pull requests and the 3 branches", stored)
	}

	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	url := func(n int) string { return "https://github.com/example/web/pull/" + strconv.Itoa(n) }
	want := []TicketPullRequest{
		{Ref: workSearchPR(7), URL: url(7), Title: "WEB-1 Old cursor", Author: "User 1", State: TicketPRMerged, Branch: "old-cursor", ProjectID: f.project},
		{Ref: workSearchPR(1), Title: "WEB-1 Fix the login", Author: "User 1", State: TicketPROpen, CI: "success", Branch: "fix-login", ProjectID: f.project},
		{Ref: workSearchPR(8), URL: url(8), Title: "WEB-1 Docs", Author: "User 1", State: TicketPRMerged, Branch: "docs", ProjectID: f.project},
		{Ref: workSearchPR(2), URL: url(2), Title: "Cache the session", Author: "user1", State: TicketPRMerged, Branch: "WEB-1-cache", ProjectID: f.project},
		{Ref: workSearchPR(3), URL: url(3), Title: "Move the host", Author: "ao-author", State: TicketPRClosed, Branch: "feature/WEB-1-host", ProjectID: f.project},
	}
	if !slices.Equal(work.PullRequests, want) {
		got, _ := json.MarshalIndent(work.PullRequests, "", " ")
		t.Errorf("pull requests = %s\nwant %+v", got, want)
	}
	var branches []string
	for _, b := range work.Branches {
		branches = append(branches, b.Name)
	}
	if !slices.Equal(branches, []string{"web-1-new"}) || len(work.ForgeProblems) != 0 || len(work.SearchFull) != 0 {
		t.Errorf("branches = %v, problems %+v, full %v; want web-1-new alone, the rest folded into their pull requests", branches, work.ForgeProblems, work.SearchFull)
	}
	// One key search, then a lookup of each branch no pull request names
	// by then: WEB-1-cache's pull request the head search found.
	if ops, _ := f.forgeOps(0); !slices.Equal(ops, []string{"gh graphql PullsByKey", "gh graphql PRsByHead", "gh graphql PRsByHead"}) {
		t.Errorf("forge requests = %v; want one key search and the lookups of feature/WEB-1-host and web-1-new", ops)
	}
}

// Work searches each repository once however many of its projects clone
// it, GitHub's and GitLab's alike, and a project whose origin names no
// forge repository is not searched.
func TestSearchTicketWorkSearchesEachRepositoryOnce(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login"}}},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{{Number: 2, Title: "WEB-1 Fix the API", HeadRef: "fix-api", State: "merged"}}},
	}})
	second := forgeProjectRepo(t, f.a, "https://github.com/example/web.git")
	gitlab := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/web.git")
	local := forgeProjectRepo(t, f.a, "")
	for i, project := range []string{second, gitlab, local} {
		p, err := f.a.store.GetProject(project)
		if err != nil {
			t.Fatal(err)
		}
		f.linkThread(t, fmt.Sprintf("t%d", i), project, p.Path)
	}
	_, mark := f.forgeOps(0)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, pr := range work.PullRequests {
		found = append(found, pr.Ref.Key()+" "+pr.State+" "+pr.ProjectID)
	}
	if !slices.Equal(found, []string{"github:example/web:1 open " + f.project, "gitlab:grp/web:2 merged " + gitlab}) || len(work.ForgeProblems) != 0 {
		t.Errorf("pull requests = %v, problems %+v; want 1 under the first project that clones example/web, and 2", found, work.ForgeProblems)
	}
	ops, _ := f.forgeOps(mark)
	if slices.Index(ops, "gh graphql PullsByKey") < 0 || slices.Index(ops, "glab api merge request list") < 0 || len(ops) != 2 {
		t.Errorf("forge requests = %v; want one search of each repository", ops)
	}
}

// A repository whose search fails is a ForgeProblem of its kind, in the
// terms the Pull requests page shows a failed read in, and the rest of
// the answer stands: the stored rows, the other repository's rows. A
// rate limit carries when the forge takes requests again.
func TestSearchTicketWorkSaysAFailedSearchBesideTheRest(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login"}}},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{{Number: 2, Title: "WEB-1 Fix the API", HeadRef: "fix-api", State: "merged"}}},
		{Forge: "gitlab", Project: "grp/docs", Pulls: []forgefake.Pull{{Number: 3, Title: "WEB-1 Docs", HeadRef: "docs", State: "merged"}}},
	}})
	gitlab := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/web.git")
	docs := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/docs.git")
	for i, project := range []string{gitlab, docs} {
		p, err := f.a.store.GetProject(project)
		if err != nil {
			t.Fatal(err)
		}
		f.linkThread(t, fmt.Sprintf("t%d", i), project, p.Path)
	}
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "WEB-1 Fix the login", "fix-login" }),
	}, time.Minute)
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullsByKey", Status: 403,
		Header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Resource": "graphql"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "glab api merge request list", Status: 403, Times: 1,
		Body: json.RawMessage(`{"message":"403 Forbidden"}`)}); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.ForgeProblems) != 2 {
		t.Fatalf("problems = %+v; want the GitHub repository's and one GitLab repository's", work.ForgeProblems)
	}
	byRepo := map[string]TicketForgeProblem{}
	for _, p := range work.ForgeProblems {
		byRepo[p.Repo.Key()] = p
	}
	limited := byRepo["github:example/web"]
	if resume, err := time.Parse(time.RFC3339, limited.ResumeAt); limited.ErrorKind != forgeFailureRateLimited || limited.ProjectID != f.project ||
		err != nil || !resume.After(time.Now()) || limited.ErrorDetails == nil {
		t.Errorf("rate limited = %+v; want a resume time to come", limited)
	}
	var refused TicketForgeProblem
	for key, p := range byRepo {
		if key != "github:example/web" {
			refused = p
		}
	}
	if refused.ErrorKind != forgeFailureForbidden || refused.Error == "" {
		t.Errorf("refused = %+v", refused)
	}
	var listed []string
	for _, pr := range work.PullRequests {
		listed = append(listed, pr.Ref.Key())
	}
	other := map[string]string{"gitlab:grp/web": "gitlab:grp/docs:3", "gitlab:grp/docs": "gitlab:grp/web:2"}[refused.Repo.Key()]
	if !slices.Equal(listed, []string{"github:example/web:1", other}) {
		t.Errorf("pull requests = %v; want the stored one and the one the other GitLab search found (%s)", listed, other)
	}
}
