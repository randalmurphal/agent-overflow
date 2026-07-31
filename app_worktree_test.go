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
	"agent-overflow/internal/provider/claude/sessionfork"
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
	seedWorktreeTestAnchor(t, app, thread.ID, "root-before-worktree", "user:0", 0)

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
	// Message anchors are workspace-independent: switching into the
	// worktree must keep them.
	assertThreadMessageAnchorCount(t, app, thread.ID, 1)

	seedWorktreeTestAnchor(t, app, thread.ID, "worktree-before-removal", "user:1", 1)

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
	assertThreadMessageAnchorCount(t, app, thread.ID, 2)
}

func TestAttachThreadWorktreeKeepsMessageAnchors(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/attach")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-attach-worktree-anchors")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	seedWorktreeTestAnchor(t, app, thread.ID, "root-before-attach", "user:0", 0)

	updated, err := app.AttachThreadWorktree(thread.ID, "feature/attach")
	if err != nil {
		t.Fatalf("AttachThreadWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, updated.WorktreePath, true)
	})
	if !samePath(updated.WorkspacePath, updated.WorktreePath) || updated.WorktreePath == "" {
		t.Fatalf("updated workspace/worktree = %q/%q, want attached worktree", updated.WorkspacePath, updated.WorktreePath)
	}
	assertThreadMessageAnchorCount(t, app, thread.ID, 1)
}

func TestSwitchThreadWorkspaceKeepsMessageAnchors(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-switch-workspace-anchors")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/switch-workspace")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})
	seedWorktreeTestAnchor(t, app, thread.ID, "worktree-before-switch", "user:0", 0)

	updated, err := app.switchThreadWorkspace(thread.ID, repo)
	if err != nil {
		t.Fatalf("switchThreadWorkspace() error = %v", err)
	}
	if !samePath(updated.WorkspacePath, repo) || updated.WorktreePath != "" {
		t.Fatalf("updated workspace/worktree = %q/%q, want project root", updated.WorkspacePath, updated.WorktreePath)
	}
	assertThreadMessageAnchorCount(t, app, thread.ID, 1)
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

func TestGetBranchBaseDiffIncludesCommittedAndUncommittedChanges(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	thread := createGitDiffTestThread(t, app, repo, "thread-branch-base-diff")

	testutil.RunGit(t, repo, "checkout", "-b", "feature/review")
	if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("committed branch work\n"), 0o644); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	testutil.RunGit(t, repo, "add", "committed.txt")
	testutil.RunGit(t, repo, "commit", "-m", "feature commit")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nuncommitted edit\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked branch work\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	diff, err := app.GetBranchBaseDiff(thread.ID, "main")
	if err != nil {
		t.Fatalf("GetBranchBaseDiff() error = %v", err)
	}
	for _, want := range []string{
		"committed.txt",
		"+committed branch work",
		"+uncommitted edit",
		"untracked.txt",
		"+untracked branch work",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}
}

func TestGetBranchBaseDiffBaseEqualsCurrentShowsOnlyUncommitted(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	thread := createGitDiffTestThread(t, app, repo, "thread-branch-base-current")

	if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("main work\n"), 0o644); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	testutil.RunGit(t, repo, "add", "committed.txt")
	testutil.RunGit(t, repo, "commit", "-m", "main commit")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nworkspace edit\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	diff, err := app.GetBranchBaseDiff(thread.ID, "main")
	if err != nil {
		t.Fatalf("GetBranchBaseDiff() error = %v", err)
	}
	if strings.Contains(diff, "committed.txt") || strings.Contains(diff, "+main work") {
		t.Fatalf("diff included committed current-branch work:\n%s", diff)
	}
	if !strings.Contains(diff, "+workspace edit") {
		t.Fatalf("diff missing workspace edit:\n%s", diff)
	}
}

func TestGetBranchBaseDiffMissingBranchErrors(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	thread := createGitDiffTestThread(t, app, repo, "thread-branch-base-missing")

	_, err := app.GetBranchBaseDiff(thread.ID, "missing-branch")
	if err == nil {
		t.Fatal("GetBranchBaseDiff() error = nil, want missing branch error")
	}
	if !strings.Contains(err.Error(), `get branch base diff: gitdiff: branch "missing-branch" not found locally or on any remote`) {
		t.Fatalf("error = %v, want wrapped branch-not-found context", err)
	}
}

