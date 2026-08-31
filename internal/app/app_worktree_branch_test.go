package app

import (
	"errors"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// TestMaybeRenameTemporaryWorktreeBranchRenamesOnFirstMessage covers the
// happy path: a thread with a temporary ao-<8-hex> branch in a real
// worktree is renamed to a descriptive target derived from the user message.
func TestMaybeRenameTemporaryWorktreeBranchRenamesOnFirstMessage(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

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

	worktreePath, err := app.GitCreateWorktree(thread.ID, "")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	var seenMessage string
	app.generateBranchNameFn = func(th store.Thread, message string) (string, error) {
		seenMessage = message
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
}

// TestMaybeRenameTemporaryWorktreeBranchSkipsNonWorktreeThread verifies the
// no-op path: when the thread has no WorktreePath set, we never try to
// rename (and never call generateBranchNameFn).
func TestMaybeRenameTemporaryWorktreeBranchSkipsNonWorktreeThread(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-no-worktree")
	thread.Branch = gitops.BuildTemporaryWorktreeBranchName()
	// WorktreePath intentionally empty; this is not a worktree thread.
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
		t.Fatal("generateBranchNameFn was called for a non-worktree thread")
	}
}

// TestMaybeRenameTemporaryWorktreeBranchSkipsNonTemporaryBranch ensures we
// don't overwrite an already-descriptive branch name. Only the ao-<8-hex>
// placeholder is a rename candidate.
func TestMaybeRenameTemporaryWorktreeBranchSkipsNonTemporaryBranch(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-descriptive-branch")
	thread.Provider = string(provider.Claude)
	thread.Branch = "feature/already-named"
	thread.WorktreePath = "/tmp/unused-worktree"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	called := false
	app.generateBranchNameFn = func(store.Thread, string) (string, error) {
		called = true
		return "different-name", nil
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "fix the bug")

	if called {
		t.Fatal("generateBranchNameFn was called for a non-temporary branch")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "feature/already-named" {
		t.Fatalf("stored Branch = %q, want preserved", stored.Branch)
	}
}

// TestMaybeRenameTemporaryWorktreeBranchGeneratorErrorIsNonFatal exercises
// the logged-and-moved-on behavior: if the generator returns an error, the
// worktree branch is unchanged but no error surfaces (the function has no
// return value).
func TestMaybeRenameTemporaryWorktreeBranchGeneratorErrorIsNonFatal(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

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

	if _, err := app.GitCreateWorktree(thread.ID, ""); err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	originalBranch := stored.Branch
	if !gitops.IsTemporaryWorktreeBranch(originalBranch) {
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
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

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

	if _, err := app.GitCreateWorktree(thread.ID, ""); err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	// Leaving generateBranchNameFn nil exercises the
	// branchNameFromUserMessage → BuildGeneratedWorktreeBranchName path.
	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "   \t\n")

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-update" {
		t.Fatalf("stored Branch = %q, want ao-update", stored.Branch)
	}
}

// TestMaybeRenameTemporaryWorktreeBranchIsIdempotentWhenSkipped verifies the
// "happens exactly once" property: once the branch has been renamed to a
// descriptive name, a second send does not attempt another rename. The
// generator fn is never called, and the branch keeps its descriptive name.
func TestMaybeRenameTemporaryWorktreeBranchIsIdempotentWhenSkipped(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-rename-idempotent")
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
	if _, err := app.GitCreateWorktree(thread.ID, ""); err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
	}

	callCount := 0
	app.generateBranchNameFn = func(store.Thread, string) (string, error) {
		callCount++
		return "first-rename", nil
	}

	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "first message")
	if callCount != 1 {
		t.Fatalf("after first call: callCount = %d, want 1", callCount)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-first-rename" {
		t.Fatalf("branch after first rename = %q, want ao-first-rename", stored.Branch)
	}

	// Second call should short-circuit inside the "not temporary" gate.
	app.maybeRenameTemporaryWorktreeBranch(thread.ID, "second message")
	if callCount != 1 {
		t.Fatalf("after second call: callCount = %d, want 1 (no re-rename)", callCount)
	}

	stored, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-first-rename" {
		t.Fatalf("branch after second call = %q, want preserved", stored.Branch)
	}
}

// Unit-level coverage for the helpers used to derive branch names from
// user messages now lives in internal/git/branch_names_test.go alongside
// the lifted BranchFragmentFromUserMessage / firstSentenceFromMessage.
// This file retains the integration tests (worktree creation, rename,
// idempotency) that exercise the full saga.
