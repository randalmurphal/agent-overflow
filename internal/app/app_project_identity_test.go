package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// mockRepositoryIdentity answers every repository identity read with id
// 123: GitHub's through the forge API transport, an SSH alias through the
// fake ssh.
func mockRepositoryIdentity(t *testing.T, app *App) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "identity-tool")
	mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then for host do :; done; printf 'hostname %s\\n' \"$host\"; else exit 1; fi\n")
	app.forgeCLIs = isolatedForgeCLIs{isolated: true, fake: fake}
	identityForge(t, app, 0)
}

// identityForge gives app a git core whose GitHub fails the first n
// repository reads, all of them when n is negative, and answers id 123
// after. It counts every read.
func identityForge(t *testing.T, app *App, n int32) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	svc := githubAPITestService(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/github/rest/repos/") {
			t.Errorf("unexpected forge request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if call := calls.Add(1); n < 0 || call <= n {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"forge unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":123}`)
	})
	app.git = app.buildGitCore(svc)
	return &calls
}

// A row whose forge was unavailable during the boot pass is retried on the
// backoff and announced once the forge answers, without waiting for the next
// boot.
func TestProjectIdentityRefreshRetriesAnUnavailableForge(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	// Creation finds the forge unavailable and the boot pass shares that
	// failure while the Core keeps it; only a retry after it expires can
	// resolve the row.
	calls := failForgeLookups(t, app, 1)
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
	if got := calls.Load(); got != 2 {
		t.Fatalf("forge calls = %d, want creation and the retry that resolved the row", got)
	}
}

// Shutdown ends a retry loop that is waiting out its delay and joins it
// without waiting for the next attempt.
func TestProjectIdentityRetryStopsWithTheApp(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	app.maintenance.identityRetry = time.Hour
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/me/app")
	// Created before the app has a forge transport, so the boot pass makes
	// the first forge call instead of sharing a failure creation left.
	if _, err := app.CreateProject(repo); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	calls := failForgeLookups(t, app, -1)
	app.startProjectIdentityRefresh()
	if _, err := app.ListProjects(); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	// The boot pass made its call; the loop now waits an hour.
	deadline := time.Now().Add(30 * time.Second)
	for calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the boot pass made no forge call")
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
// is negative, and returns the count of every call.
func failForgeLookups(t *testing.T, app *App, n int32) *atomic.Int32 {
	t.Helper()
	app.forgeCLIs = isolatedForgeCLIs{isolated: true}
	return identityForge(t, app, n)
}
