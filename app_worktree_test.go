package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

func TestGitCreateAndRemoveWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	// The thread must belong to a project whose path is the repo root
	// so GitCreateWorktree can drive git from the correct cwd.
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/worktree")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree path to exist: %v", err)
	}
	if !strings.Contains(worktreePath, "feature-worktree") {
		t.Fatalf("expected sanitized worktree path, got %q", worktreePath)
	}

	worktrees, err := app.GitListWorktrees(thread.ID)
	if err != nil {
		t.Fatalf("GitListWorktrees() error = %v", err)
	}
	if !containsWorktree(worktrees, worktreePath) {
		t.Fatalf("expected worktree %q in list: %+v", worktreePath, worktrees)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if !samePath(stored.WorktreePath, worktreePath) {
		t.Fatalf("stored WorktreePath = %q, want %q", stored.WorktreePath, worktreePath)
	}
	if !samePath(stored.WorkspacePath, worktreePath) {
		t.Fatalf("stored WorkspacePath = %q, want %q", stored.WorkspacePath, worktreePath)
	}
	if stored.Branch != "feature/worktree" {
		t.Fatalf("stored Branch = %q, want feature/worktree", stored.Branch)
	}

	if err := app.GitRemoveWorktree(thread.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removal, stat err = %v", err)
	}

	stored, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.WorktreePath != "" {
		t.Fatalf("stored WorktreePath after removal = %q, want empty", stored.WorktreePath)
	}
	if !samePath(stored.WorkspacePath, repo) {
		t.Fatalf("stored WorkspacePath after removal = %q, want %q", stored.WorkspacePath, repo)
	}
	if stored.Branch != "main" {
		t.Fatalf("stored Branch after removal = %q, want main", stored.Branch)
	}
}

func TestGetWorkspaceCurrentDiffUsesLinkedWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-current-diff")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/workspace-current-diff")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nmain-root-change\n"), 0o644); err != nil {
		t.Fatalf("write main worktree change: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "README.txt"), []byte("hello\nlinked-worktree-change\n"), 0o644); err != nil {
		t.Fatalf("write linked worktree change: %v", err)
	}

	diff, err := app.GetWorkspaceCurrentDiff(thread.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceCurrentDiff() error = %v", err)
	}
	if !strings.Contains(diff, "linked-worktree-change") {
		t.Fatalf("diff did not include linked worktree change:\n%s", diff)
	}
	if strings.Contains(diff, "main-root-change") {
		t.Fatalf("diff included project-root change instead of only worktree change:\n%s", diff)
	}

	testutil.RunGit(t, worktreePath, "add", "README.txt")
	testutil.RunGit(t, worktreePath, "commit", "-m", "commit linked worktree change")

	diff, err = app.GetWorkspaceCurrentDiff(thread.ID)
	if err != nil {
		t.Fatalf("GetWorkspaceCurrentDiff(clean worktree) error = %v", err)
	}
	if strings.TrimSpace(diff) != "" {
		t.Fatalf("diff after committing linked worktree = %q, want empty", diff)
	}
}

func TestGitCreateWorktreePreservesExplicitBranchCase(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	if err := app.gitCore().CreateBranch(repo, "case-probe-branch"); err != nil {
		t.Fatalf("CreateBranch(case probe lower) error = %v", err)
	}
	if err := app.gitCore().CreateBranch(repo, "CASE-PROBE-BRANCH"); err != nil {
		t.Skipf("git on this host does not support case-distinct branch refs: %v", err)
	}

	lowerThread := testThread("thread-worktree-lowercase")
	lowerThread.ProjectID = project.ID
	lowerThread.WorkspacePath = repo
	if err := app.store.CreateThread(lowerThread); err != nil {
		t.Fatalf("CreateThread(lower) error = %v", err)
	}
	lowerWorktreePath, err := app.GitCreateWorktree(lowerThread.ID, "blitz-73")
	if err != nil {
		t.Fatalf("GitCreateWorktree(lower) error = %v", err)
	}

	upperThread := testThread("thread-worktree-uppercase")
	upperThread.ProjectID = project.ID
	upperThread.WorkspacePath = repo
	upperThread.Branch = "main"
	if err := app.store.CreateThread(upperThread); err != nil {
		t.Fatalf("CreateThread(upper) error = %v", err)
	}
	upperWorktreePath, err := app.GitCreateWorktree(upperThread.ID, "BLITZ-73")
	if err != nil {
		t.Fatalf("GitCreateWorktree(upper) error = %v", err)
	}

	if samePath(lowerWorktreePath, upperWorktreePath) {
		t.Fatalf("worktree paths should differ for case-distinct branches, got %q", lowerWorktreePath)
	}

	lowerStored, err := app.store.GetThread(lowerThread.ID)
	if err != nil {
		t.Fatalf("GetThread(lower) error = %v", err)
	}
	if lowerStored.Branch != "blitz-73" {
		t.Fatalf("lower Branch = %q, want blitz-73", lowerStored.Branch)
	}

	upperStored, err := app.store.GetThread(upperThread.ID)
	if err != nil {
		t.Fatalf("GetThread(upper) error = %v", err)
	}
	if upperStored.Branch != "BLITZ-73" {
		t.Fatalf("upper Branch = %q, want BLITZ-73", upperStored.Branch)
	}

	branches, err := app.GitListBranches(lowerThread.ID)
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}
	if !containsBranch(branches, "blitz-73") {
		t.Fatalf("expected blitz-73 in branches: %+v", branches)
	}
	if !containsBranch(branches, "BLITZ-73") {
		t.Fatalf("expected BLITZ-73 in branches: %+v", branches)
	}
}

