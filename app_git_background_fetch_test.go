package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// memoKeys snapshots the remembered failure keys.
func memoKeys(memo *backgroundFetchErrorMemo) []string {
	memo.mu.Lock()
	defer memo.mu.Unlock()
	keys := make([]string, 0, len(memo.last))
	for key := range memo.last {
		keys = append(keys, key)
	}
	return keys
}

// TestBackgroundFetchErrorMemoTransitions covers the memo over
// sequences, not states: the same failure repeating must stay quiet, a
// different failure must speak up, and a success in between must make
// the ORIGINAL failure loggable again. State coverage alone would miss
// the last one, which is the whole point of clearing on success.
func TestBackgroundFetchErrorMemoTransitions(t *testing.T) {
	var memo backgroundFetchErrorMemo
	const repo = "repo:/a/.git"

	if !memo.shouldLog(repo, "auth failed") {
		t.Fatal("first failure must log")
	}
	if memo.shouldLog(repo, "auth failed") {
		t.Fatal("the same failure repeating must not log again")
	}
	if !memo.shouldLog(repo, "host unreachable") {
		t.Fatal("a different failure must log")
	}
	if memo.shouldLog(repo, "host unreachable") {
		t.Fatal("the new failure repeating must not log again")
	}

	// off -> on -> off: recovery, then the same error as before.
	memo.clear(repo)
	if !memo.shouldLog(repo, "host unreachable") {
		t.Fatal("a failure after a success must log again — it is news")
	}

	// Repos are independent: one repo's noise must not silence another's.
	const other = "repo:/b/.git"
	if !memo.shouldLog(other, "host unreachable") {
		t.Fatal("an identical failure on a different repo must log")
	}

	// Clearing a key nobody remembers is a no-op, not a panic — the
	// success path calls it on every pass.
	memo.clear("repo:/never-seen/.git")

	memo.retain(map[string]struct{}{repo: {}})
	if got := memoKeys(&memo); len(got) != 1 || got[0] != repo {
		t.Fatalf("after retain, memo keys = %v, want [%s]", got, repo)
	}
	if !memo.shouldLog(other, "host unreachable") {
		t.Fatal("a repo dropped from the project list must report freshly when it returns")
	}
}

// newBackgroundFetchTestApp builds an App with a store, settings, and a
// pinned git Core (gitCore() hands out a throwaway Core when a.git is
// nil, which would reset the fetch throttle between passes).
func newBackgroundFetchTestApp(t *testing.T) *App {
	t.Helper()
	app, cleanup := newTestApp(t)
	t.Cleanup(cleanup)
	app.git = gitops.NewCore()
	return app
}

func addProject(t *testing.T, app *App, id, path string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := app.store.CreateProject(store.Project{
		ID:        id,
		Path:      path,
		Name:      id,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
}

func behindCount(t *testing.T, app *App, repo string) int {
	t.Helper()
	status, err := app.gitCore().StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast(%s): %v", repo, err)
	}
	return status.BehindCount
}

// TestBackgroundFetchPassRefreshesBehindCount is the feature in one
// test: a project's repository falls behind its remote with no local
// activity at all, and one pass of the cadence makes the count true.
func TestBackgroundFetchPassRefreshesBehindCount(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	addProject(t, app, "p-behind", repo)

	testutil.AdvanceOriginMain(t, bare)
	if got := behindCount(t, app, repo); got != 0 {
		t.Fatalf("BehindCount before the pass = %d, want 0 (refs still stale)", got)
	}

	app.runBackgroundFetchPass(t.Context())

	if got := behindCount(t, app, repo); got != 1 {
		t.Fatalf("BehindCount after the pass = %d, want 1", got)
	}
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 0 {
		t.Fatalf("memo = %v, want empty after a clean pass", got)
	}
}

func TestBackgroundFetchPassSkippedWhenSettingDisabled(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	addProject(t, app, "p-disabled", repo)
	testutil.AdvanceOriginMain(t, bare)

	if _, err := app.settings.Update(map[string]any{"backgroundGitFetch": false}); err != nil {
		t.Fatalf("disable backgroundGitFetch: %v", err)
	}

	app.runBackgroundFetchPass(t.Context())
	if got := behindCount(t, app, repo); got != 0 {
		t.Fatalf("BehindCount = %d with the setting off, want 0 (no fetch)", got)
	}

	// And back on again — the toggle is read live, so the very next
	// pass works without a restart.
	if _, err := app.settings.Update(map[string]any{"backgroundGitFetch": true}); err != nil {
		t.Fatalf("re-enable backgroundGitFetch: %v", err)
	}
	app.runBackgroundFetchPass(t.Context())
	if got := behindCount(t, app, repo); got != 1 {
		t.Fatalf("BehindCount = %d after re-enabling, want 1", got)
	}
}

// TestBackgroundFetchPassFetchesEachRepositoryOnce registers the same
// repository twice — once as its primary checkout, once as a linked
// worktree — and asserts the pass treats them as one repo. The origin is
// unreachable so every attempt fails loudly: one memo entry means one
// attempt, two would mean the worktree got its own fetch.
func TestBackgroundFetchPassFetchesEachRepositoryOnce(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	worktree := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "branch", "feature/bg-fetch")
	testutil.RunGit(t, repo, "worktree", "add", worktree, "feature/bg-fetch")

	addProject(t, app, "p-primary", repo)
	addProject(t, app, "p-worktree", worktree)

	app.runBackgroundFetchPass(t.Context())

	commonDir, err := app.gitCore().CommonDir(repo)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	want := backgroundFetchRepoKey + commonDir
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 || got[0] != want {
		t.Fatalf("memo keys = %v, want exactly [%s] (one fetch per repository)", got, want)
	}
}