func createGitDiffTestThread(t *testing.T, app *App, repo string, threadID string) store.Thread {
	t.Helper()
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread(threadID)
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return thread
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

// The removal gate is per-worktree, not per-caller: a thread that is
// mid-turn may still remove a worktree no running thread occupies. The
// old behavior activity-checked the caller unconditionally, forcing the
// user to wait out their own turn before cleaning up unrelated worktrees.
func TestRemoveOtherWorktreeAllowsBusyCallerOnUnoccupiedWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	caller := testThread("thread-remove-busy-caller")
	caller.ProjectID = project.ID
	caller.WorkspacePath = repo
	caller.Branch = "main"
	if err := app.store.CreateThread(caller); err != nil {
		t.Fatalf("CreateThread(caller): %v", err)
	}

	// Registered worktree with no attached threads.
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-idle-cleanup")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/idle", worktreePath)
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	// Caller is mid-turn; the worktree it wants to remove is not its own.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-remove-busy-caller",
		ThreadID:  caller.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	if err := app.RemoveOtherWorktree(caller.ID, worktreePath, false); err != nil {
		t.Fatalf("RemoveOtherWorktree() during caller's own turn error = %v, want success for unoccupied worktree", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree %q should be gone after removal, stat err = %v", worktreePath, err)
	}
	// The busy caller was never attached to the removed worktree, so its
	// workspace must be untouched.
	refreshed, err := app.store.GetThread(caller.ID)
	if err != nil {
		t.Fatalf("GetThread(caller): %v", err)
	}
	if !samePath(refreshed.WorkspacePath, repo) {
		t.Errorf("caller WorkspacePath = %q, want unchanged %q", refreshed.WorkspacePath, repo)
	}
}

// The removal gate refuses on running background tasks, not just active
// turns — a backgrounded Codex terminal has its cwd inside the worktree.
// Also pins the toast contract: the final `: `-segment of the error must
// stand alone because the frontend keeps only that segment.
func TestRemoveOtherWorktreeRejectsRunningBackgroundTaskOnSibling(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-remove-bg-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/bg-busy")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	sibling := testThread("thread-remove-bg-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = worktreePath
	sibling.WorktreePath = worktreePath
	sibling.Branch = "feature/bg-busy"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}
	insertRunningBackgroundToolCall(t, app.store, sibling.ID, "item-bg-live", 0, 0)

	err = app.RemoveOtherWorktree(owner.ID, worktreePath, true)
	if err == nil {
		t.Fatal("expected RemoveOtherWorktree to refuse while sibling has a running background task")
	}
	if !strings.Contains(err.Error(), sibling.ID) {
		t.Fatalf("error should name the busy sibling: got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot remove worktree while 1 background task(s) are running") {
		t.Fatalf("final error segment should be self-contained about background tasks: got %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should still exist after refused remove: %v", err)
	}
}

// The placeholder-pane path (no caller thread row) must apply the same
// occupancy gate — callerThreadID=="" skips the caller lock append, never
// the attached-thread check.
func TestRemoveOtherWorktreeForProjectRejectsBusyOccupant(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-project-busy")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/project-busy", worktreePath)
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	occupant := testThread("thread-project-busy-occupant")
	occupant.ProjectID = project.ID
	occupant.WorkspacePath = worktreePath
	occupant.WorktreePath = worktreePath
	occupant.Branch = "feature/project-busy"
	if err := app.store.CreateThread(occupant); err != nil {
		t.Fatalf("CreateThread(occupant): %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-project-busy",
		ThreadID:  occupant.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	_, err = app.RemoveOtherWorktreeForProject(project.ID, "", worktreePath, true)
	if err == nil {
		t.Fatal("expected placeholder-path removal to refuse while an occupant is mid-turn")
	}
	if !strings.Contains(err.Error(), occupant.ID) {
		t.Fatalf("error should name the busy occupant: got %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should still exist after refused remove: %v", err)
	}
}

// Occupancy matches on WorkspacePath alone — a thread can sit inside a
// worktree without a WorktreePath of its own (e.g. reattached state).
// Busy: gate refuses. Idle: the sweep's workspace-only branch reattaches
// it to the project root with the branch reset.
func TestRemoveOtherWorktreeHandlesWorkspaceOnlyOccupant(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	caller := testThread("thread-wsonly-caller")
	caller.ProjectID = project.ID
	caller.WorkspacePath = repo
	if err := app.store.CreateThread(caller); err != nil {
		t.Fatalf("CreateThread(caller): %v", err)
	}
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-ws-only")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/ws-only", worktreePath)
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	occupant := testThread("thread-wsonly-occupant")
	occupant.ProjectID = project.ID
	occupant.WorkspacePath = worktreePath
	occupant.WorktreePath = ""
	occupant.Branch = "feature/ws-only"
	if err := app.store.CreateThread(occupant); err != nil {
		t.Fatalf("CreateThread(occupant): %v", err)
	}
	insertRunningBackgroundToolCall(t, app.store, occupant.ID, "item-wsonly-live", 0, 0)

	err = app.RemoveOtherWorktree(caller.ID, worktreePath, true)
	if err == nil || !strings.Contains(err.Error(), occupant.ID) {
		t.Fatalf("expected refusal naming the workspace-only occupant, got %v", err)
	}

	// Drain the background task; the same removal must now succeed and
	// sweep the workspace-only occupant back to the project root.
	if _, err := app.store.MarkLiveBackgroundToolCallsInactive(occupant.ID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("MarkLiveBackgroundToolCallsInactive(): %v", err)
	}
	if err := app.RemoveOtherWorktree(caller.ID, worktreePath, true); err != nil {
		t.Fatalf("RemoveOtherWorktree() after drain error = %v", err)
	}
	refreshed, err := app.store.GetThread(occupant.ID)
	if err != nil {
		t.Fatalf("GetThread(occupant): %v", err)
	}
	if !samePath(refreshed.WorkspacePath, repo) {
		t.Errorf("occupant WorkspacePath = %q, want %q", refreshed.WorkspacePath, repo)
	}
	if refreshed.WorktreePath != "" {
		t.Errorf("occupant WorktreePath = %q, want empty", refreshed.WorktreePath)
	}
	if refreshed.Branch != "main" {
		t.Errorf("occupant Branch = %q, want main", refreshed.Branch)
	}
}

