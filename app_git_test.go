package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func TestGetGitStatusUsesWorkspacePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-status")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	status, err := app.GetGitStatus(thread.ID)
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected workspace git status to report a repo")
	}
	if status.Branch != "main" {
		t.Fatalf("Branch = %q, want main", status.Branch)
	}
}

func TestGitListBranchesUsesProjectPath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	// Anchor the thread to a project at the git repo — that's the
	// implicit project.Path for GitListBranches to resolve.
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-branches")
	thread.ProjectID = project.ID
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := app.GitCreateBranch(thread.ID, "feature/demo"); err != nil {
		t.Fatalf("GitCreateBranch() error = %v", err)
	}

	branches, err := app.GitListBranches(thread.ID)
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}

	if !containsBranch(branches, "feature/demo") {
		t.Fatalf("expected feature/demo in branches: %+v", branches)
	}
}

func TestGitCommitReturnsCommitSHA(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-commit")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	readme := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readme, []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := app.GitCommit(thread.ID, "update readme", "body")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}
	if result.Action != "commit" {
		t.Fatalf("Action = %q, want commit", result.Action)
	}
	if len(result.Commit) != 40 {
		t.Fatalf("Commit length = %d, want 40", len(result.Commit))
	}
	if result.Branch != "main" {
		t.Fatalf("Branch = %q, want main", result.Branch)
	}
}

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
	if _, err := os.Stat(filepath.Join(updated.WorktreePath, "BASE.txt")); err != nil {
		t.Fatalf("expected worktree to branch from release: %v", err)
	}
}

func TestGitCheckoutUpdatesStoredBranch(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "feature/checkout")

	thread := testThread("thread-checkout")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := app.GitCheckout(thread.ID, "feature/checkout"); err != nil {
		t.Fatalf("GitCheckout() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "feature/checkout" {
		t.Fatalf("stored Branch = %q, want feature/checkout", stored.Branch)
	}
}

func TestGitCheckoutRejectsActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "feature/checkout")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-checkout-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-checkout-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	err = app.GitCheckout(thread.ID, "feature/checkout")
	if err == nil || !strings.Contains(err.Error(), "cannot switch workspace while turn 0 is active") {
		t.Fatalf("GitCheckout() error = %v, want active-turn rejection", err)
	}
}

func TestGitCheckoutDefaultBranchFromWorktreeReturnsToProjectRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-feature")

	testutil.RunGit(t, repo, "branch", "dev")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/worktree", worktreePath, "main")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})
	testutil.RunGit(t, repo, "checkout", "dev")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-checkout-default-from-worktree")
	thread.ProjectID = project.ID
	thread.WorkspacePath = worktreePath
	thread.WorktreePath = worktreePath
	thread.Branch = "feature/worktree"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := app.GitCheckout(thread.ID, "main"); err != nil {
		t.Fatalf("GitCheckout(main) error = %v", err)
	}

	if got := currentGitBranch(app.gitCore(), repo); got != "main" {
		t.Fatalf("project root branch = %q, want main", got)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if !samePath(stored.WorkspacePath, repo) {
		t.Fatalf("WorkspacePath = %q, want %q", stored.WorkspacePath, repo)
	}
	if stored.WorktreePath != "" {
		t.Fatalf("WorktreePath = %q, want empty", stored.WorktreePath)
	}
	if stored.Branch != "main" {
		t.Fatalf("Branch = %q, want main", stored.Branch)
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

func TestGitCreateBranchFromCurrentBaseKeepsDirtyTree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-current")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	updated, err := app.GitCreateBranchFrom(thread.ID, "feature/keep", "main", true)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom() error = %v", err)
	}
	if updated.Branch != "feature/keep" {
		t.Fatalf("Branch = %q, want feature/keep", updated.Branch)
	}
	contents, err := os.ReadFile(filepath.Join(repo, "README.txt"))
	if err != nil {
		t.Fatalf("read repo file: %v", err)
	}
	if !strings.Contains(string(contents), "dirty edit") {
		t.Fatalf("dirty edit was lost; got %q", string(contents))
	}
}

func TestGitCreateBranchFromOtherBaseDiscardsDirtyTree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(repo, "RELEASE.txt"), []byte("release marker\n"), 0o644); err != nil {
		t.Fatalf("release write: %v", err)
	}
	testutil.RunGit(t, repo, "add", "RELEASE.txt")
	testutil.RunGit(t, repo, "commit", "-m", "release marker")
	testutil.RunGit(t, repo, "checkout", "main")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-discard")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	updated, err := app.GitCreateBranchFrom(thread.ID, "feature/discard", "release", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom() error = %v", err)
	}
	if updated.Branch != "feature/discard" {
		t.Fatalf("Branch = %q, want feature/discard", updated.Branch)
	}

	// README should be back to the release-base content. RELEASE.txt
	// should exist (it comes from the release branch).
	contents, err := os.ReadFile(filepath.Join(repo, "README.txt"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if strings.Contains(string(contents), "dirty edit") {
		t.Fatalf("expected dirty edit discarded; still present: %q", string(contents))
	}
	if _, err := os.Stat(filepath.Join(repo, "RELEASE.txt")); err != nil {
		t.Fatalf("expected base content RELEASE.txt: %v", err)
	}

	// The stash stack should be empty — the destructive path drops the
	// stash after a clean checkout.
	stdout, _, err := app.gitCore().Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stash; got %q", stdout)
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
		if t, ok := data.(store.Thread); ok {
			broadcast = append(broadcast, t.ID)
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

func TestGitCreateBranchFromCarryWithOtherBaseRejected(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "release")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-mismatch")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.GitCreateBranchFrom(thread.ID, "feature/mismatch", "release", true); err == nil {
		t.Fatal("expected error when carryLocalChanges=true but base != current branch")
	}
}

func containsBranch(branches []gitops.GitBranch, want string) bool {
	for _, branch := range branches {
		if branch.Name == want {
			return true
		}
	}
	return false
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
