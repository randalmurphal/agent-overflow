package app

import (
	"agent-overflow/internal/testutil/mockexec"
	"path/filepath"
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
	mockRepositoryIdentity(t, app)
	repo := initMainGitRepo(t)
	created, err := app.CreateProject(repo)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if created.RepositoryID != "" || created.IdentityError == "" {
		t.Fatalf("created identity = %+v, want verification unavailable without origin", created)
	}
	const origin = "git@github.com:me/app.git"
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
		if evt.Action != triage.ProjectActionFull || evt.Project == nil || evt.Project.ID != created.ID || evt.Project.RepositoryID != "github:github.com:123" {
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
	if stored.RepositoryID != "github:github.com:123" || stored.IdentityError != "" {
		t.Fatalf("stored identity = %+v, want verified repository ID", stored)
	}
}

func mockRepositoryIdentity(t *testing.T, app *App) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "identity-tool")
	mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then for host do :; done; printf 'hostname %s\\n' \"$host\"; else printf '{\"id\":123}'; fi\n")
	app.forgeCLIs = isolatedForgeCLIs{isolated: true, fake: fake}
}