// Membership is validated even with force=true — force skips the
// loss-of-work gate, not the "registered worktree of this project"
// boundary. Without it the only guard against force-removing an
// arbitrary directory is git's own refusal.
func TestRemoveOtherWorktreeForceRejectsNonWorktreePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-remove-nonworktree")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	victim := filepath.Join(t.TempDir(), "not-a-worktree")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	err = app.RemoveOtherWorktree(thread.ID, victim, true)
	if err == nil {
		t.Fatal("expected force removal of a non-worktree path to be refused")
	}
	if !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("error = %v, want membership refusal", err)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("victim directory must be untouched: %v", statErr)
	}
}

// Regression: the lock set is a clone of the attached set. When they shared
// a backing array, appending a busy caller whose ID sorts before an attached
// thread's ID let the in-place sort swap the caller into attached's view —
// the gate then refused on the caller's own activity (the exact behavior
// this feature removes) and the reattach sweep dropped a genuinely attached
// thread. Needs several attached threads (len < cap after append growth)
// plus a low-sorting busy caller to reproduce.
func TestRemoveOtherWorktreeBusyCallerReattachesAllIdleOccupants(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	// "thread-aaa-caller" sorts before every "thread-idle-N" occupant.
	caller := testThread("thread-aaa-caller")
	caller.ProjectID = project.ID
	caller.WorkspacePath = repo
	caller.Branch = "main"
	if err := app.store.CreateThread(caller); err != nil {
		t.Fatalf("CreateThread(caller): %v", err)
	}

	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-multi-idle")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/multi-idle", worktreePath)
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	occupants := []string{"thread-idle-1", "thread-idle-2", "thread-idle-3"}
	for _, id := range occupants {
		occ := testThread(id)
		occ.ProjectID = project.ID
		occ.WorkspacePath = worktreePath
		occ.WorktreePath = worktreePath
		occ.Branch = "feature/multi-idle"
		if err := app.store.CreateThread(occ); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}

	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-aaa-caller",
		ThreadID:  caller.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	if err := app.RemoveOtherWorktree(caller.ID, worktreePath, false); err != nil {
		t.Fatalf("RemoveOtherWorktree() error = %v, want success — caller is busy but not attached, occupants are idle", err)
	}
	for _, id := range occupants {
		occ, err := app.store.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", id, err)
		}
		if !samePath(occ.WorkspacePath, repo) {
			t.Errorf("occupant %s WorkspacePath = %q, want reattached to %q", id, occ.WorkspacePath, repo)
		}
		if occ.WorktreePath != "" {
			t.Errorf("occupant %s WorktreePath = %q, want cleared", id, occ.WorktreePath)
		}
	}
}

