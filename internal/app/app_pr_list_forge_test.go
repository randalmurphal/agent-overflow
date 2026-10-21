package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// reviewersThen is n other reviewers, then login.
func reviewersThen(n int, login string) []string {
	reviewers := make([]string, 0, n+1)
	for i := range n {
		reviewers = append(reviewers, fmt.Sprintf("reviewer-%03d", i))
	}
	return append(reviewers, login)
}

// The list pump against the fake forge on both forges: the projects come
// from the store and their origins, and each project's read (the list and
// its detail requests, none per row) gives every row its attention state,
// approvals, unresolved threads and pipeline.
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
			// The author answered the viewer's change request without a push.
			{Number: 4, Title: "Carol replied", Author: "carol",
				Reviews:  []forgefake.Review{{Author: "ao-viewer", State: "CHANGES_REQUESTED", SubmittedAt: "2026-01-02T00:00:00Z"}},
				Comments: []forgefake.Comment{{Author: "carol", Body: "Done, see the reply", CreatedAt: "2026-01-03T00:00:00Z"}}},
			// Someone else commented; the author did not answer.
			{Number: 5, Title: "Dave commented", Author: "carol",
				Reviews:  []forgefake.Review{{Author: "ao-viewer", State: "CHANGES_REQUESTED", SubmittedAt: "2026-01-02T00:00:00Z"}},
				Comments: []forgefake.Comment{{Author: "dave", Body: "+1", CreatedAt: "2026-01-03T00:00:00Z"}}},
		}},
		{Forge: "gitlab", Project: "team/app", Merge: &forgefake.RepoMerge{ApprovalRules: []int{1}}, Pulls: []forgefake.Pull{
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
			// The viewer is past the first 100 reviewers.
			{Number: 11, Title: "I asked for changes", Author: "carol", Reviewers: reviewersThen(100, "ao-viewer"), ChangeRequests: []string{"ao-viewer"}},
			// Neither the viewer's nor asked of it: no detail read covers it.
			{Number: 12, Title: "Someone else's", Author: "carol"},
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
	events := capturePRList(t, app)
	subscribePRList(t, app)
	snapshot := awaitPRListRead(t, events)
	type want struct {
		state, reason string
		approvals     *PRApprovals
		unresolved    int
		ci            string
	}
	check := func(project ProjectPRList, wants map[int]want) {
		t.Helper()
		if project.Error != "" || project.DetailError != "" || len(project.Rows) != len(wants) {
			t.Fatalf("project = %+v", project)
		}
		for _, row := range project.Rows {
			w := wants[row.Ref.Number]
			reason := ""
			if row.Reason != nil {
				reason = row.Reason.Kind
			}
			unresolved := -1
			if row.Unresolved != nil {
				unresolved = *row.Unresolved
			}
			if row.State != w.state || reason != w.reason || unresolved != w.unresolved || row.CI != w.ci ||
				(row.Approvals == nil) != (w.approvals == nil) || (w.approvals != nil && *row.Approvals != *w.approvals) {
				t.Errorf("%s = %s/%s approvals %+v unresolved %d ci %q; want %+v", row.Ref.Key(), row.State, reason, row.Approvals, unresolved, row.CI, w)
			}
		}
	}
	check(prListProject(t, snapshot, github), map[int]want{
		1: {state: PRStateReady, approvals: &PRApprovals{Given: 1}, unresolved: 1},
		2: {state: PRStateYourTurn, reason: PRReasonReviewRequested, approvals: &PRApprovals{}},
		3: {state: PRStatePipelineFailed, approvals: &PRApprovals{}, ci: gitops.CIStatusFailed},
		4: {state: PRStateYourTurn, reason: PRReasonReplied, approvals: &PRApprovals{}},
		5: {state: PRStateWaitingOnAuthor, approvals: &PRApprovals{}},
	})
	check(prListProject(t, snapshot, gitlab), map[int]want{
		7:  {state: PRStatePipelineFailed, approvals: &PRApprovals{Given: 1, Required: 1}, unresolved: 1, ci: gitops.CIStatusFailed},
		8:  {state: PRStateApproved, reason: PRReasonApproved, approvals: &PRApprovals{Given: 1, Required: 1}},
		9:  {state: PRStateYourTurn, reason: PRReasonReviewRequested, approvals: &PRApprovals{Required: 1}},
		10: {state: PRStateApproved, reason: PRReasonApproved, approvals: &PRApprovals{Given: 1, Required: 1}},
		11: {state: PRStateWaitingOnAuthor, approvals: &PRApprovals{Required: 1}},
		12: {state: PRStateOpen, unresolved: -1},
	})
	for _, row := range prListProject(t, snapshot, github).Rows {
		if row.Ref.Number == 4 && row.Reason.Replies != 1 {
			t.Errorf("PR 4 replies = %d, want the author's one comment", row.Reason.Replies)
		}
	}
	// No request names one PR: each project costs its forge's fixed set.
	perRoute := map[string]int{}
	for _, inv := range engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unhandled forge call: %+v", inv)
		}
		if !strings.HasSuffix(inv.Route, "repository identity") {
			// Registering a project reads its repository's identity.
			perRoute[inv.Route]++
		}
	}
	for _, route := range []string{"gh graphql PullsSearch", "glab api merge request list", "glab graphql PullsReviewing", "glab graphql PullsAuthored",
		"glab graphql PullsReviewingActivity", "glab graphql PullsAuthoredActivity", "glab api user events", "glab api approval rules", "glab api user"} {
		if perRoute[route] != 1 {
			t.Errorf("route %s sent %d times, want once: %v", route, perRoute[route], perRoute)
		}
	}
	if len(perRoute) != 9 {
		t.Errorf("routes = %v, want only the project reads", perRoute)
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
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	if got := prListProject(t, awaitPRListRead(t, events), projectID); got.Error != "" || len(got.Rows) != 1 {
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
