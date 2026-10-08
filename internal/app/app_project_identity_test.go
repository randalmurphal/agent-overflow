package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
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

// A row whose forge was unavailable during the boot pass is retried on the
// backoff and announced once the forge answers, without waiting for the next
// boot.
func TestProjectIdentityRefreshRetriesAnUnavailableForge(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	calls := filepath.Join(t.TempDir(), "calls")
	// Creation and the boot pass both find the forge unavailable; only a
	// retry can resolve the row.
	failForgeLookups(t, app, calls, 2)
	app.maintenance.identityRetry = 50 * time.Millisecond
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/me/app")
	created, err := app.CreateProject(repo)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if created.RepositoryID != "" || created.IdentityError == "" {
		t.Fatalf("created identity = %+v, want the forge unavailable", created)
	}

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
		if evt.Project == nil || evt.Project.ID != created.ID || evt.Project.RepositoryID != "github:github.com:123" || evt.Project.IdentityError != "" {
			t.Fatalf("announced %+v, want the verified row", evt)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the retry never resolved the row")
	}
	app.projectIdentityWG.Wait()
	if got := forgeCalls(t, calls); got != 3 {
		t.Fatalf("forge calls = %d, want creation, the pass and one retry", got)
	}
}

// Shutdown ends a retry loop that is waiting out its delay and joins it
// without waiting for the next attempt.
func TestProjectIdentityRetryStopsWithTheApp(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	calls := filepath.Join(t.TempDir(), "calls")
	failForgeLookups(t, app, calls, -1)
	app.maintenance.identityRetry = time.Hour
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/me/app")
	if _, err := app.CreateProject(repo); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app.startProjectIdentityRefresh()
	if _, err := app.ListProjects(); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	// Creation and the boot pass made two calls; the loop now waits an hour.
	deadline := time.Now().Add(30 * time.Second)
	for forgeCalls(t, calls) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("forge calls = %d, want creation and the pass", forgeCalls(t, calls))
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- app.Shutdown(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown waited on the retry loop's delay")
	}
}

// failForgeLookups makes the first n forge lookups fail, all of them when n
// is negative, counting every call in the calls file.
func failForgeLookups(t *testing.T, app *App, calls string, n int) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "identity-tool")
	mockexec.Write(t, fake, fmt.Sprintf("#!/bin/sh\nprintf 'call\\n' >> %q\nn=$(wc -l < %q)\nif [ %d -lt 0 ] || [ \"$n\" -le %d ]; then echo 'forge unavailable' >&2; exit 1; fi\nprintf '{\"id\":123}'\n", calls, calls, n, n))
	app.forgeCLIs = isolatedForgeCLIs{isolated: true, fake: fake}
}

func forgeCalls(t *testing.T, calls string) int {
	t.Helper()
	data, err := os.ReadFile(calls)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read forge calls: %v", err)
	}
	return strings.Count(string(data), "\n")
}
