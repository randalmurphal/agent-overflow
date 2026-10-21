package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// failCheckoutHook makes every checkout in repo fail after git has
// written the worktree: a post-checkout hook that exits non-zero, which
// git reports as the add's failure while leaving the worktree registered.
func failCheckoutHook(t *testing.T, repo string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is unix-only")
	}
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\necho injected checkout failure >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "config", "core.hooksPath", hooks)
}

// occupiedPath is a non-empty directory git refuses to add a worktree at.
func occupiedPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertAddUndone fails unless repo's main checkout is its only worktree
// and branch exists exactly when wantBranch.
func assertAddUndone(t *testing.T, repo, branch string, wantBranch bool) {
	t.Helper()
	core := NewCore()
	worktrees, err := core.ListWorktrees(repo)
	if err != nil || len(worktrees) != 1 {
		t.Fatalf("worktrees = %+v (%v), want the main checkout alone", worktrees, err)
	}
	exists, err := core.branchExistsChecked(repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	if exists != wantBranch {
		t.Fatalf("branch %s exists = %v, want %v", branch, exists, wantBranch)
	}
}

func assertOccupiedPathKept(t *testing.T, path string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(path, "keep.txt")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the occupied destination changed: %q, %v", data, err)
	}
}

// A branch only origin has is made locally for the worktree; a failed
// checkout deletes it and leaves no worktree, and an occupied destination
// is left as it was.
func TestAttachWorktreeOfAnOriginBranchUndoesAFailedCheckout(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo, bare := testutil.InitGitRepoWithOrigin(t)
		originOnlyBranch(t, bare, "feature/mr")
		testutil.RunGit(t, repo, "fetch", "-q", "origin")
		path := filepath.Join(t.TempDir(), "mr")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		created, err := NewCore().AttachWorktree(repo, path, "feature/mr")
		if err == nil || created {
			t.Fatalf("%s: AttachWorktree = created %v, %v; want a failure that made nothing", failure, created, err)
		}
		if strings.Contains(err.Error(), "undo") {
			t.Fatalf("%s: the undo failed: %v", failure, err)
		}
		assertAddUndone(t, repo, "feature/mr", false)
		if failure == "occupied" {
			assertOccupiedPathKept(t, path)
		}
	}
}

// A local branch the attach did not make survives its failed checkout.
func TestAttachWorktreeOfALocalBranchKeepsItWhenTheCheckoutFails(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "branch", "feature/local")
		path := filepath.Join(t.TempDir(), "local")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		created, err := NewCore().AttachWorktree(repo, path, "feature/local")
		if err == nil || created {
			t.Fatalf("%s: AttachWorktree = created %v, %v; want a failure", failure, created, err)
		}
		assertAddUndone(t, repo, "feature/local", true)
	}
}

// An attach git refuses because the branch is already checked out at the
// path leaves that worktree registered: it predates the call.
func TestAttachWorktreeKeepsAWorktreeThatWasAlreadyThere(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	core := NewCore()
	testutil.RunGit(t, repo, "branch", "feature/there")
	path := filepath.Join(t.TempDir(), "there")
	if _, err := core.AttachWorktree(repo, path, "feature/there"); err != nil {
		t.Fatal(err)
	}

	if _, err := core.AttachWorktree(repo, path, "feature/there"); err == nil {
		t.Fatal("a second attach at the same path succeeded")
	}
	registered, err := core.worktreeRegisteredAt(repo, path)
	if err != nil || !registered {
		t.Fatalf("the existing worktree was removed (%v)", err)
	}
}

// A new branch cut for a worktree is deleted when its checkout fails, and
// the failure is the checkout's, not a duplicate branch.
func TestCreateWorktreeUndoesTheBranchOfAFailedCheckout(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo, _ := testutil.InitGitRepoWithOrigin(t)
		path := filepath.Join(t.TempDir(), "cut")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		err := NewCore().CreateWorktreeFromBranch(repo, path, "main", "ao-cut")
		if err == nil {
			t.Fatalf("%s: CreateWorktreeFromBranch succeeded", failure)
		}
		if strings.Contains(err.Error(), `branch "ao-cut" already exists`) || strings.Contains(err.Error(), "undo") {
			t.Fatalf("%s: CreateWorktreeFromBranch = %v, want the checkout's failure and a clean undo", failure, err)
		}
		assertAddUndone(t, repo, "ao-cut", false)
		if failure == "occupied" {
			assertOccupiedPathKept(t, path)
		}

		// The fresh-base cut from origin takes the same path.
		_, err = NewCore().CreateWorktreeFromFreshBase(t.Context(), repo, path, "main", "ao-fresh")
		if err == nil {
			t.Fatalf("%s: CreateWorktreeFromFreshBase succeeded", failure)
		}
		assertAddUndone(t, repo, "ao-fresh", false)
	}
}

// A cut whose branch already exists fails before it makes anything and
// leaves that branch alone.
func TestCreateWorktreeKeepsAnExistingBranch(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "ao-taken")
	err := NewCore().CreateWorktreeFromBranch(repo, filepath.Join(t.TempDir(), "wt"), "", "ao-taken")
	if err == nil || !strings.Contains(err.Error(), `branch "ao-taken" already exists`) {
		t.Fatalf("CreateWorktreeFromBranch = %v, want the duplicate branch refusal", err)
	}
	assertAddUndone(t, repo, "ao-taken", true)
}