func TestGitCreateWorktreeUsesTemporaryBranchWhenEmpty(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-auto")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if !gitops.IsTemporaryWorktreeBranch(stored.Branch) {
		t.Fatalf("stored Branch = %q, want ao-<8-hex>", stored.Branch)
	}
	if !strings.Contains(worktreePath, "ao-") {
		t.Fatalf("worktreePath = %q, want ao-prefixed temp path", worktreePath)
	}
}

func TestWorktreeMutationRejectsActiveTurnWithoutSession(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-worktree-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	_, err = app.PrepareThreadWorktree(thread.ID, "main", "feature/blocked", false)
	if err == nil || !strings.Contains(err.Error(), "cannot switch workspace while turn 0 is active") {
		t.Fatalf("PrepareThreadWorktree() error = %v, want active-turn rejection", err)
	}
}

func TestPrepareThreadWorktreeWaitsForLiveSessionRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-sync-restart")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "old-session",
		liveness: newSessionLiveness(time.Now()),
	}

	restartWorkspace := make(chan string, 1)
	releaseRestart := make(chan struct{})
	restartBlocked := make(chan struct{})
	app.startSessionFn = func(threadID string) error {
		stored, err := app.store.GetThread(threadID)
		if err != nil {
			return err
		}
		restartWorkspace <- stored.WorkspacePath
		close(restartBlocked)
		<-releaseRestart
		return nil
	}

	type result struct {
		thread store.Thread
		err    error
	}
	done := make(chan result, 1)
	go func() {
		updated, err := app.PrepareThreadWorktree(thread.ID, "main", "feature/sync-restart", false)
		done <- result{thread: updated, err: err}
	}()

	var worktreePath string
	select {
	case worktreePath = <-restartWorkspace:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for synchronous session restart")
	}
	if samePath(worktreePath, repo) {
		t.Fatalf("restart saw old workspace %q, want new worktree", worktreePath)
	}
	if !strings.Contains(worktreePath, "feature-sync-restart") {
		t.Fatalf("restart workspace = %q, want generated worktree path", worktreePath)
	}
	<-restartBlocked

	select {
	case got := <-done:
		t.Fatalf("PrepareThreadWorktree returned before restart completed: %+v", got)
	default:
	}

	close(releaseRestart)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PrepareThreadWorktree() error = %v", got.err)
		}
		if !samePath(got.thread.WorkspacePath, worktreePath) {
			t.Fatalf("WorkspacePath = %q, want %q", got.thread.WorkspacePath, worktreePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PrepareThreadWorktree after restart release")
	}
}

