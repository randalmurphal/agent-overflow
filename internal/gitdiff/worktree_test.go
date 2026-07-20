package gitdiff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestIsGitRepository(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	if !IsGitRepository(context.Background(), repo) {
		t.Fatal("expected a git repo to be detected")
	}
	if IsGitRepository(context.Background(), t.TempDir()) {
		t.Fatal("expected a plain directory to not be a repo")
	}
}

func TestDiffWorkspaceVsHeadCombinesTrackedAndUntracked(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "README.txt", "hello\nedited\n")
	writeFile(t, repo, "new.txt", "brand new\n")

	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	text := string(patch)
	if !strings.Contains(text, "+edited") {
		t.Fatalf("expected the tracked edit, got:\n%s", text)
	}
	if !strings.Contains(text, "+brand new") {
		t.Fatalf("expected the untracked file, got:\n%s", text)
	}
}

func TestDiffWorkspaceVsHeadCleanTreeIsEmpty(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected an empty patch for a clean tree, got:\n%s", patch)
	}
}

func TestDiffWorkspaceVsHeadFreshInitRepo(t *testing.T) {
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-b", "main")
	writeFile(t, repo, "new.txt", "no commits yet\n")

	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	if !strings.Contains(string(patch), "+no commits yet") {
		t.Fatalf("expected the untracked file even without a HEAD, got:\n%s", patch)
	}
}

func TestDiffWorkspaceVsHeadShowsDeletions(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "doomed.txt", "was here\n", "add doomed")
	if err := os.Remove(filepath.Join(repo, "doomed.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	if !strings.Contains(string(patch), "-was here") {
		t.Fatalf("expected the deletion in the diff, got:\n%s", patch)
	}
}

func TestDiffWorkspaceVsHeadStagesSymlinks(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	if err := os.Symlink("README.txt", filepath.Join(repo, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	text := string(patch)
	if !strings.Contains(text, "120000") || !strings.Contains(text, "+README.txt") {
		t.Fatalf("expected the symlink as a new 120000 entry with its target text, got:\n%s", text)
	}
}

func TestDiffWorkspaceVsHeadRespectsGitignore(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, ".gitignore", "ignored.txt\n", "add gitignore")
	writeFile(t, repo, "ignored.txt", "should not appear\n")

	patch, err := DiffWorkspaceVsHead(context.Background(), repo)
	if err != nil {
		t.Fatalf("DiffWorkspaceVsHead: %v", err)
	}
	if strings.Contains(string(patch), "should not appear") {
		t.Fatalf("ignored file leaked into the diff:\n%s", patch)
	}
}

func TestDiffBranchBaseToWorktreeSpansCommittedStagedAndUntracked(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "committed.txt", "committed change\n", "feature commit")
	writeFile(t, repo, "staged.txt", "staged change\n")
	testutil.RunGit(t, repo, "add", "staged.txt")
	writeFile(t, repo, "untracked.txt", "untracked change\n")

	patch, err := DiffBranchBaseToWorktree(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("DiffBranchBaseToWorktree: %v", err)
	}
	text := string(patch)
	for _, want := range []string{"committed change", "staged change", "untracked change"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in the branch-base patch, got:\n%s", want, text)
		}
	}
}

func TestDiffBranchBaseToWorktreeLeavesUserIndexUntouched(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	writeFile(t, repo, "untracked.txt", "untracked change\n")

	if _, err := DiffBranchBaseToWorktree(context.Background(), repo, "main"); err != nil {
		t.Fatalf("DiffBranchBaseToWorktree: %v", err)
	}

	staged, _, _, err := runGit(context.Background(), repo, nil, false, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("diff --cached: %v", err)
	}
	if strings.TrimSpace(staged) != "" {
		t.Fatalf("snapshot staged into the user's index: %q", staged)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "objects", "pack")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat objects: %v", err)
	}
}

func TestDiffBranchBaseToWorktreeSkipsCleanFilters(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	// A repo-defined clean filter that would fail loudly if executed. The
	// snapshot must hash with --no-filters, so the diff still succeeds and
	// carries the on-disk bytes.
	commitFile(t, repo, ".gitattributes", "*.dat filter=boom\n", "add attributes")
	testutil.RunGit(t, repo, "config", "filter.boom.clean", "false")
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	writeFile(t, repo, "payload.dat", "raw bytes\n")

	patch, err := DiffBranchBaseToWorktree(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("DiffBranchBaseToWorktree: %v", err)
	}
	if !strings.Contains(string(patch), "raw bytes") {
		t.Fatalf("expected the unfiltered file content, got:\n%s", patch)
	}
}

func TestDiffBranchBaseToWorktreeRequiresBase(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	if _, err := DiffBranchBaseToWorktree(context.Background(), repo, "  "); err == nil {
		t.Fatal("expected error for an empty base branch")
	}
}

func TestDiffBranchBaseToWorktreeResolvesRemoteOnlyBaseBranch(t *testing.T) {
	clone := cloneWithRemoteOnlyBranch(t)
	writeFile(t, clone, "untracked.txt", "uncommitted too\n")

	patch, err := DiffBranchBaseToWorktree(context.Background(), clone, "release")
	if err != nil {
		t.Fatalf("DiffBranchBaseToWorktree with remote-only base: %v", err)
	}
	text := string(patch)
	if !strings.Contains(text, "local work") || !strings.Contains(text, "uncommitted too") {
		t.Fatalf("expected committed + uncommitted changes vs origin/release, got:\n%s", text)
	}
}
