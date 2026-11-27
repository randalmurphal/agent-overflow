package app

import (
	"slices"
	"testing"

	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/harness/trackerfake"
)

// workBranches are the branches Work lists, as project and name.
func workBranches(work TicketWork) []string {
	var out []string
	for _, b := range work.Branches {
		out = append(out, b.ProjectID+" "+b.Name)
	}
	return out
}

// A merge request only Jira links folds the branch of its name in the
// project that clones its repository, and leaves another repository's
// branch of that name in Work, with its Start work; the row opens in the
// project that clones its repository.
func TestSearchTicketWorkFoldsAJiraRowsBranchOnlyInTheProjectThatClonesItsRepository(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixtureOn(t, trackerBoardSetup{site: devSite(t,
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/ops!4", Name: "WEB-1 Rework the cache", URL: "https://gitlab.com/example/ops/-/merge_requests/4",
			Status: "MERGED", Branch: "WEB-1-cache"},
	)}, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}, {Forge: "gitlab", Project: "example/ops"}}})
	runGit(t, f.dir, "branch", "WEB-1-cache")
	ops := forgeProjectRepo(t, f.a, "https://gitlab.com/example/ops.git")
	opsProject, err := f.a.store.GetProject(ops)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, opsProject.Path, "branch", "WEB-1-cache")
	f.linkThread(t, "t-ops", ops, opsProject.Path)

	web, opsBranch := f.project+" WEB-1-cache", ops+" WEB-1-cache"
	stored, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workBranches(stored); !slices.Contains(got, web) || !slices.Contains(got, opsBranch) {
		t.Fatalf("the stored answer's branches %v; want both projects' WEB-1-cache", got)
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workBranches(work); !slices.Contains(got, web) || slices.Contains(got, opsBranch) {
		t.Errorf("branches %v after the search; want %q beside the merge request and %q folded into it", got, web, opsBranch)
	}
	at := slices.IndexFunc(work.PullRequests, func(pr TicketPullRequest) bool { return pr.URL == "https://gitlab.com/example/ops/-/merge_requests/4" })
	if at < 0 {
		t.Fatalf("pull requests %+v; want Jira's merge request", work.PullRequests)
	}
	if pr := work.PullRequests[at]; pr.ReviewProjectID != ops || pr.ProjectID != "" {
		t.Errorf("Jira's merge request is listed under %q and opens in %q; want no project and %q", pr.ProjectID, pr.ReviewProjectID, ops)
	}
}

// A pull request only Jira links opens in a project of the computer that
// clones its repository though Work covers no such project, and on the
// forge when none clones it.
func TestSearchTicketWorkOpensAJiraRowInAnyProjectThatClonesItsRepository(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixtureOn(t, trackerBoardSetup{site: devSite(t,
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/api!7", Name: "Rework the API", URL: "https://gitlab.com/example/api/-/merge_requests/7", Status: "OPEN"},
		trackerfake.JiraPullRequest{InstanceType: devInstance, ID: "example/docs!8", Name: "Document it", URL: "https://gitlab.com/example/docs/-/merge_requests/8", Status: "OPEN"},
	)}, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	api := forgeProjectRepo(t, f.a, "https://gitlab.com/example/api.git")
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	opens := map[string]string{}
	for _, pr := range work.PullRequests {
		opens[pr.URL] = pr.ReviewProjectID
	}
	want := map[string]string{"https://gitlab.com/example/api/-/merge_requests/7": api, "https://gitlab.com/example/docs/-/merge_requests/8": ""}
	for url, project := range want {
		if got, ok := opens[url]; !ok || got != project {
			t.Errorf("%s opens in %q (listed %v); want %q", url, got, ok, project)
		}
	}
}