// The flip side: a busy caller still cannot remove the worktree it
// occupies — it is one of the attached threads the per-worktree gate
// checks, and removal would reattach it mid-turn.
func TestGitRemoveWorktreeRejectsBusyCallerOnOwnWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-remove-own-busy")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(thread.ID, "feature/own-busy")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-remove-own-busy",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	err = app.GitRemoveWorktree(thread.ID)
	if err == nil {
		t.Fatal("expected GitRemoveWorktree to refuse while the occupying caller is mid-turn")
	}
	if !strings.Contains(err.Error(), thread.ID) {
		t.Fatalf("error should name the busy occupying thread: got %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should still exist after refused remove: %v", err)
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
	worktrees, err := app.GitListWorktrees(owner.ID)
	if err != nil {
		t.Fatalf("GitListWorktrees() error = %v", err)
	}
	foundWorktree := false
	for _, worktree := range worktrees {
		if samePath(worktree.Path, worktreePath) {
			foundWorktree = true
			if !worktree.DeleteBlocked {
				t.Fatal("DeleteBlocked = false, want true for active attached sibling")
			}
		}
	}
	if !foundWorktree {
		t.Fatalf("worktree %q not found in list: %+v", worktreePath, worktrees)
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

func TestGitListWorktreesMarksDeleteBlockedBySiblingBackgroundTask(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-list-background-owner")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/background-blocked")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	sibling := testThread("thread-list-background-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = worktreePath
	sibling.WorktreePath = worktreePath
	sibling.Branch = "feature/background-blocked"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread(sibling): %v", err)
	}
	insertRunningBackgroundToolCall(t, app.store, sibling.ID, "bg-list-worktree", 0, 0)

	worktrees, err := app.GitListWorktrees(owner.ID)
	if err != nil {
		t.Fatalf("GitListWorktrees() error = %v", err)
	}
	for _, worktree := range worktrees {
		if samePath(worktree.Path, worktreePath) {
			if !worktree.DeleteBlocked {
				t.Fatal("DeleteBlocked = false, want true for attached sibling background task")
			}
			return
		}
	}
	t.Fatalf("worktree %q not found in list: %+v", worktreePath, worktrees)
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

func containsWorktree(worktrees []WorktreeListItem, want string) bool {
	for _, worktree := range worktrees {
		if samePath(worktree.Path, want) {
			return true
		}
	}
	return false
}

func seedWorktreeTestAnchor(t *testing.T, app *App, threadID, summary, userItemID string, turnIndex int) {
	t.Helper()
	if err := app.store.InsertItem(store.Item{
		ID:        userItemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   summary,
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem(%s): %v", userItemID, err)
	}
	seedMessageAnchor(t, app.store, threadID, userItemID, turnIndex, "", "")
}

// assertThreadMessageAnchorCount pins the workspace-change posture:
// message anchors correlate messages to provider history, not to a
// workspace, so worktree attach/switch/remove must leave them intact.
func assertThreadMessageAnchorCount(t *testing.T, app *App, threadID string, want int) {
	t.Helper()
	anchors, err := app.store.ListMessageAnchors(threadID)
	if err != nil {
		t.Fatalf("ListMessageAnchors(%s): %v", threadID, err)
	}
	if len(anchors) != want {
		t.Fatalf("message anchors for %s = %d, want %d", threadID, len(anchors), want)
	}
}

func samePath(left, right string) bool {
	return testutil.CanonicalPath(nil, left) == testutil.CanonicalPath(nil, right)
}

// claudeProjectSlugForTest mirrors Claude's sanitizePath (and sessionfork's
// exactWorkspaceSlug) for the common <=200-char case: canonicalize, then map
// every non-alphanumeric rune to '-'.
func claudeProjectSlugForTest(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("evalsymlinks %s: %v", path, err)
	}
	abs, err := filepath.Abs(canonical)
	if err != nil {
		t.Fatalf("abs %s: %v", canonical, err)
	}
	var b strings.Builder
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Worktree removal must NOT brick a Claude thread's session. Claude resolves
// --resume against the slug of the current cwd, so reattaching to the project
// root would strand the transcript under the deleted worktree's slug and the
// next resume would fail with "No conversation found". The sweep relocates the
// transcript to the root's slug AND preserves the session ref — it must never
// clear the ref or silently start a fresh session.
func TestGitRemoveWorktreeRelocatesClaudeSessionAndKeepsRef(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread("thread-claude-relocate")
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	owner.Provider = string(provider.Claude)
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/relocate")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	// Point HOME at a temp ~/.claude AFTER git setup — InitGitRepo uses
	// repo-local git config, so this doesn't disturb git. Simulate a session
	// born inside the worktree: its transcript lives only under the worktree's
	// slug.
	home := t.TempDir()
	t.Setenv("HOME", home)

	const sessionID = "01079734-relocate-test"
	live, err := app.store.GetThread(owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	live.WorkspacePath = worktreePath
	live.WorktreePath = worktreePath
	live.SessionRef = sessionID
	if err := app.store.UpdateThread(live); err != nil {
		t.Fatalf("UpdateThread(set session) error = %v", err)
	}

	wtDir := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, worktreePath))
	if err := os.MkdirAll(wtDir, 0o700); err != nil {
		t.Fatalf("mkdir worktree slug dir: %v", err)
	}
	srcTranscript := filepath.Join(wtDir, sessionID+".jsonl")
	if err := os.WriteFile(srcTranscript, []byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := app.GitRemoveWorktree(owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	// Transcript relocated under the project-root slug, so resume with cwd ==
	// repo resolves it.
	wantDest := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, repo), sessionID+".jsonl")
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("transcript not relocated to project-root slug %q: %v", wantDest, err)
	}
	if located, err := sessionfork.LocateSessionFile(sessionID, repo); err != nil {
		t.Errorf("LocateSessionFile from repo root: %v", err)
	} else if !samePath(located, wantDest) {
		t.Errorf("LocateSessionFile = %q, want relocated %q", located, wantDest)
	}

	// Move, not copy: the stale transcript under the deleted worktree's slug is
	// purged so exactly one copy follows the thread (no orphan left behind on the
	// dead slug, no stale copy the locate fallback could later surface).
	if _, err := os.Stat(srcTranscript); !os.IsNotExist(err) {
		t.Errorf("old worktree-slug transcript should be purged after move, stat err = %v", err)
	}

	// The session ref MUST survive — relocation preserves the conversation; it
	// never clears the ref or starts fresh (the whole point of the fix).
	refreshed, err := app.store.GetThread(owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q (must not clear / start fresh)", refreshed.SessionRef, sessionID)
	}
	if !samePath(refreshed.WorkspacePath, repo) {
		t.Errorf("WorkspacePath = %q, want project root %q", refreshed.WorkspacePath, repo)
	}
}

// relocateTestEnv is the shared scaffolding for the worktree-removal relocation
// tests: a thread attached to a fresh worktree with HOME pointed at a temp
// ~/.claude.
type relocateTestEnv struct {
	app          *App
	repo         string
	owner        store.Thread
	worktreePath string
	home         string
}

// setupWorktreeThreadForRelocate builds a thread on a fresh worktree for the
// given provider. HOME is set AFTER git init (InitGitRepo uses repo-local git
// config, so the temp HOME doesn't disturb git) and points at an empty dir, so
// each test controls exactly what lives under ~/.claude.
func setupWorktreeThreadForRelocate(t *testing.T, name, providerName string) relocateTestEnv {
	t.Helper()
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread(name)
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	owner.Provider = providerName
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	worktreePath, err := app.GitCreateWorktree(owner.ID, "feature/"+name)
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return relocateTestEnv{app: app, repo: repo, owner: owner, worktreePath: worktreePath, home: home}
}

// placeWorktreeTranscript writes a minimal transcript for id under workspace's
// Claude project slug — the dir Claude would have written it to while the
// thread ran in that workspace.
func placeWorktreeTranscript(t *testing.T, home, workspace, id string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, workspace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir slug dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatalf("write transcript %s: %v", id, err)
	}
}