// TestBackgroundFetchPassMemoRecoversAndReports walks the pass-level
// transition the memo exists for: fail, recover, fail again.
func TestBackgroundFetchPassMemoRecoversAndReports(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	addProject(t, app, "p-flaky", repo)

	broken := filepath.Join(t.TempDir(), "gone.git")
	testutil.RunGit(t, repo, "remote", "set-url", "origin", broken)
	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 {
		t.Fatalf("memo keys after a failing pass = %v, want one entry", got)
	}

	testutil.RunGit(t, repo, "remote", "set-url", "origin", bare)
	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 0 {
		t.Fatalf("memo keys after recovery = %v, want empty", got)
	}

	// The throttle would skip a second fetch inside the window, and a
	// skip is not a success signal to build a memo test on — drop the
	// stamp so this pass genuinely re-runs git and fails again.
	app.gitCore().InvalidateFetchCache(repo)
	testutil.RunGit(t, repo, "remote", "set-url", "origin", broken)
	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 {
		t.Fatalf("memo keys after the failure returned = %v, want one entry", got)
	}
}

// TestBackgroundFetchPassRemembersUnresolvableProjectPaths covers the
// project-path key namespace: a project whose directory is not a
// repository never reaches a fetch, and its complaint is remembered
// against the path (there is no repo identity to remember it against).
func TestBackgroundFetchPassRemembersUnresolvableProjectPaths(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	notARepo := t.TempDir()
	addProject(t, app, "p-not-a-repo", notARepo)

	app.runBackgroundFetchPass(t.Context())

	want := backgroundFetchPathKey + notARepo
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 || got[0] != want {
		t.Fatalf("memo keys = %v, want [%s]", got, want)
	}

	// Repeat passes keep exactly one entry: the log is memoized, and
	// retain must not drop a key the pass just re-reported.
	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 || got[0] != want {
		t.Fatalf("memo keys after a second pass = %v, want [%s]", got, want)
	}
}

// TestBackgroundFetchPassForgetsRemovedProjects keeps the memo bounded
// by the CURRENT project list rather than by everything ever seen.
func TestBackgroundFetchPassForgetsRemovedProjects(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	notARepo := t.TempDir()
	addProject(t, app, "p-gone", notARepo)

	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 1 {
		t.Fatalf("memo keys = %v, want one entry", got)
	}

	if err := app.store.DeleteProject("p-gone"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	app.runBackgroundFetchPass(t.Context())
	if got := memoKeys(&app.backgroundFetchErrors); len(got) != 0 {
		t.Fatalf("memo keys after the project was removed = %v, want empty", got)
	}
}

func TestBackgroundFetchPassStopsOnCancelledContext(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	addProject(t, app, "p-stop", repo)
	testutil.AdvanceOriginMain(t, bare)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	app.runBackgroundFetchPass(ctx)

	if got := behindCount(t, app, repo); got != 0 {
		t.Fatalf("BehindCount = %d after a pass whose context was cancelled, want 0", got)
	}
}

// TestBackgroundGitFetchDisabledNeverStartsALoop pins the harness /
// offline guarantee: with the flag set there is no goroutine at all, so
// no timer can reach a repository.
func TestBackgroundGitFetchDisabledNeverStartsALoop(t *testing.T) {
	app := newBackgroundFetchTestApp(t)
	app.backgroundFetchDisabled = true

	app.startBackgroundGitFetch()
	app.backgroundFetchMu.Lock()
	stop := app.backgroundFetchStop
	app.backgroundFetchMu.Unlock()
	if stop != nil {
		t.Fatal("startBackgroundGitFetch created a loop while disabled")
	}

	// Stopping something that never started must be a no-op, not a hang.
	app.stopBackgroundGitFetch()
}

func TestBackgroundGitFetchStartIsIdempotentAndStops(t *testing.T) {
	app := newBackgroundFetchTestApp(t)

	app.startBackgroundGitFetch()
	app.backgroundFetchMu.Lock()
	first := app.backgroundFetchStop
	app.backgroundFetchMu.Unlock()
	if first == nil {
		t.Fatal("startBackgroundGitFetch did not create a loop")
	}

	app.startBackgroundGitFetch()
	app.backgroundFetchMu.Lock()
	second := app.backgroundFetchStop
	app.backgroundFetchMu.Unlock()
	if second != first {
		t.Fatal("a second start replaced the running loop instead of no-oping")
	}

	app.stopBackgroundGitFetch()
	app.stopBackgroundGitFetch()
}