func TestPrepareThreadWorktreeRestartsAfterInFlightSessionStart(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-inflight-restart")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	firstStartWorkspace := make(chan string, 1)
	secondStartWorkspace := make(chan string, 1)
	releaseFirstStart := make(chan struct{})
	releaseSecondStart := make(chan struct{})
	var startCalls int
	app.startSessionFn = func(threadID string) error {
		stored, err := app.store.GetThread(threadID)
		if err != nil {
			return err
		}
		startCalls++
		switch startCalls {
		case 1:
			firstStartWorkspace <- stored.WorkspacePath
			<-releaseFirstStart
			app.sessionManager().put(threadID, session{
				provider: string(provider.Claude),
				token:    "first-start",
				liveness: newSessionLiveness(time.Now()),
			})
		case 2:
			secondStartWorkspace <- stored.WorkspacePath
			<-releaseSecondStart
		default:
			t.Fatalf("unexpected startSessionFn call %d", startCalls)
		}
		return nil
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- app.startSession(thread.ID)
	}()
	select {
	case workspace := <-firstStartWorkspace:
		if !samePath(workspace, repo) {
			t.Fatalf("first start workspace = %q, want repo %q", workspace, repo)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial in-flight start")
	}

	type result struct {
		thread store.Thread
		err    error
	}
	done := make(chan result, 1)
	go func() {
		updated, err := app.PrepareThreadWorktree(thread.ID, "main", "feature/inflight-restart", false)
		done <- result{thread: updated, err: err}
	}()

	close(releaseFirstStart)
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("initial start error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial start release")
	}

	var worktreePath string
	select {
	case worktreePath = <-secondStartWorkspace:
	case got := <-done:
		t.Fatalf("PrepareThreadWorktree returned before post-start restart: %+v", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workspace restart after in-flight start")
	}
	if samePath(worktreePath, repo) {
		t.Fatalf("second start saw old workspace %q, want new worktree", worktreePath)
	}
	if !strings.Contains(worktreePath, "feature-inflight-restart") {
		t.Fatalf("second start workspace = %q, want generated worktree path", worktreePath)
	}

	select {
	case got := <-done:
		t.Fatalf("PrepareThreadWorktree returned before second restart completed: %+v", got)
	default:
	}

	close(releaseSecondStart)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PrepareThreadWorktree() error = %v", got.err)
		}
		if !samePath(got.thread.WorkspacePath, worktreePath) {
			t.Fatalf("WorkspacePath = %q, want %q", got.thread.WorkspacePath, worktreePath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PrepareThreadWorktree after restart release")
	}
}

func TestPrepareThreadWorktreeKeepsWorkspaceSwitchWhenRestartFails(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-restart-fails")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "old-session",
		liveness: newSessionLiveness(time.Now()),
	}
	app.startSessionFn = func(string) error {
		return errors.New("provider restart failed")
	}

	updated, err := app.PrepareThreadWorktree(thread.ID, "main", "feature/restart-fails", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}
	if samePath(updated.WorkspacePath, repo) {
		t.Fatalf("WorkspacePath = %q, want new worktree despite restart failure", updated.WorkspacePath)
	}
	if !strings.Contains(updated.WorkspacePath, "feature-restart-fails") {
		t.Fatalf("WorkspacePath = %q, want generated worktree path", updated.WorkspacePath)
	}
	if app.hasActiveSession(thread.ID) {
		t.Fatal("expected failed reconnect to remove stale session")
	}
}

// GitRemoveWorktree used to refuse when a sibling thread referenced the same
// worktree. Under the new cleanup semantics it auto-reattaches every thread
// (including archived ones, which the user might restore later) back to the
// project root and proceeds with the removal.
func TestGitRemoveWorktreeReattachesOtherThreadsToProjectRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-worktree-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner) error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/shared")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	other := testThread("thread-worktree-user")
	other.ProjectID = project.ID
	other.WorkspacePath = worktreePath
	other.WorktreePath = worktreePath
	other.Branch = "feature/shared"
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other) error = %v", err)
	}

	if err := app.GitRemoveWorktree(owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	refreshed, err := app.store.GetThread(other.ID)
	if err != nil {
		t.Fatalf("GetThread(other) error = %v", err)
	}
	if !samePath(refreshed.WorkspacePath, repo) {
		t.Errorf("other thread WorkspacePath = %q, want project root %q", refreshed.WorkspacePath, repo)
	}
	if refreshed.WorktreePath != "" {
		t.Errorf("other thread WorktreePath = %q, want empty", refreshed.WorktreePath)
	}
	if refreshed.Branch != "main" {
		t.Errorf("other thread Branch = %q, want main", refreshed.Branch)
	}
}

