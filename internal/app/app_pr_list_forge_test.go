package app

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/testutil"
)

// useForgefake serves engine over HTTP as the app's isolated forge
// transport.
func useForgefake(t *testing.T, app *App, engine *forgefake.Engine) {
	t.Helper()
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: srv.URL, Token: mergeForgeToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	app.git = gitops.NewCore(gitops.WithForgeAPI(svc))
}

// forgeProjectRepo is a checkout registered as a project whose origin is
// originURL ("" for none).
func forgeProjectRepo(t *testing.T, app *App, originURL string) string {
	t.Helper()
	repo := initMainGitRepo(t)
	_ = testutil.RunGitAllowError(repo, "remote", "remove", "origin")
	if originURL != "" {
		testutil.RunGit(t, repo, "remote", "add", "origin", originURL)
	}
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	return project.ID
}

// The list pump against the fake forge on both forges: the projects come
// from the store and their origins, the rows from each forge's list, and
// the detail read of the user's own and reviewed rows adds approvals,
// unresolved threads and the pipeline, which moves the attention state.
func TestPRListReadsBothForgesThroughTheirAPIs(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	line := 1
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Mine, approved", Author: "ao-viewer", Reviews: []forgefake.Review{{Author: "alice", AuthorName: "Alice", State: "APPROVED"}},
				Threads: []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Body: "nit"}}}}},
			{Number: 2, Title: "Asked of me", Author: "carol", Reviewers: []string{"ao-viewer"}},
			{Number: 3, Title: "Mine, red", Author: "ao-viewer", CI: &forgefake.Pipeline{Jobs: []forgefake.Job{{Name: "unit", Status: "failed"}}}},
		}},
		{Forge: "gitlab", Project: "team/app", Pulls: []forgefake.Pull{
			{Number: 7, Title: "Mine, red", Author: "ao-viewer", Readiness: &forgefake.Readiness{RequiredApprovals: 1},
				Reviews: []forgefake.Review{{Author: "bob", AuthorName: "Bob", State: "APPROVED"}},
				Threads: []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Body: "fix"}}}},
				CI:      &forgefake.Pipeline{Jobs: []forgefake.Job{{Name: "unit", Status: "failed"}}}},
			{Number: 8, Title: "Approved by me", Author: "carol", Reviewers: []string{"ao-viewer"},
				Reviews: []forgefake.Review{{Author: "ao-viewer", AuthorName: "Viewer", State: "APPROVED"}}},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	github := forgeProjectRepo(t, app, "https://github.com/acme/widgets.git")
	gitlab := forgeProjectRepo(t, app, "https://gitlab.com/team/app.git")
	forgeProjectRepo(t, app, "") // no origin: not a forge project

	projects, err := app.ListForgeProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 {
		t.Fatalf("forge projects = %+v, want the two with forge origins", projects)
	}
	for _, project := range projects {
		if project.Error != "" || project.Repo.Validate() != nil {
			t.Fatalf("project = %+v", project)
		}
	}

	t.Cleanup(app.prUpdates.wg.Wait)
	sub := subscribePRList(t, app)
	snapshot := sub.Snapshot
	gh := prListProject(t, snapshot, github)
	if gh.Error != "" || len(gh.Rows) != 3 {
		t.Fatalf("github project = %+v", gh)
	}
	states := map[int]string{}
	for _, row := range gh.Rows {
		states[row.Ref.Number] = row.State
	}
	if states[1] != PRStateReady || states[2] != PRStateYourTurn || states[3] != PRStatePipelineFailed {
		t.Fatalf("github states = %v", states)
	}
	gl := prListProject(t, snapshot, gitlab)
	if gl.Error != "" || len(gl.Rows) != 2 {
		t.Fatalf("gitlab project = %+v", gl)
	}
	// Every row is the user's own or one they review, so the first tick
	// read each detail: GitLab's list alone has no pipeline and cannot say
	// the viewer approved.
	for _, project := range snapshot.Projects {
		for _, row := range project.Rows {
			if row.Detail == nil {
				t.Fatalf("%s has no detail (%s)", row.Ref.Key(), row.DetailError)
			}
		}
	}
	for _, row := range prListProject(t, snapshot, gitlab).Rows {
		switch row.Ref.Number {
		case 7:
			if row.State != PRStatePipelineFailed || row.Detail.ApprovalsGiven != 1 || row.Detail.ApprovalsRequired != 1 || row.Detail.Unresolved != 1 {
				t.Fatalf("MR 7 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
			}
		case 8:
			if row.State != PRStateOpen || !row.Detail.ViewerApproved {
				t.Fatalf("MR 8 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
			}
		}
	}
	for _, row := range prListProject(t, snapshot, github).Rows {
		if row.Ref.Number == 1 && (row.State != PRStateReady || row.Detail.ApprovalsGiven != 1 || row.Detail.Unresolved != 1) {
			t.Fatalf("PR 1 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
		}
	}
}