// attachSessionToWorktree points the thread at the worktree and sets its
// session refs, mirroring a session that ran inside the worktree before removal.
func attachSessionToWorktree(t *testing.T, env relocateTestEnv, sessionRef, pendingForkRef string) {
	t.Helper()
	live, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	live.WorkspacePath = env.worktreePath
	live.WorktreePath = env.worktreePath
	live.SessionRef = sessionRef
	live.PendingForkRef = pendingForkRef
	if err := env.app.store.UpdateThread(live); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}
}

// Worktree removal must relocate a thread's PendingForkRef transcript too, not
// just SessionRef. The lazy-fork resume (`--fork-session`) reads the forked
// transcript at the new cwd slug, so leaving it under the deleted worktree
// would brick the pending fork. Both refs must land under the root slug and
// survive on the thread row.
func TestGitRemoveWorktreeRelocatesPendingForkRef(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "thread-fork-relocate", string(provider.Claude))

	const sessionID = "session-keep"
	const forkID = "pending-fork-keep"
	attachSessionToWorktree(t, env, sessionID, forkID)
	placeWorktreeTranscript(t, env.home, env.worktreePath, sessionID)
	placeWorktreeTranscript(t, env.home, env.worktreePath, forkID)

	if err := env.app.GitRemoveWorktree(env.owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	for _, id := range []string{sessionID, forkID} {
		want := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, env.repo), id+".jsonl")
		if located, err := sessionfork.LocateSessionFile(id, env.repo); err != nil {
			t.Errorf("LocateSessionFile(%s) from repo root: %v", id, err)
		} else if !samePath(located, want) {
			t.Errorf("LocateSessionFile(%s) = %q, want relocated %q", id, located, want)
		}
	}

	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q", refreshed.SessionRef, sessionID)
	}
	if refreshed.PendingForkRef != forkID {
		t.Errorf("PendingForkRef = %q, want preserved %q", refreshed.PendingForkRef, forkID)
	}
}

// claude-tui runs the SAME `claude` CLI binary with cwd-keyed `--resume` and
// persists its SessionRef through the same handleInit path as the headless
// claude provider, so it bricks on a workspace change identically — and must
// relocate identically. Regression guard for the provider gate in
// copyClaudeSessionForWorkspaceChange: scoping that gate to provider.Claude
// alone silently strands every claude-tui transcript (this test fails on both
// the relocate and the purge assertion without the claude-tui branch of the
// gate).
func TestGitRemoveWorktreeRelocatesClaudeTUISession(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "thread-claude-tui-relocate", string(provider.ClaudeTUI))

	const sessionID = "claude-tui-session-keep"
	attachSessionToWorktree(t, env, sessionID, "")
	placeWorktreeTranscript(t, env.home, env.worktreePath, sessionID)
	srcTranscript := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, env.worktreePath), sessionID+".jsonl")

	if err := env.app.GitRemoveWorktree(env.owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	// Relocated under the project-root slug so `claude --resume` with cwd == repo
	// resolves it.
	wantDest := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, env.repo), sessionID+".jsonl")
	if located, err := sessionfork.LocateSessionFile(sessionID, env.repo); err != nil {
		t.Errorf("LocateSessionFile from repo root: %v", err)
	} else if !samePath(located, wantDest) {
		t.Errorf("LocateSessionFile = %q, want relocated %q", located, wantDest)
	}

	// Move, not copy: the worktree-slug source is purged so exactly one copy
	// follows the thread.
	if _, err := os.Stat(srcTranscript); !os.IsNotExist(err) {
		t.Errorf("old worktree-slug transcript should be purged after move, stat err = %v", err)
	}

	// Ref preserved — relocation never clears the ref or starts a fresh session.
	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q (must not clear / start fresh)", refreshed.SessionRef, sessionID)
	}
}