// The auto-reattach sweep is system-driven: it must NOT bump UpdatedAt,
// because the sidebar sorts threads by updated_at DESC and a bump would
// jerk every reattached thread to the top of the list, erasing the
// position the user had built up. Set known-old timestamps before the
// removal and assert they survive intact.
func TestGitRemoveWorktreeDoesNotBumpReattachedThreadActivity(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-worktree-owner-activity")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner) error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/activity-stable")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	other := testThread("thread-worktree-user-activity")
	other.ProjectID = project.ID
	other.WorkspacePath = worktreePath
	other.WorktreePath = worktreePath
	other.Branch = "feature/activity-stable"
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other) error = %v", err)
	}

	persistedOwner, err := app.store.GetThread(owner.ID)
	if err != nil {
		t.Fatalf("GetThread(owner) error = %v", err)
	}
	ownerBefore := persistedOwner.UpdatedAt
	persistedOther, err := app.store.GetThread(other.ID)
	if err != nil {
		t.Fatalf("GetThread(other) error = %v", err)
	}
	otherBefore := persistedOther.UpdatedAt

	if err := app.GitRemoveWorktree(owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	refreshedOwner, err := app.store.GetThread(owner.ID)
	if err != nil {
		t.Fatalf("GetThread(owner) after remove: %v", err)
	}
	if refreshedOwner.UpdatedAt != ownerBefore {
		t.Errorf("owner.UpdatedAt = %d, want %d (sweep should not bump)", refreshedOwner.UpdatedAt, ownerBefore)
	}
	refreshedOther, err := app.store.GetThread(other.ID)
	if err != nil {
		t.Fatalf("GetThread(other) after remove: %v", err)
	}
	if refreshedOther.UpdatedAt != otherBefore {
		t.Errorf("other.UpdatedAt = %d, want %d (sweep should not bump)", refreshedOther.UpdatedAt, otherBefore)
	}
}

func TestGitRemoveWorktreeReattachesArchivedThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-worktree-owner-archived-ref")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner) error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/archived-shared")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	other := testThread("thread-worktree-archived-user")
	other.ProjectID = project.ID
	other.WorkspacePath = worktreePath
	other.WorktreePath = worktreePath
	other.Branch = "feature/archived-shared"
	other.Archived = true
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other) error = %v", err)
	}

	if err := app.GitRemoveWorktree(owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	refreshed, err := app.store.GetThread(other.ID)
	if err != nil {
		t.Fatalf("GetThread(other) error = %v", err)
	}
	if !samePath(refreshed.WorkspacePath, repo) {
		t.Errorf("archived thread WorkspacePath = %q, want project root %q", refreshed.WorkspacePath, repo)
	}
	if refreshed.WorktreePath != "" {
		t.Errorf("archived thread WorktreePath = %q, want empty", refreshed.WorktreePath)
	}
}

func TestPrepareThreadWorktreeBranchesFromSelectedBase(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(repo, "BASE.txt"), []byte("release\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	testutil.RunGit(t, repo, "add", "BASE.txt")
	testutil.RunGit(t, repo, "commit", "-m", "release base")
	testutil.RunGit(t, repo, "checkout", "main")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-base")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	updated, err := app.PrepareThreadWorktree(thread.ID, "release", "feature/base", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}
	if updated.Branch != "feature/base" {
		t.Fatalf("Branch = %q, want feature/base", updated.Branch)
	}
	if updated.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", updated.UpdatedAt, thread.UpdatedAt)
	}
	if _, err := os.Stat(filepath.Join(updated.WorktreePath, "BASE.txt")); err != nil {
		t.Fatalf("expected worktree to branch from release: %v", err)
	}
}

func TestPrepareThreadWorktreeCarriesLocalChanges(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-carry")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Stage a tracked change AND drop an untracked file so we exercise
	// the stash push -u path (stash create wouldn't cover untracked).
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nedited\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	updated, err := app.PrepareThreadWorktree(thread.ID, "main", "feature/carry", true)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}
	if updated.WorktreePath == "" {
		t.Fatal("WorktreePath empty after PrepareThreadWorktree")
	}

	// Both the tracked modification and the untracked file should land in
	// the new worktree.
	carriedTracked, err := os.ReadFile(filepath.Join(updated.WorktreePath, "README.txt"))
	if err != nil {
		t.Fatalf("read carried tracked file: %v", err)
	}
	if !strings.Contains(string(carriedTracked), "edited") {
		t.Fatalf("expected tracked edit to carry; got %q", string(carriedTracked))
	}
	if _, err := os.Stat(filepath.Join(updated.WorktreePath, "scratch.txt")); err != nil {
		t.Fatalf("expected untracked file to carry: %v", err)
	}

	// The source workspace's stash stack should be empty — the carry path
	// drops the entry once it's applied in the new worktree.
	core := app.gitCore()
	stdout, _, err := core.Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stash; got %q", stdout)
	}
}

