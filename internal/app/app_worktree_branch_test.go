package app

import (
	"errors"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestMaybeRenameTemporaryWorktreeBranchRenamesOnFirstMessage covers the
// happy path: a thread with a temporary ao-<8-hex> branch in a real
// worktree is renamed to a descriptive target derived from the user message.
// The rename happens exactly once: a second send short-circuits at the "not
// temporary" gate, never calls the generator, and keeps the name.
func TestMaybeRenameTemporaryWorktreeBranchRenamesOnFirstMessage(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)

	thread := testThread("thread-worktree-rename")
	thread.Provider = string(provider.Claude)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := worktreePathOf(app.PrepareThreadWorktree(thread.ID, "", "", false))
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}

	var seenMessage string
	callCount := 0
	app.generateBranchNameFn = func(th store.Thread, message string) (string, error) {
		seenMessage = message
		callCount++
		if th.ID != thread.ID {
			t.Fatalf("generate called with thread %q, want %q", th.ID, thread.ID)
		}
		return "describe-the-work", nil
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "Describe the work")

	if seenMessage != "Describe the work" {
		t.Fatalf("generateBranchNameFn saw message %q", seenMessage)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-describe-the-work" {
		t.Fatalf("stored Branch = %q, want ao-describe-the-work", stored.Branch)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("stored UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}

	// The actual worktree head should agree.
	status, _, err := app.gitCore().Execute(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse error = %v", err)
	}
	if strings.TrimSpace(status) != "ao-describe-the-work" {
		t.Fatalf("worktree HEAD branch = %q, want ao-describe-the-work", strings.TrimSpace(status))
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "second message")
	if callCount != 1 {
		t.Fatalf("after second call: callCount = %d, want 1 (no re-rename)", callCount)
	}
	stored, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-describe-the-work" {
		t.Fatalf("branch after second call = %q, want preserved", stored.Branch)
	}
}

// The rename writes the branch and nothing else: a move that lands while the
// branch name is generated, after the row was read, survives it.
func TestMaybeRenameTemporaryWorktreeBranchKeepsAMoveThatLandedAfterItsRead(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	thread := testThread("thread-worktree-rename-move")
	thread.Provider = string(provider.Claude)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	worktreePath, err := worktreePathOf(app.PrepareThreadWorktree(thread.ID, "", "", false))
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}
	elsewhere := worktreePath + "-elsewhere"
	app.generateBranchNameFn = func(store.Thread, string) (string, error) {
		return "describe-the-work", app.store.UpdateWorkspacePath(thread.ID, elsewhere)
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "Describe the work")

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.WorkspacePath != elsewhere || stored.Branch != "ao-describe-the-work" {
		t.Fatalf("row = workspace %q branch %q, want %q and ao-describe-the-work", stored.WorkspacePath, stored.Branch, elsewhere)
	}
}

// TestMaybeRenameTemporaryWorktreeBranchSkipsANonCandidate verifies the
// no-op paths: a thread with no WorktreePath, and a branch that is already
// descriptive (only the ao-<8-hex> placeholder is a rename candidate), are
// never renamed and never call generateBranchNameFn.
func TestMaybeRenameTemporaryWorktreeBranchSkipsANonCandidate(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ branch, worktreePath string }{
		"no worktree":        {gitops.BuildTemporaryWorktreeBranchNameWithPrefix(gitops.AutoWorktreeBranchPrefix), ""},
		"descriptive branch": {"feature/already-named", "/tmp/unused-worktree"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			thread := testThread("thread-rename-skip")
			thread.Provider = string(provider.Claude)
			thread.Branch = test.branch
			thread.WorktreePath = test.worktreePath
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread() error = %v", err)
			}
			called := false
			app.generateBranchNameFn = func(store.Thread, string) (string, error) {
				called = true
				return "should-not-happen", nil
			}

			app.maybeRenameTemporaryWorktreeBranch(thread.ID, "fix the bug")

			if called {
				t.Fatal("generateBranchNameFn was called")
			}
			stored, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatalf("GetThread() error = %v", err)
			}
			if stored.Branch != test.branch {
				t.Fatalf("stored Branch = %q, want preserved %q", stored.Branch, test.branch)
			}
		})
	}
}

// TestMaybeRenameTemporaryWorktreeBranchGeneratorErrorIsNonFatal exercises
// the logged-and-moved-on behavior: if the generator returns an error, the
// worktree branch is unchanged but no error surfaces (the function has no
// return value).
func TestMaybeRenameTemporaryWorktreeBranchGeneratorErrorIsNonFatal(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)

	thread := testThread("thread-generator-error")
	thread.Provider = string(provider.Claude)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.PrepareThreadWorktree(thread.ID, "", "", false); err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	originalBranch := stored.Branch
	if !gitops.IsTemporaryWorktreeBranchWithPrefix(originalBranch, gitops.AutoWorktreeBranchPrefix) {
		t.Fatalf("seed branch %q is not temporary", originalBranch)
	}

	app.generateBranchNameFn = func(store.Thread, string) (string, error) {
		return "", errors.New("generator boom")
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "fix it")

	stored, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != originalBranch {
		t.Fatalf("branch = %q, want unchanged %q", stored.Branch, originalBranch)
	}
}

// TestMaybeRenameTemporaryWorktreeBranchFallsBackOnEmptyMessage confirms the
// branchNameFromUserMessage fallback ("update") kicks in when the user
// message has no usable words and gets wrapped to "ao-update".
func TestMaybeRenameTemporaryWorktreeBranchFallsBackOnEmptyMessage(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)

	thread := testThread("thread-fallback-branch")
	thread.Provider = string(provider.Claude)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.PrepareThreadWorktree(thread.ID, "", "", false); err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}

	// Leaving generateBranchNameFn nil exercises the
	// branchNameFromUserMessage → BuildGeneratedWorktreeBranchNameWithPrefix path.
	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "   \t\n")

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-update" {
		t.Fatalf("stored Branch = %q, want ao-update", stored.Branch)
	}
}

// Unit-level coverage for the helpers used to derive branch names from
// user messages now lives in internal/git/branch_names_test.go alongside
// the lifted BranchFragmentFromUserMessage / firstSentenceFromMessage.
// This file retains the integration tests (worktree creation, rename,
// idempotency) that exercise the full saga.