// The user's hard rule: "rather brick a session than default to a fresh
// session." When the transcript is already gone from disk, worktree removal
// must still SUCCEED, leave the SessionRef intact, and leave the session
// genuinely unresolvable — never cleared, never replaced with a fresh one.
func TestGitRemoveWorktreeMissingTranscriptPreservesRefAndDoesNotFabricate(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "thread-missing-transcript", string(provider.Claude))

	// A populated ~/.claude/projects but NO transcript for this session.
	if err := os.MkdirAll(filepath.Join(env.home, ".claude", "projects"), 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	const sessionID = "01079734-gone"
	attachSessionToWorktree(t, env, sessionID, "")

	// Removal succeeds even though there is nothing to relocate.
	if err := env.app.GitRemoveWorktree(env.owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() must succeed when transcript is already gone, got %v", err)
	}

	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	// Ref preserved — never cleared, never swapped for a fresh session.
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q (must not clear / fabricate fresh)", refreshed.SessionRef, sessionID)
	}
	if !samePath(refreshed.WorkspacePath, env.repo) {
		t.Errorf("WorkspacePath = %q, want project root %q", refreshed.WorkspacePath, env.repo)
	}
	// Positive proof of "bricked, not fabricated": the session stays
	// unresolvable from the reattach target.
	if _, err := sessionfork.LocateSessionFile(sessionID, env.repo); !errors.Is(err, sessionfork.ErrSessionFileNotFound) {
		t.Errorf("LocateSessionFile err = %v, want ErrSessionFileNotFound (bricked, not silently rehomed)", err)
	}
}

// Relocation is Claude-only. A Codex thread (resumes by thread id from
// ~/.codex, not a cwd-keyed slug) must pass through worktree removal untouched:
// removal succeeds, the ref is preserved, and no ~/.claude/projects dir is
// created — proof the Claude-only relocation never ran.
func TestGitRemoveWorktreeCodexThreadSkipsRelocation(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "thread-codex-noop", string(provider.Codex))

	const sessionID = "codex-thread-xyz"
	attachSessionToWorktree(t, env, sessionID, "")

	if err := env.app.GitRemoveWorktree(env.owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() error = %v", err)
	}

	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q", refreshed.SessionRef, sessionID)
	}
	if !samePath(refreshed.WorkspacePath, env.repo) {
		t.Errorf("WorkspacePath = %q, want project root %q", refreshed.WorkspacePath, env.repo)
	}
	if _, statErr := os.Stat(filepath.Join(env.home, ".claude", "projects")); !os.IsNotExist(statErr) {
		t.Errorf("~/.claude/projects must not exist for a Codex thread (stat err = %v)", statErr)
	}
}

// A subagent-subdir copy failure must NOT fail the whole worktree removal: the
// transcript itself relocated, so --resume works, and only subagent history is
// partial. The sweep logs it and continues. Reverting the ErrSubagentCopyIncomplete
// switch arm to `return err` makes GitRemoveWorktree fail here — that's the
// regression this guards (the package-level E4 only proves the sentinel is
// returned, not that the app caller swallows it).
func TestGitRemoveWorktreeSubagentCopyFailureDoesNotFailReattach(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "thread-subagent-fail", string(provider.Claude))

	const sessionID = "01079734-subfail"
	attachSessionToWorktree(t, env, sessionID, "")

	// Source: transcript + a subagent subdir, both under the worktree slug.
	placeWorktreeTranscript(t, env.home, env.worktreePath, sessionID)
	wtSlugDir := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, env.worktreePath))
	if err := os.MkdirAll(filepath.Join(wtSlugDir, sessionID), 0o700); err != nil {
		t.Fatalf("mkdir src subagent subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtSlugDir, sessionID, "agent.jsonl"), []byte("sub\n"), 0o600); err != nil {
		t.Fatalf("write src subagent: %v", err)
	}

	// Block ONLY the subagent copy at the destination (repo-root slug):
	// pre-create the <id>/ path as a regular file so copyTree's MkdirAll fails
	// while <id>.jsonl still copies cleanly.
	rootSlugDir := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, env.repo))
	if err := os.MkdirAll(rootSlugDir, 0o700); err != nil {
		t.Fatalf("mkdir root slug dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootSlugDir, sessionID), []byte("blocker\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// Removal must SUCCEED despite the subagent-subdir copy failure.
	if err := env.app.GitRemoveWorktree(env.owner.ID); err != nil {
		t.Fatalf("GitRemoveWorktree() must succeed when only the subagent subdir copy fails, got %v", err)
	}

	// The transcript itself relocated, so resume resolves at the root slug.
	wantDest := filepath.Join(rootSlugDir, sessionID+".jsonl")
	if located, err := sessionfork.LocateSessionFile(sessionID, env.repo); err != nil {
		t.Errorf("LocateSessionFile from repo root: %v", err)
	} else if !samePath(located, wantDest) {
		t.Errorf("LocateSessionFile = %q, want relocated %q", located, wantDest)
	}
	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q", refreshed.SessionRef, sessionID)
	}

	// Soft-failure contract: because the subagent copy failed, the destination
	// holds only the main transcript — the source <id>/ subdir is the ONLY
	// complete copy of the subagent history. The helper must therefore NOT purge
	// the source on the soft path. (The source lives under HOME/.claude/projects,
	// not inside the removed worktree, so it is still on disk to assert against.)
	// Flipping the ErrSubagentCopyIncomplete arm to append the source to the
	// purge list would delete this and silently lose subagent history.
	if _, err := os.Stat(filepath.Join(wtSlugDir, sessionID, "agent.jsonl")); err != nil {
		t.Errorf("source subagent history must survive a soft subagent-copy failure (no purge): %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtSlugDir, sessionID+".jsonl")); err != nil {
		t.Errorf("source transcript must survive a soft subagent-copy failure (no purge): %v", err)
	}
}

// setupRootThreadForRelocate builds a Claude thread anchored at the project ROOT
// (no worktree yet) with a session transcript already on disk under the root
// slug, then returns the env. This is the precondition for the create/attach
// relocation tests: a live session born in the root that a workspace change must
// carry to the new worktree slug. HOME is set AFTER git init (repo-local config),
// so the temp ~/.claude holds exactly the transcript we stage.
func setupRootThreadForRelocate(t *testing.T, name, sessionID string) (env relocateTestEnv, srcTranscript string) {
	t.Helper()
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	// Pre-create the branch the attach test will point at; harmless for create.
	testutil.RunGit(t, repo, "branch", "feature/"+name)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	owner := testThread(name)
	owner.ProjectID = project.ID
	owner.WorkspacePath = repo
	owner.Provider = string(provider.Claude)
	owner.SessionRef = sessionID
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	placeWorktreeTranscript(t, home, repo, sessionID)
	srcTranscript = filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, repo), sessionID+".jsonl")
	return relocateTestEnv{app: app, repo: repo, owner: owner, home: home}, srcTranscript
}