func TestPrepareThreadWorktreeRejectsCarryFromDifferentBase(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "release")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-worktree-carry-mismatch")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.PrepareThreadWorktree(thread.ID, "release", "feature/mismatch", true); err == nil {
		t.Fatal("expected error when carryLocalChanges=true but base != current branch")
	}
}

func TestGitWorktreeStatusReportsDirtyAndAttached(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-status-owner")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/status")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	// Attach a second thread to the same worktree so the attached-thread
	// counter has something to count beyond the caller.
	other := testThread("thread-status-sibling")
	other.ProjectID = project.ID
	other.WorkspacePath = worktreePath
	other.WorktreePath = worktreePath
	other.Branch = "feature/status"
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}

	status, err := app.GitWorktreeStatus(thread.ID, worktreePath)
	if err != nil {
		t.Fatalf("GitWorktreeStatus() error = %v", err)
	}
	if !status.Dirty {
		t.Fatalf("Dirty = false, want true")
	}
	if status.UncommittedCount != 1 {
		t.Errorf("UncommittedCount = %d, want 1", status.UncommittedCount)
	}
	if status.HasUpstream {
		t.Errorf("HasUpstream = true, want false (test repo has no remote)")
	}
	if status.AttachedThreads != 2 {
		t.Errorf("AttachedThreads = %d, want 2", status.AttachedThreads)
	}
}

func TestGitWorktreeStatusForProjectDoesNotRequireThreadID(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-project-status-owner")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/project-status")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	status, err := app.GitWorktreeStatusForProject(project.ID, worktreePath)
	if err != nil {
		t.Fatalf("GitWorktreeStatusForProject() error = %v", err)
	}
	if !status.Dirty {
		t.Fatalf("Dirty = false, want true")
	}
	if status.UncommittedCount != 1 {
		t.Fatalf("UncommittedCount = %d, want 1", status.UncommittedCount)
	}
	if status.AttachedThreads != 1 {
		t.Fatalf("AttachedThreads = %d, want 1", status.AttachedThreads)
	}
}

func TestRemoveOtherWorktreeRefusesDirtyWithoutForce(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-remove-refuse")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	owner.Branch = "main"
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/refuse")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	if err := app.RemoveOtherWorktree(owner.ID, worktreePath, false); err == nil {
		t.Fatal("expected RemoveOtherWorktree to refuse a dirty worktree without force")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("dirty worktree should still exist after refused remove: %v", err)
	}

	if err := app.RemoveOtherWorktree(owner.ID, worktreePath, true); err != nil {
		t.Fatalf("RemoveOtherWorktree(force=true) error = %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("force remove should delete worktree; err = %v", err)
	}
}

func TestRemoveOtherWorktreeForProjectRemovesAndReturnsPlaceholderState(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-project-remove-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	owner.Branch = "main"
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}

	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/project-remove")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	state, err := app.RemoveOtherWorktreeForProject(project.ID, worktreePath, worktreePath, true)
	if err != nil {
		t.Fatalf("RemoveOtherWorktreeForProject() error = %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed; stat err = %v", err)
	}
	if !samePath(state.WorkspacePath, repo) {
		t.Fatalf("returned WorkspacePath = %q, want %q", state.WorkspacePath, repo)
	}
	if state.WorktreePath != "" {
		t.Fatalf("returned WorktreePath = %q, want empty", state.WorktreePath)
	}
	if state.Branch != "main" {
		t.Fatalf("returned Branch = %q, want main", state.Branch)
	}

	refreshedOwner, err := app.store.GetThread(owner.ID)
	if err != nil {
		t.Fatalf("GetThread(owner): %v", err)
	}
	if !samePath(refreshedOwner.WorkspacePath, repo) {
		t.Fatalf("owner WorkspacePath = %q, want %q", refreshedOwner.WorkspacePath, repo)
	}
	if refreshedOwner.WorktreePath != "" {
		t.Fatalf("owner WorktreePath = %q, want empty", refreshedOwner.WorktreePath)
	}
}

func TestRemoveOtherWorktreeRefusesProjectRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-remove-root-guard")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := app.RemoveOtherWorktree(thread.ID, repo, true); err == nil {
		t.Fatal("expected RemoveOtherWorktree to refuse the project root")
	}
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("project root should still exist after refused remove: %v", err)
	}
}

