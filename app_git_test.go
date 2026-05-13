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

// GitCheckout straddles app_git.go and app_worktree.go: when the user
// checks out the default branch from a worktree, the resolver snaps
// the thread back to the project root. Lives here because GitCheckout
// itself is in app_git.go; samePath / worktree fixtures are in
// app_worktree_test.go via shared package scope.
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

	if got := app.gitCore().CurrentBranch(repo); got != "main" {
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