// assertSessionMovedTo verifies the move landed exactly one transcript at the
// destination workspace's slug, resolvable by --resume from there, with the
// pre-move source purged and the session ref preserved on the thread row.
func assertSessionMovedTo(t *testing.T, env relocateTestEnv, destWorkspace, srcTranscript, sessionID string) {
	t.Helper()
	wantDest := filepath.Join(env.home, ".claude", "projects", claudeProjectSlugForTest(t, destWorkspace), sessionID+".jsonl")
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("transcript not relocated to destination slug %q: %v", wantDest, err)
	}
	if located, err := sessionfork.LocateSessionFile(sessionID, destWorkspace); err != nil {
		t.Errorf("LocateSessionFile from destination: %v", err)
	} else if !samePath(located, wantDest) {
		t.Errorf("LocateSessionFile = %q, want relocated %q", located, wantDest)
	}
	if _, err := os.Stat(srcTranscript); !os.IsNotExist(err) {
		t.Errorf("pre-move source transcript should be purged, stat err = %v", err)
	}
	refreshed, err := env.app.store.GetThread(env.owner.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if refreshed.SessionRef != sessionID {
		t.Errorf("SessionRef = %q, want preserved %q (must not clear / start fresh)", refreshed.SessionRef, sessionID)
	}
	if !samePath(refreshed.WorkspacePath, destWorkspace) {
		t.Errorf("WorkspacePath = %q, want destination %q", refreshed.WorkspacePath, destWorkspace)
	}
}

// Creating a worktree changes the thread's cwd, so Claude would resolve --resume
// against the new worktree slug — where the root-born transcript doesn't live.
// PrepareThreadWorktree must carry it to the worktree slug (and purge the root
// copy) so resume keeps working; it never clears the ref or starts fresh.
func TestPrepareThreadWorktreeRelocatesClaudeSession(t *testing.T) {
	const sessionID = "01079734-create"
	env, srcTranscript := setupRootThreadForRelocate(t, "create-relocate", sessionID)

	worktreePath, err := env.app.GitCreateWorktree(env.owner.ID, "feature/new-create")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}
	assertSessionMovedTo(t, env, worktreePath, srcTranscript, sessionID)
}

// Attaching to an existing branch's worktree changes the thread's cwd the same
// way create does — the root-born transcript must move to the attached worktree
// slug, the root copy is purged, and the ref survives.
func TestAttachThreadWorktreeRelocatesClaudeSession(t *testing.T) {
	const sessionID = "01079734-attach"
	env, srcTranscript := setupRootThreadForRelocate(t, "attach-relocate", sessionID)

	updated, err := env.app.AttachThreadWorktree(env.owner.ID, "feature/attach-relocate")
	if err != nil {
		t.Fatalf("AttachThreadWorktree() error = %v", err)
	}
	assertSessionMovedTo(t, env, updated.WorktreePath, srcTranscript, sessionID)
}

// slugTranscriptPath returns the on-disk path Claude would resume from for id
// when cwd == workspace.
func slugTranscriptPath(t *testing.T, home, workspace, id string) string {
	t.Helper()
	return filepath.Join(home, ".claude", "projects", claudeProjectSlugForTest(t, workspace), id+".jsonl")
}

