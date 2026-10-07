package app

import (
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// The boot pass runs once a client has read its catalogs, re-derives a row
// whose origin changed after creation, and announces the moved row so a
// client that already listed it converges. Shutdown joins it.
func TestProjectIdentityRefreshAnnouncesAnOriginAddedAfterCreation(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	created, err := app.CreateProject(repo)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if created.RemoteURL != "" || created.RootCommit == "" {
		t.Fatalf("created identity = %q/%q, want a root and no origin yet", created.RemoteURL, created.RootCommit)
	}
	const origin = "git@example.com:me/app.git"
	testutil.RunGit(t, repo, "remote", "add", "origin", origin)

	announced := make(chan triage.ProjectUpdateEvent, 8)
	app.emitEventFn = func(name string, data any) {
		if evt, ok := data.(triage.ProjectUpdateEvent); ok && name == "project:updated" {
			announced <- evt
		}
	}
	app.startProjectIdentityRefresh()
	if _, err := app.ListProjects(); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	select {
	case evt := <-announced:
		if evt.Action != triage.ProjectActionFull || evt.Project == nil || evt.Project.ID != created.ID || evt.Project.RemoteURL != origin {
			t.Fatalf("announced %+v, want the full row of %s with origin %q", evt, created.ID, origin)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh announced nothing")
	}
	app.projectIdentityWG.Wait()
	select {
	case evt := <-announced:
		t.Fatalf("an unchanged row was announced: %+v", evt)
	default:
	}
	stored, err := app.store.GetProject(created.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RemoteURL != origin || stored.RootCommit != created.RootCommit || stored.IdentityError != "" {
		t.Fatalf("stored identity = %+v, want origin %q and root %q", stored, origin, created.RootCommit)
	}
}
