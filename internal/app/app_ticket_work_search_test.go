package app

import (
	"context"
	"encoding/json"
	"errors"
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
// name. A pull request listed already, or found by both searches, is
// listed once, as the forge answers it now, with the stored list's CI. A found pull request that does not name the key (WEB-12,
// "web 1", the key in its body) is not listed, and a branch a listed
// pull request names is folded into it. GetTicketWork asks the forge
// nothing.
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
		// Found by both searches.
		{Number: 9, Title: "WEB-1 Both", HeadRef: "WEB-1-both", Draft: true, Author: "user1"},
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
		{Ref: workSearchPR(7), URL: url(7), Title: "WEB-1 Old cursor", Author: "User 1", State: TicketPRMerged, CI: "failed", Branch: "old-cursor", ProjectID: f.project},
		{Ref: workSearchPR(1), URL: url(1), Title: "WEB-1 Fix the login", Author: "User 1", State: TicketPROpen, CI: "success", Branch: "fix-login", ProjectID: f.project},
		{Ref: workSearchPR(8), URL: url(8), Title: "WEB-1 Docs", Author: "User 1", State: TicketPRMerged, Branch: "docs", ProjectID: f.project},
		{Ref: workSearchPR(9), URL: url(9), Title: "WEB-1 Both", Author: "user1", State: TicketPRDraft, Branch: "WEB-1-both", ProjectID: f.project},
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
	// One key search, then one lookup of each branch of the ticket, here
	// (WEB-1-cache, feature/WEB-1-host, web-1-new) or on the forge
	// (WEB-1-both; the forge's WEB-1-cache and feature/WEB-1-host are the
	// same branches).
	lookups := slices.Repeat([]string{"gh graphql PRsByHead"}, 4)
	if ops, _ := f.forgeOps(0); !slices.Equal(ops, append([]string{"gh graphql PullsByKey"}, lookups...)) {
		t.Errorf("forge requests = %v; want one key search and 4 lookups", ops)
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
	slices.Sort(ops)
	if !slices.Equal(ops, []string{"gh graphql PullsByKey", "glab api branch list", "glab api merge request list"}) {
		t.Errorf("forge requests = %v; want one search of each repository, GitLab's of its titles and its branches", ops)
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

// workPulls is each pull request's key and state, as Work lists them.
func workPulls(work TicketWork) []string {
	var out []string
	for _, pr := range work.PullRequests {
		out = append(out, pr.Ref.Key()+" "+pr.State)
	}
	return out
}

// A pull request whose key is only in its branch name, at the start or
// later, in any case, is found though this computer has no such branch,
// on both forges, while its branch is on the forge. One whose branch the
// forge deleted is not: the forges cannot search a deleted branch's name
// but by its start (GitHub's head search).
func TestSearchTicketWorkFindsBranchOnlyPullRequestsThisComputerLacks(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Fix the cursor", HeadRef: "feature/WEB-1-cursor"},
			{Number: 2, Title: "Fix the scroll", HeadRef: "fix/web-1-scroll", State: "merged"},
			{Number: 3, Title: "Start the docs", HeadRef: "WEB-1-docs", State: "merged"},
			{Number: 4, Title: "Gone", HeadRef: "feature/WEB-1-gone", State: "merged"},
		}, DeletedBranches: []string{"WEB-1-docs", "feature/WEB-1-gone"}},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{
			{Number: 5, Title: "Fix the API", HeadRef: "WEB-1-api", State: "merged"},
			{Number: 6, Title: "Fix the client", HeadRef: "feature/web-1-client", State: "closed"},
			{Number: 7, Title: "Gone", HeadRef: "feature/WEB-1-gone", State: "merged"},
		}, DeletedBranches: []string{"feature/WEB-1-gone"}},
	}})
	gitlab := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/web.git")
	p, err := f.a.store.GetProject(gitlab)
	if err != nil {
		t.Fatal(err)
	}
	f.linkThread(t, "t-gitlab", gitlab, p.Path)

	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	got := workPulls(work)
	slices.Sort(got)
	want := []string{"github:example/web:1 open", "github:example/web:2 merged", "github:example/web:3 merged",
		"gitlab:grp/web:5 merged", "gitlab:grp/web:6 closed"}
	if !slices.Equal(got, want) || len(work.ForgeProblems) != 0 {
		t.Errorf("pull requests = %v, problems %+v\nwant %v", got, work.ForgeProblems, want)
	}
}

// Every branch of the ticket is looked up whatever Work lists already: a
// stored open pull request does not hide the closed one from the same
// branch, nor a linked thread's record a pull request of another
// repository from a branch of the same name.
func TestSearchTicketWorkLooksUpEveryBranchWhateverIsListed(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
			{Number: 1, Title: "First try", HeadRef: "feature/WEB-1-cursor", State: "closed"},
			{Number: 2, Title: "Second try", HeadRef: "feature/WEB-1-cursor"},
			{Number: 5, Title: "Into the release", HeadRef: "feature/WEB-1-cursor", BaseRef: "release"},
		}, DeletedBranches: []string{"feature/WEB-1-cursor"}},
		{Forge: "github", Project: "example/api", Pulls: []forgefake.Pull{
			{Number: 3, Title: "API side", HeadRef: "feature/WEB-1-cursor", State: "merged"},
		}, DeletedBranches: []string{"feature/WEB-1-cursor"}},
	}})
	runGit(t, f.dir, "branch", "feature/WEB-1-cursor")
	api := forgeProjectRepo(t, f.a, "https://github.com/example/api.git")
	p, err := f.a.store.GetProject(api)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, p.Path, "branch", "feature/WEB-1-cursor")
	f.linkThread(t, "t-api", api, p.Path)
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 2, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "Second try", "feature/WEB-1-cursor" }),
	}, time.Minute)
	// The linked thread was last seen with 5, a record of no project.
	if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: "t-api", Forge: "github", Host: "github.com",
		Project: "example/web", Number: 5, URL: "https://github.com/example/web/pull/5", Branch: "feature/WEB-1-cursor", WorktreePath: p.Path, SeenOpenAt: 1}); err != nil {
		t.Fatal(err)
	}

	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	got := workPulls(work)
	slices.Sort(got)
	want := []string{"github:example/api:3 merged", "github:example/web:1 closed", "github:example/web:2 open", "github:example/web:5 open"}
	if !slices.Equal(got, want) || len(work.ForgeProblems) != 0 {
		t.Errorf("pull requests = %v, problems %+v\nwant %v", got, work.ForgeProblems, want)
	}
}