// writeTranscriptContent writes exact bytes to the transcript slug path for id
// under workspace, creating the slug dir. Used by the round-trip test to seed
// and then grow specific transcript content (placeWorktreeTranscript only writes
// a fixed one-line body).
func writeTranscriptContent(t *testing.T, home, workspace, id, content string) {
	t.Helper()
	path := slugTranscriptPath(t, home, workspace, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir slug dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript content: %v", err)
	}
}

// The switch-back data-loss trap. A thread that visits a workspace, accrues
// turns elsewhere, then returns must resume the LATEST transcript — not a stale
// copy left under the destination slug from the earlier visit. The move
// overwrites the destination and purges the source on every switch, so exactly
// one authoritative copy follows the cwd. The pre-fix helper (copy-if-absent,
// never purge) failed this: on the return switch it saw a stale destination,
// no-op'd, and resume silently dropped the turns taken away. This test plants
// that exact stale copy and asserts it is overwritten.
func TestSwitchThreadWorkspaceRoundTripPreservesLatestTurns(t *testing.T) {
	env := setupWorktreeThreadForRelocate(t, "roundtrip", string(provider.Claude))

	const sessionID = "01079734-roundtrip"
	attachSessionToWorktree(t, env, sessionID, "")
	// Session born in the worktree with one turn.
	writeTranscriptContent(t, env.home, env.worktreePath, sessionID, "turn1\n")

	// Switch worktree -> root: the transcript moves to the root slug; the
	// worktree copy is purged.
	if _, err := env.app.switchThreadWorkspace(env.owner.ID, env.repo); err != nil {
		t.Fatalf("switchThreadWorkspace(->root) error = %v", err)
	}
	rootPath := slugTranscriptPath(t, env.home, env.repo, sessionID)
	if _, err := os.Stat(rootPath); err != nil {
		t.Fatalf("transcript not at root slug after first switch: %v", err)
	}

	// Claude appends a turn while the thread runs in the root.
	if err := os.WriteFile(rootPath, []byte("turn1\nturn2\n"), 0o600); err != nil {
		t.Fatalf("grow root transcript: %v", err)
	}
	// Plant a STALE leftover at the worktree slug — as if an earlier purge had
	// failed. The return switch must overwrite it, not resume it.
	writeTranscriptContent(t, env.home, env.worktreePath, sessionID, "STALE-turn1-only\n")

	// Switch root -> worktree: the latest root transcript overwrites the stale
	// worktree copy and the root copy is purged.
	if _, err := env.app.switchThreadWorkspace(env.owner.ID, env.worktreePath); err != nil {
		t.Fatalf("switchThreadWorkspace(->worktree) error = %v", err)
	}

	wtPath := slugTranscriptPath(t, env.home, env.worktreePath, sessionID)
	got, err := os.ReadFile(wtPath)
	if err != nil {
		t.Fatalf("read worktree transcript after round trip: %v", err)
	}
	if string(got) != "turn1\nturn2\n" {
		t.Errorf("worktree transcript = %q, want latest %q (stale copy must be overwritten, not resumed)", got, "turn1\nturn2\n")
	}
	if _, err := os.Stat(rootPath); !os.IsNotExist(err) {
		t.Errorf("root copy should be purged after switch back, stat err = %v", err)
	}
}

// Guard-and-refuse at the helper boundary: when the destination workspace's slug
// is unresolvable (sanitized form exceeds MaxSanitizedSlugLen, where Claude
// appends an unreproducible Bun.hash suffix), copyClaudeSessionForWorkspaceChange
// must return a hard error with NO purge list and leave the source transcript
// untouched — so an abortable caller (switch/create/attach) can refuse the change
// with the conversation still resumable from its current workspace.
func TestCopyClaudeSessionForWorkspaceChangeRefusesUnresolvableDest(t *testing.T) {
	app := newTestAppWithStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	fromWS := t.TempDir()
	const sessionID = "01079734-overlength"
	placeWorktreeTranscript(t, home, fromWS, sessionID)
	srcTranscript := slugTranscriptPath(t, home, fromWS, sessionID)

	// Destination dir must EXIST (exactWorkspaceSlug canonicalizes via
	// EvalSymlinks) so we hit the genuine over-length slug guard rather than a
	// canonicalize-failure path. A 220-char component sanitizes — with the home
	// prefix — well past the 200-char ceiling.
	overlong := filepath.Join(home, strings.Repeat("d", 220))
	if err := os.MkdirAll(overlong, 0o700); err != nil {
		t.Fatalf("mkdir overlong dest: %v", err)
	}
	thread := store.Thread{
		ID:            "thread-overlength",
		Provider:      string(provider.Claude),
		WorkspacePath: overlong,
		SessionRef:    sessionID,
	}

	purge, err := app.copyClaudeSessionForWorkspaceChange(thread, fromWS)
	if err == nil {
		t.Fatal("expected hard error for unresolvable destination slug, got nil")
	}
	if purge != nil {
		t.Errorf("purge = %v, want nil on hard failure (nothing moved, nothing to purge)", purge)
	}
	if _, statErr := os.Stat(srcTranscript); statErr != nil {
		t.Errorf("source transcript must survive a refused relocation: %v", statErr)
	}
}