func TestRemoveOtherWorktreeRejectsActiveTurnOnSibling(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-remove-active-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/active")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	sibling := testThread("thread-remove-active-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = worktreePath
	sibling.WorktreePath = worktreePath
	sibling.Branch = "feature/active"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}

	// Mid-turn on the sibling: ensureWorkspaceChangeAllowed must reject
	// the remove BEFORE the worktree is touched, so the sibling's in-flight
	// work can't be yanked out from under it.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-active-sibling",
		ThreadID:  sibling.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	err = app.RemoveOtherWorktree(owner.ID, worktreePath, true)
	if err == nil {
		t.Fatal("expected RemoveOtherWorktree to refuse while sibling is mid-turn")
	}
	if !strings.Contains(err.Error(), sibling.ID) {
		t.Fatalf("error should name the offending sibling: got %v", err)
	}
	// Worktree must still exist — the rejection happens before any git op.
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should still exist after refused remove: %v", err)
	}
	// Sibling thread must still point at the worktree.
	refreshedSibling, err := app.store.GetThread(sibling.ID)
	if err != nil {
		t.Fatalf("GetThread(sibling) error = %v", err)
	}
	if !samePath(refreshedSibling.WorkspacePath, worktreePath) {
		t.Errorf("sibling WorkspacePath = %q, want unchanged %q", refreshedSibling.WorkspacePath, worktreePath)
	}
}

func TestRemoveOtherWorktreeIgnoresObsoleteInflightTurnOnSibling(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-remove-stale-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/stale-turn")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	sibling := testThread("thread-remove-stale-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = worktreePath
	sibling.WorktreePath = worktreePath
	sibling.Branch = "feature/stale-turn"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}

	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-stale-sibling",
		ThreadID:  sibling.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(stale): %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-done-sibling",
		ThreadID:  sibling.ID,
		TurnIndex: 1,
		StartedAt: 2,
	}); err != nil {
		t.Fatalf("InsertTurn(done): %v", err)
	}
	if err := app.store.UpdateTurnCompleted("turn-done-sibling", 3, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(done): %v", err)
	}

	if err := app.RemoveOtherWorktree(owner.ID, worktreePath, true); err != nil {
		t.Fatalf("RemoveOtherWorktree() error = %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed; stat err = %v", err)
	}
	refreshedSibling, err := app.store.GetThread(sibling.ID)
	if err != nil {
		t.Fatalf("GetThread(sibling) error = %v", err)
	}
	if !samePath(refreshedSibling.WorkspacePath, repo) {
		t.Errorf("sibling WorkspacePath = %q, want project root %q", refreshedSibling.WorkspacePath, repo)
	}
	if refreshedSibling.WorktreePath != "" {
		t.Errorf("sibling WorktreePath = %q, want empty", refreshedSibling.WorktreePath)
	}
}

func TestRemoveOtherWorktreeBroadcastsSiblingUpdates(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-remove-broadcast-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/broadcast")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	sibling := testThread("thread-remove-broadcast-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = worktreePath
	sibling.WorktreePath = worktreePath
	sibling.Branch = "feature/broadcast"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}

	// Capture every thread:updated broadcast — siblings depend on this
	// event to know their workspace got reset to the project root.
	var mu sync.Mutex
	var broadcast []string
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if evt, ok := data.(triage.ThreadUpdateEvent); ok {
			if evt.Thread != nil {
				broadcast = append(broadcast, evt.Thread.ID)
			} else if evt.ID != "" {
				broadcast = append(broadcast, evt.ID)
			}
		}
	}

	if err := app.RemoveOtherWorktree(owner.ID, worktreePath, true); err != nil {
		t.Fatalf("RemoveOtherWorktree() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	siblingBroadcast := false
	for _, id := range broadcast {
		if id == sibling.ID {
			siblingBroadcast = true
			break
		}
	}
	if !siblingBroadcast {
		t.Fatalf("expected thread:updated broadcast for sibling %q; got %v", sibling.ID, broadcast)
	}
}

func containsWorktree(worktrees []gitops.Worktree, want string) bool {
	for _, worktree := range worktrees {
		if samePath(worktree.Path, want) {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	return testutil.CanonicalPath(nil, left) == testutil.CanonicalPath(nil, right)
}