// A GitLab fork's branch is looked up in the fork and in the project it
// was forked from, both: the fork's own old merge request does not hide
// the one opened into the parent.
func TestSearchTicketWorkReadsAForksBranchInBothProjects(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "gitlab", Project: "grp/up", Pulls: []forgefake.Pull{
			{Number: 2, Title: "Fix it", HeadRef: "WEB-1", HeadRepo: "alice/up"},
		}},
		{Forge: "gitlab", Project: "alice/up", Parent: "grp/up", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Try it", HeadRef: "WEB-1", State: "closed"},
		}},
	}})
	fork := forgeProjectRepo(t, f.a, "https://gitlab.com/alice/up.git")
	p, err := f.a.store.GetProject(fork)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, p.Path, "branch", "WEB-1")
	f.linkThread(t, "t-fork", fork, p.Path)

	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	got := workPulls(work)
	slices.Sort(got)
	if want := []string{"gitlab:alice/up:1 closed", "gitlab:grp/up:2 open"}; !slices.Equal(got, want) || len(work.ForgeProblems) != 0 {
		t.Errorf("pull requests = %v, problems %+v\nwant %v", got, work.ForgeProblems, want)
	}
}

// No project's local read starts once the method's context has ended:
// GetTicketWork, whose read SearchTicketWork shares, stops.
func TestTicketWorkStartsNoProjectReadAfterItsContextEnded(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.a.GetTicketWork(ctx, f.conn.ID, "10101"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetTicketWork with its context ended = %v; want it stopped", err)
	}
}

// A GitHub fork's ticket branch is looked up in the project it was forked
// from too: the fork's head search, which found nothing, does not stand
// for the parent's pull requests.
func TestSearchTicketWorkLooksUpAGitHubForksBranchInItsParent(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "team/up", Pulls: []forgefake.Pull{
			{Number: 2, Title: "Fix login", HeadRef: "WEB-1-login", HeadRepo: "alice/up"},
		}},
		{Forge: "github", Project: "alice/up", Parent: "team/up"},
	}})
	fork := forgeProjectRepo(t, f.a, "https://github.com/alice/up.git")
	p, err := f.a.store.GetProject(fork)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, p.Path, "branch", "WEB-1-login")
	f.linkThread(t, "t-fork", fork, p.Path)

	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"github:team/up:2 open"}) || len(work.ForgeProblems) != 0 {
		t.Errorf("pull requests = %v, problems %+v; want team/up's 2", got, work.ForgeProblems)
	}
}

// What a search read before one of its requests failed is listed beside
// the failure: GitLab's title search answered, its branch search did not.
func TestSearchTicketWorkListsWhatASearchReadBeforeItFailed(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{{Number: 1, Title: "WEB-1 Fix the API", HeadRef: "fix-api", State: "merged"}}},
	}})
	gitlab := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/web.git")
	p, err := f.a.store.GetProject(gitlab)
	if err != nil {
		t.Fatal(err)
	}
	f.linkThread(t, "t-gl", gitlab, p.Path)
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "glab api branch list", Status: 403, Body: json.RawMessage(`{"message":"403 Forbidden"}`)}); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"gitlab:grp/web:1 merged"}) || len(work.ForgeProblems) != 1 {
		t.Errorf("pull requests = %v, problems %+v; want grp/web's 1 beside the branch search's failure", got, work.ForgeProblems)
	}
}

// A stored list keeps its rows while a project still clones its
// repository, though the project it was read under was removed.
func TestTicketWorkKeepsAStoredListWhoseProjectWasRemovedWhileAnotherClonesIt(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	removed := forgeProjectRepo(t, f.a, "https://github.com/example/web.git")
	storePRList(t, f.a, removed, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.PipelineStatus = "WEB-1 Fix the login", "fix-login", "success"
		}),
	}, time.Minute)
	if err := f.a.store.DeleteProject(removed); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.PullRequests) != 1 || work.PullRequests[0].CI != "success" || len(work.Problems) != 0 {
		t.Errorf("pull requests = %+v, problems %+v; want the stored row with its CI and no problem", work.PullRequests, work.Problems)
	}
}

// A stored row the forge finds shows what the forge names it now, its
// title, author and state, and keeps the CI only the stored list has.
func TestSearchTicketWorkShowsAStoredRowAsTheForgeNamesItNow(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		{Number: 7, Title: "WEB-1 New cursor", HeadRef: "old-cursor", State: "merged", Author: "user1", AuthorName: "User 1"},
	}}}})
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 7, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.PipelineStatus = "WEB-1 Old cursor", "old-cursor", "failed"
		}),
	}, time.Minute)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v", work.PullRequests)
	}
	if pr := work.PullRequests[0]; pr.Title != "WEB-1 New cursor" || pr.Author != "User 1" || pr.State != TicketPRMerged || pr.CI != "failed" || pr.ProjectID != f.project {
		t.Errorf("row = %+v; want the forge's title, author and state, the stored CI and project", pr)
	}
}
