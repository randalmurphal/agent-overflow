package app

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			// Dave's change request is not the viewer's.
			{Number: 9, Title: "Dave asked for changes", Author: "carol", Reviewers: []string{"ao-viewer", "dave"}, ChangeRequests: []string{"dave"}},
			{Number: 10, Title: "Approved by me, Dave asked for changes", Author: "carol", Reviewers: []string{"ao-viewer", "dave"}, ChangeRequests: []string{"dave"},
				Reviews: []forgefake.Review{{Author: "ao-viewer", State: "APPROVED"}}},
			{Number: 11, Title: "I asked for changes", Author: "carol", Reviewers: []string{"ao-viewer"}, ChangeRequests: []string{"ao-viewer"}},
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
	// The fixture's default project points at a directory that does not
	// exist: git cannot read it, so it is listed with that failure.
	if len(projects) != 3 {
		t.Fatalf("forge projects = %+v, want the two with forge origins and the unreadable one", projects)
	}
	for _, project := range projects {
		unreadable := project.ProjectID == "default-test-project"
		if unreadable != (project.Error != "") || (!unreadable && project.Repo.Validate() != nil) {
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
	if gl.Error != "" || len(gl.Rows) != 5 {
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
		case 9:
			if row.State != PRStateYourTurn || row.ReviewDecision != "CHANGES_REQUESTED" || row.Detail.ViewerReviewState != "unreviewed" {
				t.Fatalf("MR 9 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
			}
		case 10:
			if row.State != PRStateOpen || row.Detail.ViewerReviewState != "approved" {
				t.Fatalf("MR 10 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
			}
		case 11:
			if row.State != PRStateWaitingOnAuthor || row.Detail.ViewerReviewState != "requested_changes" {
				t.Fatalf("MR 11 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
			}
		}
	}
	for _, row := range prListProject(t, snapshot, github).Rows {
		if row.Ref.Number == 1 && (row.State != PRStateReady || row.Detail.ApprovalsGiven != 1 || row.Detail.Unresolved != 1) {
			t.Fatalf("PR 1 = %s %+v (%s)", row.State, row.Detail, row.DetailError)
		}
	}
}

// A checkout whose origin git cannot read, once the cached origin is gone,
// stays listed with the failure beside the rows read before it, and
// recovers when the checkout reads again.
func TestPRListKeepsAProjectWhoseOriginCannotBeRead(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{{Number: 1, Title: "One", Author: "carol"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	projectID := forgeProjectRepo(t, app, "https://github.com/acme/widgets.git")
	project, err := app.store.GetProject(projectID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.prUpdates.wg.Wait)
	sub := subscribePRList(t, app)
	if got := prListProject(t, sub.Snapshot, projectID); got.Error != "" || len(got.Rows) != 1 {
		t.Fatalf("project = %+v", got)
	}

	config := filepath.Join(project.Path, ".git", "config")
	good, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	app.gitCore().InvalidateForgeCache(project.Path)
	if err := os.WriteFile(config, []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	got := prListProject(t, prListSnapshotNow(app), projectID)
	if got.Error == "" || got.ErrorKind != forgeFailureForge || len(got.Rows) != 1 || got.Repo.Key() != "github:acme/widgets" {
		t.Fatalf("unreadable project = %+v, want its error beside the rows read before", got)
	}

	if err := os.WriteFile(config, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := prListProject(t, prListSnapshotNow(app), projectID); got.Error != "" || len(got.Rows) != 1 {
		t.Fatalf("recovered project = %+v", got)
	}
}
