package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

// Bug C3 regression: GetGitStatus must surface in-progress multi-step
// operations (merge/rebase/bisect) so the Ship Changes wizard can disable
// commit and tell the user why.
func TestPendingOperationClean(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "" {
		t.Fatalf("PendingOperation = %q, want empty for idle repo", status.PendingOperation)
	}
}

func TestPendingOperationMerge(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Create two diverging branches that touch the same line to force a
	// conflict when we merge them back together.
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("feature side\n"), 0o644); err != nil {
		t.Fatalf("write feature README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "feature change")

	testutil.RunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("main side\n"), 0o644); err != nil {
		t.Fatalf("write main README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "main change")

	// Merge attempts to combine both sides; the conflict leaves MERGE_HEAD
	// in place until the user resolves or aborts. Expect a non-zero exit
	// code but don't fail the test - we specifically want the mid-merge
	// state.
	_ = testutil.RunGitAllowError(repo, "merge", "--no-commit", "--no-ff", "feature")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "merge" {
		t.Fatalf("PendingOperation = %q, want 'merge'", status.PendingOperation)
	}
}

func TestPendingOperationRebase(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Set up a conflicting commit on main, then start a rebase of a
	// divergent branch onto main to trigger an unresolved rebase state.
	testutil.RunGit(t, repo, "checkout", "-b", "topic")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("topic side\n"), 0o644); err != nil {
		t.Fatalf("write topic README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "topic change")

	testutil.RunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("main side\n"), 0o644); err != nil {
		t.Fatalf("write main README: %v", err)
	}
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "main change")

	testutil.RunGit(t, repo, "checkout", "topic")
	// Expect the rebase to stop on the conflict - that's what we want.
	_ = testutil.RunGitAllowError(repo, "rebase", "main")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "rebase" {
		t.Fatalf("PendingOperation = %q, want 'rebase'", status.PendingOperation)
	}
}

func TestPendingOperationBisect(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	// Create a short history so there's something to bisect.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("file-%d.txt", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		testutil.RunGit(t, repo, "add", name)
		testutil.RunGit(t, repo, "commit", "-m", fmt.Sprintf("add %s", name))
	}

	testutil.RunGit(t, repo, "bisect", "start")
	testutil.RunGit(t, repo, "bisect", "bad")
	// Provide a known-good commit to activate the bisect session.
	testutil.RunGit(t, repo, "bisect", "good", "HEAD~2")

	status, err := NewCore().Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.PendingOperation != "bisect" {
		t.Fatalf("PendingOperation = %q, want 'bisect'", status.PendingOperation)
	}
}

func TestResolveGitDirMemoizesSuccess(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	first := core.resolveGitDir(repo)
	if first == "" {
		t.Fatal("expected a git dir for a real repo")
	}

	// Break the repo on disk: a fresh rev-parse would now fail, so getting
	// the same answer back proves the memo was hit.
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, ".git-moved")); err != nil {
		t.Fatalf("rename .git: %v", err)
	}
	if second := core.resolveGitDir(repo); second != first {
		t.Fatalf("memoized resolveGitDir = %q, want %q", second, first)
	}
}

func TestResolveGitDirDoesNotCacheFailure(t *testing.T) {
	dir := t.TempDir()
	core := NewCore()

	if got := core.resolveGitDir(dir); got != "" {
		t.Fatalf("resolveGitDir on non-repo = %q, want empty", got)
	}

	// The failure must not stick: after init, the same Core resolves.
	testutil.RunGit(t, dir, "init", "-b", "main")
	if got := core.resolveGitDir(dir); got == "" {
		t.Fatal("expected resolveGitDir to succeed after git init on the same Core")
	}
}

func TestPendingOperationNonRepoReturnsEmpty(t *testing.T) {
	// A non-repo directory must yield an empty pendingOperation - never a
	// false positive, since Status already reports IsRepo=false.
	dir := t.TempDir()
	status, err := NewCore().Status(dir)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.IsRepo {
		t.Fatal("expected IsRepo=false for non-repo dir")
	}
	if status.PendingOperation != "" {
		t.Fatalf("PendingOperation = %q, want empty for non-repo dir", status.PendingOperation)
	}
}
