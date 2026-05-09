package main

import (
	"os"
	"path/filepath"
	"strings"
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

	_, err = app.PrepareThreadWorktree(thread.ID, "main", "feature/blocked")
	if err == nil || !strings.Contains(err.Error(), "cannot switch workspace while turn 0 is active") {
		t.Fatalf("PrepareThreadWorktree() error = %v, want active-turn rejection", err)
	}
}

func TestGitRemoveWorktreeRejectsOtherThreadWorkspaceReference(t *testing.T) {
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
	other.Branch = "feature/shared"
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other) error = %v", err)
	}

	err = app.GitRemoveWorktree(owner.ID)
	if err == nil || !strings.Contains(err.Error(), "used by another thread") {
		t.Fatalf("GitRemoveWorktree() error = %v, want shared worktree rejection", err)
	}
}

func TestGitRemoveWorktreeRejectsArchivedThreadWorkspaceReference(t *testing.T) {
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
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	other := testThread("thread-worktree-archived-user")
	other.ProjectID = project.ID
	other.WorkspacePath = worktreePath
	other.WorktreePath = worktreePath
	other.Branch = "feature/archived-shared"
	other.Archived = true
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other) error = %v", err)
	}

	err = app.GitRemoveWorktree(owner.ID)
	if err == nil || !strings.Contains(err.Error(), "used by another thread") {
		t.Fatalf("GitRemoveWorktree() error = %v, want archived shared worktree rejection", err)
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

	updated, err := app.PrepareThreadWorktree(thread.ID, "release", "feature/base")
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
