package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
)

func TestGetGitStatusUsesWorkspacePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := initAppGitRepo(t)

	thread := testThread("thread-status")
	thread.ProjectPath = t.TempDir()
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
	repo := initAppGitRepo(t)

	thread := testThread("thread-branches")
	thread.ProjectPath = repo
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
	repo := initAppGitRepo(t)

	thread := testThread("thread-commit")
	thread.ProjectPath = repo
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
	repo := initAppGitRepo(t)

	thread := testThread("thread-worktree")
	thread.ProjectPath = repo
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

func TestGitCheckoutUpdatesStoredBranch(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := initAppGitRepo(t)

	runGitCommand(t, repo, "branch", "feature/checkout")

	thread := testThread("thread-checkout")
	thread.ProjectPath = repo
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
	return canonicalAppPath(left) == canonicalAppPath(right)
}

func canonicalAppPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func initAppGitRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	runGitCommand(t, repo, "init", "-b", "main")
	runGitCommand(t, repo, "config", "user.name", "Agent Overflow")
	runGitCommand(t, repo, "config", "user.email", "agent-overflow@example.com")

	readme := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readme, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runGitCommand(t, repo, "add", "README.txt")
	runGitCommand(t, repo, "commit", "-m", "initial commit")

	return repo
}

func runGitCommand(t *testing.T, cwd string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
}
